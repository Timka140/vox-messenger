"use strict";

// composer-keys.js — чем отправлять сообщение: Enter (по умолчанию) или
// Ctrl+Enter (настройка «Отправка сообщений» во «Внешнем виде»).
//
//   Enter-режим:      Enter и Ctrl+Enter — отправить, Shift+Enter — новая строка.
//   Ctrl+Enter-режим: Ctrl+Enter — отправить, Enter — новая строка.
//
// Настройка send_on_enter ("1" | "0") лежит в БД (SaveSetting), читается
// после загрузки профиля (setup.js). Проверку isSendKey используют chat.js и
// composer-attach.js — одна точка правды для обоих слушателей поля.

import { svc, flog } from "./core.js";

var sendOnEnter = true;

export function isSendOnEnter() {
    return sendOnEnter;
}

export function setSendOnEnter(on) {
    sendOnEnter = !!on;
    svc("SaveSetting", "send_on_enter", sendOnEnter ? "1" : "0").catch(function (err) {
        flog("ERR", "frontend.composer-keys", "save: " + err);
    });
}

export function loadSendMode() {
    return svc("GetSetting", "send_on_enter").then(function (v) {
        sendOnEnter = v !== "0";
    }).catch(function () { /* профиль ещё не открыт — остаётся Enter */ });
}

// isSendKey — нажатие в поле ввода означает «отправить».
export function isSendKey(e) {
    if (e.key !== "Enter" || e.altKey || e.shiftKey) return false;
    if (e.ctrlKey || e.metaKey) return true;
    return sendOnEnter;
}
