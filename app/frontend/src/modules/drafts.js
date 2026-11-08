"use strict";

// drafts.js — черновики строки ввода по чатам (ревью интерфейса 2026-11).
//
// Раньше текст, набранный в одном чате, оставался в поле при переходе в другой
// и мог уйти не тому адресату. Теперь при смене чата текст запоминается за
// прежним чатом и возвращается при возврате, а в списке чатов неотправленный
// текст виден как «Черновик: …». Хранится в localStorage (vox-drafts) и
// переживает перезапуск. Ключи — как у mute.js: p:<peerId> / g:<groupId>.
//
// Порядок событий: vox:ui-chat-changed уходит из selectPeer/selectGroup ДО
// смены state — в этот момент поле ещё принадлежит прежнему чату (saveCurrent);
// restoreDraft зовут те же функции уже после смены state.

import { state, dom, autoGrowTextarea } from "./core.js";
import { peerKey, groupKey } from "./mute.js";

const STORE = "vox-drafts";
let drafts = {};
try { drafts = JSON.parse(localStorage.getItem(STORE) || "{}") || {}; } catch (e) { drafts = {}; }

let persistTimer = null;
function persistNow() {
    try { localStorage.setItem(STORE, JSON.stringify(drafts)); } catch (e) { /* хранилище недоступно */ }
}
function persist() {
    if (persistTimer) clearTimeout(persistTimer);
    persistTimer = setTimeout(function () { persistTimer = null; persistNow(); }, 300);
}

// currentKey — ключ открытого чата; "" для mesh-эфира и пустого выбора.
export function currentKey() {
    if (state.selectedGroupID) return groupKey(state.selectedGroupID);
    if (state.selectedPeerID && String(state.selectedPeerID).indexOf("__mesh:") !== 0) return peerKey(state.selectedPeerID);
    return "";
}

export function getDraft(key) {
    return (key && drafts[key]) || "";
}

export function setDraft(key, text) {
    if (!key) return;
    if (!text || !text.trim()) {
        if (key in drafts) { delete drafts[key]; persist(); }
        return;
    }
    if (drafts[key] === text) return;
    drafts[key] = text;
    persist();
}

export function clearDraft(key) {
    setDraft(key, "");
}

// Редактирование сообщения: в поле чужой текст, не черновик — не трогаем.
function editing() {
    return !!(dom.inputArea && dom.inputArea.classList.contains("is-editing"));
}

// saveCurrent — запомнить текст поля за открытым чатом.
function saveCurrent() {
    if (!dom.messageInput || editing()) return;
    setDraft(currentKey(), dom.messageInput.value);
}

// restoreDraft — подставить черновик открытого чата в поле (после смены
// state или по окончании редактирования).
export function restoreDraft() {
    if (!dom.messageInput) return;
    var text = getDraft(currentKey());
    if (dom.messageInput.value === text) return;
    dom.messageInput.value = text;
    autoGrowTextarea();
}

document.addEventListener("vox:ui-chat-changed", saveCurrent);
if (dom.messageInput) {
    dom.messageInput.addEventListener("input", function () {
        if (editing()) return;
        setDraft(currentKey(), dom.messageInput.value);
    });
}
window.addEventListener("beforeunload", function () {
    saveCurrent();
    persistNow();
});
