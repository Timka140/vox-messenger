"use strict";

// signal-banner.js — баннер «Нет связи с сигнал-сервером» над списком чатов
// (макет EmptyStates): не модалка, исчезает сам, когда связь вернулась.
// Показываем только если связи нет дольше OFFLINE_GRACE_MS — чтобы не мигать
// при старте и коротких переподключениях. Опроса нет: состояние проверяем по
// vox:signal-updated и один раз после старта.

import { svc, Events } from "./core.js";
import { showError } from "./toast.js";

const OFFLINE_GRACE_MS = 5000;
let timer = null;

function banner() {
    return document.getElementById("signal-banner");
}

function isOnline() {
    return svc("GetSignalServers").then((servers) => !!(servers && servers.some((s) => s.online))).catch(() => false);
}

function check() {
    isOnline().then((online) => {
        const b = banner();
        if (!b) return;
        if (online) {
            clearTimeout(timer);
            timer = null;
            b.hidden = true;
            return;
        }
        if (timer || !b.hidden) return;
        timer = setTimeout(() => {
            timer = null;
            isOnline().then((nowOnline) => { if (!nowOnline) b.hidden = false; });
        }, OFFLINE_GRACE_MS);
    });
}

Events.On("vox:signal-updated", check);
// Первый старт: если сигналка так и не ответила, vox:signal-updated может не прийти.
setTimeout(check, 8000);

const retry = document.getElementById("signal-banner-retry");
if (retry) {
    retry.addEventListener("click", () => {
        retry.disabled = true;
        svc("ReconnectSignal").catch((err) => showError(String((err && err.message) || err)))
            .finally(() => { retry.disabled = false; check(); });
    });
}
