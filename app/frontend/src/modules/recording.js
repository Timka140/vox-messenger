"use strict";

import { svc, state, dom } from "./core.js";
import { forgetMessage } from "./messages.js";
import { t } from "./i18n.js";
import { appendMessage } from "./messages.js";
import { updateContacts } from "./contacts-list.js";
import { showError } from "./toast.js";

// --- DOM elements ---
var recordBtn = null;
var recordingBar = null;
var inputRow = null;
var recDuration = null;
var recWaveform = null;
var recCancelBtn = null;
var recSendBtn = null;

// --- Module state ---
var recTimer = null;
var recActive = false;  // true пока идёт цикл опроса статуса записи
var sending = false;    // guard: sendVoiceMessage может быть вызван из UI и из auto-full одновременно
var waveformBars = []; // live waveform bars during recording

// --- Init (called once on page load) ---
export function initRecording() {
    recordBtn = document.getElementById("record-btn");
    recordingBar = document.getElementById("recording-bar");
    inputRow = document.getElementById("input-row");
    recDuration = document.getElementById("rec-duration");
    recWaveform = document.getElementById("rec-waveform");
    recCancelBtn = document.getElementById("rec-cancel-btn");
    recSendBtn = document.getElementById("rec-send-btn");

    if (recordBtn) recordBtn.addEventListener("click", startRecording);
    if (recCancelBtn) recCancelBtn.addEventListener("click", cancelRecording);
    if (recSendBtn) recSendBtn.addEventListener("click", sendVoiceMessage);
}

// --- Helpers ---

function showRecordingBar() {
    if (inputRow) inputRow.style.display = "none";
    if (recordingBar) recordingBar.style.display = "flex";
    // Clear waveform
    if (recWaveform) recWaveform.innerHTML = "";
    waveformBars = [];
}

function hideRecordingBar() {
    if (recordingBar) recordingBar.style.display = "none";
    if (inputRow) inputRow.style.display = "flex";
}

function formatDuration(ms) {
    var sec = Math.floor(ms / 1000);
    var min = Math.floor(sec / 60);
    sec = sec % 60;
    return min + ":" + (sec < 10 ? "0" : "") + sec;
}

// --- Recording functions ---

export async function startRecording() {
    if (!state.selectedPeerID && !state.selectedGroupID) return;
    if (recActive || sending) return; // уже пишем / отправляем — второй StartRecording не нужен
    recActive = true;
    var micIdx = -1;
    try {
        var sel = await svc("GetSelectedMicrophone");
        if (sel !== "" && sel !== null && sel !== undefined) {
            var n = parseInt(sel, 10);
            micIdx = isNaN(n) ? -1 : n; // «|| -1» превращал микрофон #0 в «default»
        }
    } catch (e) {}
    try {
        await svc("StartRecording", micIdx);
        showRecordingBar();
        if (window.lucide) lucide.createIcons();
        sending = false;
        scheduleStatusPoll();
    } catch (err) {
        recActive = false;
        console.error("start recording:", err);
        showError(String(err && err.message || err), { title: t("rec.start_failed") });
    }
}

// scheduleStatusPoll — setTimeout-цепочка вместо setInterval: следующий опрос
// планируется только после того как предыдущий ответ обработан. setInterval
// накапливал pending-промисы GetRecordingStatus (правило проекта — без polling),
// и каждый из них мог повторно вызвать sendVoiceMessage при status.full.
function scheduleStatusPoll() {
    if (!recActive) return;
    recTimer = setTimeout(function () {
        updateRecordingUI().finally(function () {
            if (recActive) scheduleStatusPoll();
        });
    }, 200);
}

function stopStatusPoll() {
    recActive = false;
    if (recTimer) { clearTimeout(recTimer); recTimer = null; }
}

function updateRecordingUI() {
    return svc("GetRecordingStatus").then(function (status) {
        if (!status) return;

        // Update duration display
        recDuration.textContent = formatDuration(status.duration);

        // Add waveform bar for current level (max 600 bars to avoid DOM bloat)
        if (recWaveform && waveformBars.length < 600) {
            var bar = document.createElement("div");
            bar.className = "wave-bar";
            // Нелинейная шкала: sqrt усиливает тихие звуки для лучшей визуализации
            var normalized = Math.sqrt(Math.min(1, status.level * 2)) * 100;
            var h = Math.max(4, Math.min(100, normalized));
            bar.style.height = h + "%";
            recWaveform.appendChild(bar);
            waveformBars.push(bar);

            // Auto-scroll waveform to the right (keep latest bars visible)
            recWaveform.scrollLeft = recWaveform.scrollWidth;
        }

        // Лимит достигнут — автоматически отправляем (guard в sendVoiceMessage
        // защитит от гонки с ручным нажатием «Отправить»).
        if (status.full) {
            sendVoiceMessage();
        }
    }).catch(function () {});
}

export async function cancelRecording() {
    if (sending) return; // отправка уже остановила запись
    stopStatusPoll();
    try { await svc("StopRecording"); } catch (e) {}
    hideRecordingBar();
}

export async function sendVoiceMessage() {
    if (sending) return; // уже отправляем (двойной клик / auto-full гонка)
    sending = true;
    stopStatusPoll();
    try {
        var result = await svc("StopRecording");
        hideRecordingBar();

        if (!result || !result.audio || (!state.selectedPeerID && !state.selectedGroupID)) return;

        // Фиксируем целевой чат до await'ов — иначе placeholder/бабл уйдёт в
        // чужой чат, если пользователь переключился во время отправки.
        var sentPeerID = state.selectedPeerID;
        var sentGroupID = state.selectedGroupID;
        function stillOnChat() {
            return sentGroupID ? state.selectedGroupID === sentGroupID
                               : state.selectedPeerID === sentPeerID;
        }

        // Показываем сообщение сразу (pending) — не ждём отправки
        var tempId = "sending-" + Date.now();
        appendMessage({
            id: tempId,
            senderId: state.myID,
            content: t("chat.voice_message"),
            timestamp: Date.now(),
            outbound: true,
            delivered: false,
            msgType: "voice",
            duration: result.duration,
            waveform: "",
        });
        dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;

        var sent;
        if (sentGroupID) {
            sent = await svc("SendGroupVoiceMessage", sentGroupID, result.audio, result.duration);
        } else {
            sent = await svc("SendVoiceMessage", sentPeerID, result.audio, result.duration);
        }

        // Заменяем placeholder на реальное сообщение
        var tempRow = dom.messagesDiv.querySelector('.message-row[data-msg-id="' + tempId + '"]');
        if (tempRow) tempRow.remove();
        forgetMessage(tempId);
        if (!stillOnChat()) return;
        appendMessage({
            id: sent.id,
            senderId: state.myID,
            content: t("chat.voice_message"),
            timestamp: Date.now(),
            outbound: true,
            delivered: true,
            msgType: "voice",
            duration: result.duration,
            waveform: sent.waveform,
        });
        dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
        updateContacts(state.peers);
    } catch (err) {
        console.error("send voice:", err);
    } finally {
        sending = false;
    }
}
