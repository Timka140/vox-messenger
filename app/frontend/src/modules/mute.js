"use strict";

// mute.js — «Без звука» для отдельного чата (макеты HeaderMenus, InfoDM,
// InfoGroup). Заглушённый чат не играет звук и не показывает всплывающее
// уведомление; счётчик непрочитанных растёт как обычно. Уведомления
// запускает только фронт (events.js), поэтому хранение — одна настройка
// muted_chats: JSON-массив ключей "p:<peerId>" / "g:<groupId>".

import { svc, state, flog } from "./core.js";

var muted = new Set();

export function peerKey(id) { return "p:" + id; }
export function groupKey(id) { return "g:" + id; }

// currentChatMuteKey — ключ открытого чата; "" для mesh и пустого выбора.
export function currentChatMuteKey() {
    if (state.selectedGroupID) return groupKey(state.selectedGroupID);
    var pid = state.selectedPeerID;
    if (pid && String(pid).indexOf("__mesh:") !== 0) return peerKey(pid);
    return "";
}

export function isMuted(key) {
    return !!key && muted.has(key);
}

// loadMuted — после входа (setup.js showChat): до LoadIdentity настроек нет.
export function loadMuted() {
    return svc("GetSetting", "muted_chats").then(function (v) {
        var arr = [];
        try { arr = JSON.parse(v || "[]"); } catch (e) { arr = []; }
        muted = new Set(Array.isArray(arr) ? arr : []);
        document.dispatchEvent(new CustomEvent("vox:ui-mute-changed", { detail: {} }));
    }).catch(function (err) {
        flog("WRN", "frontend.mute", "load: " + err);
    });
}

export function setMuted(key, on) {
    if (!key) return;
    if (on) muted.add(key); else muted.delete(key);
    svc("SaveSetting", "muted_chats", JSON.stringify(Array.from(muted))).catch(function (err) {
        flog("ERR", "frontend.mute", "save: " + err);
    });
    document.dispatchEvent(new CustomEvent("vox:ui-mute-changed", { detail: { key: key, on: !!on } }));
}

export function toggleMuted(key) {
    setMuted(key, !isMuted(key));
}
