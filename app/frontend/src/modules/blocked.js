"use strict";

// blocked.js — «Заблокировать / Разблокировать» (фаза 7b, решения Тима
// 2026-09-15). Полный игнор делает бэкенд (services/blocklist.go); здесь —
// список для интерфейса, подтверждение, баннер вместо строки ввода в
// заблокированном чате. В группах сообщения заблокированного скрывает
// messages.js строкой «Сообщение от заблокированного скрыто».

import { svc, state, dom, Events, flog } from "./core.js";
import { t } from "./i18n.js";
import { confirmDialog } from "./ui.js";
import { showSuccess, showError } from "./toast.js";
import { refreshPeers } from "./contacts-list.js";

let blocked = new Set();

export function isBlocked(id) {
    return !!id && blocked.has(id);
}

export function blockedCount() {
    return blocked.size;
}

function changed() {
    applyBlockedState();
    document.dispatchEvent(new CustomEvent("vox:ui-blocked-changed"));
}

// loadBlocked — после входа (setup.js) и на vox:peer-blocked/unblocked.
// Отдаёт список [{peerId, nickname, blockedAt}] — для диалога в настройках.
export function loadBlocked() {
    return svc("GetBlockedPeers").then((list) => {
        blocked = new Set((list || []).map((p) => p.peerId));
        changed();
        return list || [];
    }).catch((err) => {
        flog("WRN", "frontend.blocked", "load: " + err);
        return [];
    });
}

// blockPeer — с подтверждением в шаблоне макета (Dialogs): вопрос, пояснение,
// красная кнопка.
export function blockPeer(id, nick) {
    const name = nick || id.substring(0, 8);
    confirmDialog(t("block.confirm_title").replace("{name}", name), t("block.confirm_text").replace(/\{name\}/g, name), t("block.block"), () => {
        svc("BlockPeer", id).then(() => {
            blocked.add(id);
            changed();
            refreshPeers();
            showSuccess(t("block.done").replace("{name}", name));
        }).catch((err) => showError(String(err && err.message || err)));
    });
}

export function unblockPeer(id) {
    return svc("UnblockPeer", id).then(() => {
        blocked.delete(id);
        changed();
        refreshPeers();
    }).catch((err) => showError(String(err && err.message || err)));
}

// applyBlockedState — открыт заблокированный личный чат: баннер вместо строки
// ввода. Иначе баннер спрятан, строку ввода возвращаем только если прятали.
export function applyBlockedState() {
    const banner = document.getElementById("blocked-banner");
    if (!banner) return;
    const pid = state.selectedGroupID ? null : state.selectedPeerID;
    if (pid && isBlocked(pid)) {
        const name = (state.selectedPeerInfo && state.selectedPeerInfo.nickname) || pid.substring(0, 8);
        banner.querySelector(".blocked-banner-text").textContent = t("block.banner").replace("{name}", name);
        banner.querySelector(".blocked-banner-btn").textContent = t("block.unblock");
        banner.hidden = false;
        if (dom.inputArea) dom.inputArea.style.display = "none";
    } else if (!banner.hidden) {
        banner.hidden = true;
        if (pid && dom.inputArea) dom.inputArea.style.display = "block";
    }
}

// Выбор/смена чата — selectPeer/selectGroup рассылают vox:ui-refresh-info-panel.
document.addEventListener("vox:ui-refresh-info-panel", applyBlockedState);
document.addEventListener("vox:ui-chat-changed", applyBlockedState);

const unblockBtn = document.querySelector("#blocked-banner .blocked-banner-btn");
if (unblockBtn) {
    unblockBtn.addEventListener("click", () => {
        if (state.selectedPeerID) unblockPeer(state.selectedPeerID);
    });
}

// Блок/разблок из другого места (другое окно, повтор) — список с бэкенда.
Events.On("vox:peer-blocked", () => { loadBlocked(); });
Events.On("vox:peer-unblocked", () => { loadBlocked(); });
