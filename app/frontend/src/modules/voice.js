"use strict";

import { Events, svc, flog, state, dom, formatMmSs, cssEscape } from "./core.js";
import { playVoiceLeaveSound } from "./sounds.js";
import { stopScreenShareIfActive } from "./screenshare.js";
import { getLocale } from "./i18n.js";

// --- Voice Playback ---
var currentAudio = null;
var currentPlayingId = null;

// --- Скорость голосовых (фаза 6d, макет MessagesFiles): 1× → 1,5× → 2× ---
// Одна на все голосовые, помнится между запусками. Чип .voice-speed виден
// только у играющего пузыря (.voice-bubble.is-playing).
var RATES = [1, 1.5, 2];
var RATE_KEY = "vox.voice_rate";
var playbackRate = 1;
try {
    var savedRate = parseFloat(localStorage.getItem(RATE_KEY) || "");
    if (RATES.indexOf(savedRate) >= 0) playbackRate = savedRate;
} catch (e) { /* storage недоступен */ }

export function voiceRateLabel() {
    var s = String(playbackRate);
    if (getLocale() === "ru") s = s.replace(".", ",");
    return s + "×";
}

export function cycleVoiceRate() {
    playbackRate = RATES[(RATES.indexOf(playbackRate) + 1) % RATES.length];
    try { localStorage.setItem(RATE_KEY, String(playbackRate)); } catch (e) { /* storage недоступен */ }
    if (currentAudio) currentAudio.playbackRate = playbackRate;
    var label = voiceRateLabel();
    document.querySelectorAll(".voice-speed").forEach(function (b) { b.textContent = label; });
    return label;
}

function setPlayingClass(msgId, on) {
    var vb = document.querySelector('.voice-bubble[data-msg-id="' + cssEscape(msgId) + '"]');
    if (vb) vb.classList.toggle("is-playing", on);
}
export var playIconSvg = '<svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor"><polygon points="5 3 19 12 5 21 5 3"/></svg>';
export var pauseIconSvg = '<svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor"><rect x="6" y="4" width="4" height="16"/><rect x="14" y="4" width="4" height="16"/></svg>';
var downloadIconSvg = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>';
var spinnerSvg = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" class="voice-spinner"><circle cx="12" cy="12" r="10" stroke-dasharray="31" stroke-dashoffset="10"/></svg>';
var cancelSvg = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>';

// Audio cache: msgId → base64 string (LRU, max 50 entries)
var audioCache = {};
var audioCacheKeys = []; // ordered by access time
var AUDIO_CACHE_MAX = 50;

function audioCacheSet(msgId, data) {
    // Удаляем из текущей позиции если есть
    var idx = audioCacheKeys.indexOf(msgId);
    if (idx !== -1) audioCacheKeys.splice(idx, 1);
    audioCacheKeys.push(msgId);
    audioCache[msgId] = data;
    // Evict oldest
    while (audioCacheKeys.length > AUDIO_CACHE_MAX) {
        var old = audioCacheKeys.shift();
        delete audioCache[old];
    }
}

// Active download: msgId → { cancelled: bool }
var activeDownloads = {};

// Threshold: auto-play for messages shorter than this (ms)
var AUTO_LOAD_THRESHOLD = 5 * 60 * 1000; // 5 minutes

function resetVoiceUI(msgId) {
    var btn = document.querySelector('.voice-play-btn[data-msg-id="' + cssEscape(msgId) + '"]');
    if (btn) btn.innerHTML = playIconSvg;
    setPlayingClass(msgId, false);
    updateWaveProgress(msgId, 0);
    var info = document.getElementById("vinfo-" + msgId);
    if (info) {
        var bubble = document.querySelector('.voice-bubble[data-msg-id="' + cssEscape(msgId) + '"]');
        var durMs = bubble ? parseInt(bubble.dataset.durMs || "0") : 0;
        info.textContent = formatMmSs(0) + " / " + formatMmSs(durMs);
    }
}

function updateWaveProgress(msgId, progress) {
    var wave = document.getElementById("wave-" + msgId);
    if (!wave) return;
    var bars = wave.children;
    var filledCount = Math.floor(progress * bars.length);
    for (var i = 0; i < bars.length; i++) {
        bars[i].classList.toggle("wave-bar-played", i < filledCount);
    }
}

// Estimate WAV size from duration: 16kHz * 2 bytes * seconds + 44 header
function estimateSize(durationMs) {
    var bytes = Math.round(durationMs / 1000 * 16000 * 2) + 44;
    if (bytes > 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + " MB";
    return Math.round(bytes / 1024) + " KB";
}

// Check if a voice message needs manual download (>5 min and not cached)
export function needsDownload(durationMs) {
    return durationMs > AUTO_LOAD_THRESHOLD;
}

// --- Download with cancel support ---

async function downloadVoiceData(msgId, btn) {
    // Already cached
    if (audioCache[msgId]) return audioCache[msgId];

    // Cancel any previous download for this message
    if (activeDownloads[msgId]) {
        activeDownloads[msgId].cancelled = true;
    }

    var download = { cancelled: false };
    activeDownloads[msgId] = download;

    // Show spinner + cancel button
    btn.innerHTML = spinnerSvg;
    btn.classList.add("voice-loading");

    try {
        var b64 = await svc("GetVoiceData", msgId);
        if (download.cancelled) return null;
        if (!b64) return null;

        // Cache it
        audioCacheSet(msgId, b64);
        delete activeDownloads[msgId];
        btn.classList.remove("voice-loading");
        return b64;
    } catch (err) {
        if (!download.cancelled) {
            console.error("download voice:", err);
        }
        delete activeDownloads[msgId];
        btn.classList.remove("voice-loading");
        btn.innerHTML = playIconSvg;
        return null;
    }
}

export function cancelDownload(msgId) {
    if (activeDownloads[msgId]) {
        activeDownloads[msgId].cancelled = true;
        delete activeDownloads[msgId];
    }
    var btn = document.querySelector('.voice-play-btn[data-msg-id="' + cssEscape(msgId) + '"]');
    if (btn) {
        btn.classList.remove("voice-loading");
        var bubble = document.querySelector('.voice-bubble[data-msg-id="' + cssEscape(msgId) + '"]');
        var durMs = bubble ? parseInt(bubble.dataset.durMs || "0") : 0;
        if (needsDownload(durMs)) {
            btn.innerHTML = downloadIconSvg;
        } else {
            btn.innerHTML = playIconSvg;
        }
    }
}

// --- Main play toggle ---

export async function togglePlayVoice(msgId, btn) {
    // If currently downloading — cancel
    if (activeDownloads[msgId]) {
        cancelDownload(msgId);
        return;
    }

    // If same audio is playing — pause/resume
    if (currentAudio && currentPlayingId === msgId) {
        if (currentAudio.paused) {
            currentAudio.play().catch(function () {});
            btn.innerHTML = pauseIconSvg;
        } else {
            currentAudio.pause();
            btn.innerHTML = playIconSvg;
        }
        return;
    }

    // Stop any currently playing audio
    if (currentAudio) {
        currentAudio.pause();
        resetVoiceUI(currentPlayingId);
        currentAudio = null;
        currentPlayingId = null;
    }

    // Download (or use cache)
    var b64 = await downloadVoiceData(msgId, btn);
    if (!b64) return;

    // Play
    currentAudio = new Audio("data:audio/wav;base64," + b64);
    currentAudio.playbackRate = playbackRate;
    currentPlayingId = msgId;
    btn.innerHTML = pauseIconSvg;
    setPlayingClass(msgId, true);

    currentAudio.ontimeupdate = function () {
        if (!currentAudio || !currentAudio.duration) return;
        var progress = currentAudio.currentTime / currentAudio.duration;
        updateWaveProgress(msgId, progress);
        var info = document.getElementById("vinfo-" + msgId);
        if (info) {
            info.textContent = formatMmSs(currentAudio.currentTime * 1000) + " / " + formatMmSs(currentAudio.duration * 1000);
        }
    };

    currentAudio.onended = function () {
        resetVoiceUI(msgId);
        currentAudio = null;
        currentPlayingId = null;
    };

    // .catch: WebView2 может отклонить play() (autoplay policy) — не роняем в
    // unhandledrejection, просто сбрасываем UI кнопки.
    currentAudio.play().catch(function () { resetVoiceUI(msgId); });
}

// --- Seek (перемотка) ---

export function seekVoice(msgId, progress) {
    if (!currentAudio || currentPlayingId !== msgId) return;
    if (!currentAudio.duration) return;
    currentAudio.currentTime = progress * currentAudio.duration;
    updateWaveProgress(msgId, progress);
}

// --- Download button for long messages ---

export { downloadIconSvg, estimateSize };

// --- Real-time Group Voice Chat ---

var audioCtx = null;
var peerBuffers = {}; // peerId -> { nextTime, gainNode }
var voiceActive = false;

export function initVoiceChat() {
    Events.On("vox:voice-frame", function (event) {
        if (!voiceActive || !event.data) return;
        playVoiceFrame(event.data.peerId, event.data.audio);
    });

    // Очистка аудио-ресурсов при выходе пира из голосового канала
    Events.On("vox:voice-leave", function (event) {
        if (event.data && event.data.peerId && peerBuffers[event.data.peerId]) {
            try { peerBuffers[event.data.peerId].gainNode.disconnect(); } catch (e) {}
            delete peerBuffers[event.data.peerId];
        }
    });

    // Hot-swap output device (юзер переключил наушники/колонки в системе).
    // AudioContext привязан к device на момент создания — без recreate
    // звук продолжит писаться в отключённое устройство и пир перестанет нас
    // слышать. Стратегия: на devicechange event инвалидируем ctx + peerBuffers
    // (lazy recreate в getAudioCtx при следующем фрейме). Debounce 500ms —
    // event фаерит несколько раз подряд при одной смене.
    if (navigator.mediaDevices && navigator.mediaDevices.addEventListener) {
        var deviceChangeTimer = null;
        navigator.mediaDevices.addEventListener("devicechange", function () {
            if (deviceChangeTimer) clearTimeout(deviceChangeTimer);
            deviceChangeTimer = setTimeout(invalidateAudioCtx, 500);
        });
    }
}

// invalidateAudioCtx закрывает текущий AudioContext + чистит peer jitter
// буферы. Следующий vox:voice-frame создаст свежий контекст через getAudioCtx
// и переинициализирует peerBuffers lazy.
function invalidateAudioCtx() {
    if (!voiceActive) return;
    flog("INF", "voice", "🔄 audio device changed, recreating playback context");
    for (var peerId in peerBuffers) {
        try { peerBuffers[peerId].gainNode.disconnect(); } catch (e) {}
    }
    peerBuffers = {};
    if (audioCtx) {
        try { audioCtx.close(); } catch (e) {}
        audioCtx = null;
    }
}

async function getAudioCtx() {
    // WebView2 иногда бросает при создании AudioContext (аудио-подсистема ещё
    // не готова) — retry с backoff вместо немедленного проброса.
    if (!audioCtx) {
        var attempts = 0;
        for (;;) {
            try {
                audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 16000 });
                break;
            } catch (e) {
                if (++attempts >= 5) throw e;
                await new Promise(function (r) { setTimeout(r, 100 * attempts); });
            }
        }
    }
    if (audioCtx.state === "suspended") {
        await audioCtx.resume();
    }
    return audioCtx;
}

async function playVoiceFrame(peerId, base64Audio) {
    var ctx = await getAudioCtx();
    var raw = atob(base64Audio);
    var samples = raw.length / 2;
    var buffer = ctx.createBuffer(1, samples, 16000);
    var channel = buffer.getChannelData(0);

    for (var i = 0; i < samples; i++) {
        var lo = raw.charCodeAt(i * 2);
        var hi = raw.charCodeAt(i * 2 + 1);
        var val = lo | (hi << 8);
        if (val >= 0x8000) val -= 0x10000;
        channel[i] = val / 32768.0;
    }

    if (!peerBuffers[peerId]) {
        peerBuffers[peerId] = {
            nextTime: 0,
            gainNode: ctx.createGain(),
            jitterMs: 60,      // адаптивный буфер: стартуем с 60ms
            lastArrival: 0,    // время прихода предыдущего фрейма
            arrivalDeltas: []  // скользящее окно задержек между фреймами
        };
        peerBuffers[peerId].gainNode.connect(ctx.destination);
    }

    var pb = peerBuffers[peerId];

    // Адаптивный jitter: замеряем разброс интервалов прихода фреймов
    var arrivalNow = performance.now();
    if (pb.lastArrival > 0) {
        var delta = arrivalNow - pb.lastArrival;

        // Сброс статистики при резком изменении RTT (>100ms скачок)
        if (pb.arrivalDeltas.length > 0) {
            var lastDelta = pb.arrivalDeltas[pb.arrivalDeltas.length - 1];
            if (Math.abs(delta - lastDelta) > 100) {
                pb.arrivalDeltas = [];
            }
        }

        pb.arrivalDeltas.push(delta);
        if (pb.arrivalDeltas.length > 50) pb.arrivalDeltas.shift(); // окно 50 фреймов (~1s)
        if (pb.arrivalDeltas.length >= 10) {
            // Jitter = стандартное отклонение интервалов
            var sum = 0;
            for (var d = 0; d < pb.arrivalDeltas.length; d++) sum += pb.arrivalDeltas[d];
            var mean = sum / pb.arrivalDeltas.length;
            var sqSum = 0;
            for (var d = 0; d < pb.arrivalDeltas.length; d++) {
                var diff = pb.arrivalDeltas[d] - mean;
                sqSum += diff * diff;
            }
            var jitter = Math.sqrt(sqSum / pb.arrivalDeltas.length);
            // Буфер = 2 × jitter, ограничен [40ms, 150ms]
            pb.jitterMs = Math.max(40, Math.min(150, jitter * 2));
        }
    }
    pb.lastArrival = arrivalNow;

    var source = ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(pb.gainNode);

    var jitterSec = pb.jitterMs / 1000;
    var now = ctx.currentTime;
    if (pb.nextTime < now) {
        pb.nextTime = now + jitterSec;
    }
    if (pb.nextTime > now + 0.3) {
        pb.nextTime = now + jitterSec;
    }
    source.start(pb.nextTime);
    pb.nextTime += buffer.duration;
}

export async function joinVoice(groupId) {
    voiceActive = true;
    try {
        // getAudioCtx внутри try: его исключение (WebView2 autoplay/audio)
        // раньше вылетало из joinVoice unhandled и оставляло voiceActive=true
        // при неработающем звуке.
        await getAudioCtx();
        await svc("JoinGroupVoice", groupId);
        // Apply saved per-peer volume settings for this group
        try {
            var data = JSON.parse(localStorage.getItem("voice_volumes_" + groupId) || "{}");
            for (var key in data) {
                if (key.startsWith("mute_")) {
                    if (data[key]) svc("SetPeerVolume", key.substring(5), 0).catch(function () {});
                } else {
                    svc("SetPeerVolume", key, data[key] / 100).catch(function () {});
                }
            }
        } catch (e) {}
    } catch (err) {
        console.error("join voice:", err);
        voiceActive = false;
    }
}

export async function leaveVoice() {
    voiceActive = false;

    // Звук выхода — играем ДО закрытия voice AudioContext
    // (sounds.js держит свой собственный default-rate контекст).
    playVoiceLeaveSound();

    // Освобождаем аудио-ресурсы
    for (var pid in peerBuffers) {
        try { peerBuffers[pid].gainNode.disconnect(); } catch (e) {}
    }
    peerBuffers = {};

    if (audioCtx) {
        try { audioCtx.close(); } catch (e) {}
        audioCtx = null;
    }

    try {
        stopScreenShareIfActive(); // бэкенд демонстрацию при выходе не гасит
        await svc("LeaveGroupVoice");
    } catch (err) {
        console.error("leave voice:", err);
    }
}

// getVoiceQuality — грубая оценка качества приёма для иконки сигнала на
// сцене: максимальный адаптивный jitter-буфер среди пиров (мс). Бэкенд
// метрики RTT не отдаёт; этого достаточно, чтобы отличить «ровно» от «рвётся».
export function getVoiceQuality() {
    var worst = 0;
    for (var pid in peerBuffers) {
        var j = peerBuffers[pid].jitterMs || 0;
        if (j > worst) worst = j;
    }
    return { jitterMs: worst };
}

export function isVoiceActive() {
    return voiceActive;
}

