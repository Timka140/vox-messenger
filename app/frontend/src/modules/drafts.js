"use strict";

// drafts.js — черновики строки ввода по чатам (ревью интерфейса 2026-11).
//
// Раньше текст, набранный в одном чате, оставался в поле при переходе в другой
// и мог уйти не тому адресату. Теперь при смене чата текст запоминается за
// прежним чатом и возвращается при возврате, а в списке чатов неотправленный
// текст виден как «Черновик: …». Ключи — как у mute.js: p:<peerId> / g:<groupId>.
//
// Хранение — настройка drafts в БД профиля (SaveSetting), как pinned_chats:
// текст сообщений не должен лежать на диске открытым, а черновики одного
// профиля — доставаться другому. Читается после входа (setup.js showChat).
//
// Порядок событий: vox:ui-chat-changed уходит из selectPeer/selectGroup ДО
// смены state — в этот момент поле ещё принадлежит прежнему чату (saveCurrent);
// restoreDraft зовут те же функции уже после смены state.

import { svc, flog, state, dom, autoGrowTextarea } from "./core.js";
import { peerKey, groupKey } from "./mute.js";

const SETTING = "drafts";
let drafts = {};
// До loadDrafts в БД не пишем: пустой набор затёр бы сохранённые черновики.
let loaded = false;

// Первая версия держала черновики в localStorage открытым текстом — убираем.
try { localStorage.removeItem("vox-drafts"); } catch (e) { /* хранилище недоступно */ }

let persistTimer = null;
function persistNow() {
    if (persistTimer) { clearTimeout(persistTimer); persistTimer = null; }
    if (!loaded) return;
    svc("SaveSetting", SETTING, JSON.stringify(drafts)).catch(function (err) {
        flog("ERR", "frontend.drafts", "save: " + err);
    });
}
function persist() {
    if (persistTimer) clearTimeout(persistTimer);
    persistTimer = setTimeout(persistNow, 300);
}

// loadDrafts — после входа (setup.js showChat), рядом с loadChatPrefs.
export function loadDrafts() {
    return svc("GetSetting", SETTING).catch(function () { return ""; }).then(function (v) {
        try {
            var o = JSON.parse(v || "{}");
            drafts = o && typeof o === "object" && !Array.isArray(o) ? o : {};
        } catch (e) { drafts = {}; }
        loaded = true;
        restoreDraft();
    });
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

// forgetDraft — чат удалён (переписка, группа, исключение из группы): черновик
// не должен всплыть снова. Если чат открыт — чистим и само поле, иначе текст
// вернулся бы в черновики при первой же смене чата.
export function forgetDraft(key) {
    if (!key) return;
    if (dom.messageInput && !editing() && currentKey() === key && dom.messageInput.value) {
        dom.messageInput.value = "";
        autoGrowTextarea();
    }
    clearDraft(key);
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
    if (persistTimer) persistNow();
});
