"use strict";

// sounds.js — generic Web Audio beep helpers used for app feedback.
// Voice playback контекст НЕ переиспользуется: voice требует фиксированный
// 16 kHz sampleRate (см. voice.js getAudioCtx), а beep'ы могут жить на default.
//
// «Звуки событий» (фаза 7c, макет SettingsAppearance): одна громкость на все
// звуки и выключатель у каждого события. sound_volume — 0..100, по умолчанию
// 60: при 60 громкость прежняя. sound_events — JSON {message, call, voice}.
// Звонки (calls.js) берут множитель через soundGain("call").

import { svc, flog } from "./core.js";

const DEFAULT_VOLUME = 60;
const DEFAULT_EVENTS = { message: true, call: true, voice: false };

var prefs = { volume: DEFAULT_VOLUME, events: Object.assign({}, DEFAULT_EVENTS) };

// loadSoundPrefs — после входа (setup.js showChat), рядом с loadChatPrefs.
export function loadSoundPrefs() {
    return Promise.all([
        svc("GetSetting", "sound_volume").catch(function () { return ""; }),
        svc("GetSetting", "sound_events").catch(function () { return ""; }),
    ]).then(function (r) {
        var n = parseInt(r[0], 10);
        prefs.volume = isNaN(n) ? DEFAULT_VOLUME : Math.max(0, Math.min(100, n));
        var ev = Object.assign({}, DEFAULT_EVENTS);
        try {
            var o = JSON.parse(r[1] || "{}");
            if (o && typeof o === "object") {
                Object.keys(DEFAULT_EVENTS).forEach(function (k) {
                    if (typeof o[k] === "boolean") ev[k] = o[k];
                });
            }
        } catch (e) {}
        prefs.events = ev;
    });
}

export function getSoundPrefs() {
    return { volume: prefs.volume, events: Object.assign({}, prefs.events) };
}

export function setSoundVolume(v) {
    prefs.volume = Math.max(0, Math.min(100, parseInt(v, 10) || 0));
    svc("SaveSetting", "sound_volume", String(prefs.volume)).catch(function (err) { flog("ERR", "frontend.sounds", "save volume: " + err); });
}

export function setSoundEvent(kind, on) {
    if (!(kind in DEFAULT_EVENTS)) return;
    prefs.events[kind] = !!on;
    svc("SaveSetting", "sound_events", JSON.stringify(prefs.events)).catch(function (err) { flog("ERR", "frontend.sounds", "save events: " + err); });
}

// soundGain — множитель громкости события: 0 — выключено.
export function soundGain(kind) {
    if (!prefs.events[kind]) return 0;
    return prefs.volume / DEFAULT_VOLUME;
}

var _ctx = null;

function ctx() {
    if (!_ctx) _ctx = new (window.AudioContext || window.webkitAudioContext)();
    // WebView2 создаёт контекст в suspended до первого user-gesture — без
    // resume() все beep'ы были бы беззвучны навсегда.
    if (_ctx.state === "suspended") {
        try { _ctx.resume().catch(function () {}); } catch (_) {}
    }
    return _ctx;
}

// playBeep — короткий sine-tone с (опционально) частотным glide.
//   freq      — частота в Гц (или массив [start, end] для glide)
//   duration  — секунд звучания (при glide частота меняется первые ~75% длительности)
//   volume    — стартовая громкость (0..1), затухает экспоненциально; 0 — тишина
function playBeep(freq, duration, volume) {
    if (!(volume > 0)) return;
    try {
        var c = ctx();
        var osc = c.createOscillator();
        var gain = c.createGain();
        osc.connect(gain);
        gain.connect(c.destination);
        osc.type = "sine";

        var now = c.currentTime;
        if (Array.isArray(freq)) {
            osc.frequency.setValueAtTime(freq[0], now);
            osc.frequency.linearRampToValueAtTime(freq[1], now + duration * 0.75);
        } else {
            osc.frequency.value = freq;
        }
        gain.gain.setValueAtTime(volume, now);
        gain.gain.exponentialRampToValueAtTime(0.001, now + duration);
        osc.start(now);
        osc.stop(now + duration);
    } catch (e) {}
}

export function playNotificationSound() {
    playBeep(880, 0.3, 0.15 * soundGain("message"));
}

export function playVoiceJoinSound() {
    playBeep([440, 660], 0.2, 0.12 * soundGain("voice"));
}

export function playVoiceLeaveSound() {
    playBeep([660, 440], 0.2, 0.12 * soundGain("voice"));
}

// playSoundPreview — пробный сигнал при смене громкости в Настройках
// (звучит, даже если все события выключены).
export function playSoundPreview() {
    playBeep(880, 0.3, 0.15 * prefs.volume / DEFAULT_VOLUME);
}
