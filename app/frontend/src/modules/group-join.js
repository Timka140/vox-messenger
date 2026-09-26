"use strict";

// group-join.js — вступление в группу по ссылке-приглашению (фаза 5d).
// Ссылка vox://group/… вставляется в поиск левой панели: вместо фильтра
// списка сверху появляется карточка «Вступить в группу» (владелец, срок).
// «Вступить» → JoinGroupByLink; дальше «Ждём владельца…», а итог приходит
// событием vox:group-join-result {groupId, code} — тостом и в карточке.

import { svc, state, Events } from "./core.js";
import { t, getLocale } from "./i18n.js";
import { showSuccess, showError, showInfo } from "./toast.js";
import { refreshPeers, selectGroup, showGroupsList } from "./contacts-list.js";

export const INVITE_PREFIX = "vox://group/";

export function isInviteLink(s) {
    return String(s || "").trim().toLowerCase().indexOf(INVITE_PREFIX) === 0;
}

let card = null;
let cardLink = "";
const previews = new Map(); // link → Promise<preview> (список перерисовывается часто)

function resultText(code, name) {
    const key = "join.result_" + code;
    let s = t(key);
    if (s === key) s = t("join.result_failed");
    return s.replace("{name}", name || "");
}

// errCode — invite_expired → expired и т.п. (коды ошибок GroupsService).
function errCode(err) {
    const m = String((err && err.message) || err);
    const hit = ["invite_expired", "invite_invalid", "invite_own_group", "invite_already_member"].find((c) => m.indexOf(c) >= 0);
    return hit ? hit.replace("invite_", "") : "failed";
}

function dateText(ms) {
    return new Date(ms).toLocaleDateString(getLocale() === "ru" ? "ru-RU" : "en-US", { day: "numeric", month: "short" }).replace(".", "");
}

function preview(link) {
    if (!previews.has(link)) {
        const p = svc("ParseGroupInviteLink", link);
        p.catch(() => previews.delete(link));
        previews.set(link, p);
    }
    return previews.get(link);
}

function openGroup(groupId) {
    document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.nav === "groups"));
    showGroupsList();
    const g = (state.groups || []).find((x) => x.groupId === groupId);
    if (g) selectGroup(groupId, g);
    const input = document.getElementById("search-input");
    if (input && isInviteLink(input.value)) {
        input.value = "";
        input.dispatchEvent(new Event("input"));
    }
}

export function removeJoinCard() {
    if (card) card.remove();
    card = null;
    cardLink = "";
}

// renderJoinCard — карточка над списком; повторный вызов с той же ссылкой
// (перерисовка списка) не дёргает Go заново.
export function renderJoinCard(container, link) {
    link = link.trim();
    if (card && cardLink === link && card.isConnected) return;
    removeJoinCard();
    cardLink = link;
    card = document.createElement("div");
    card.className = "join-card";
    card.innerHTML =
        '<div class="jc-icon"><i data-lucide="users" class="icon-20"></i></div>' +
        '<div class="jc-body"><div class="jc-title"></div><div class="jc-sub"></div></div>' +
        '<button type="button" class="jc-btn"></button>';
    container.prepend(card);
    if (window.lucide) lucide.createIcons({ nodes: [card] });
    const c = card;
    const title = c.querySelector(".jc-title");
    const sub = c.querySelector(".jc-sub");
    const btn = c.querySelector(".jc-btn");
    title.textContent = t("join.title");
    sub.textContent = "…";
    btn.textContent = t("join.join");
    btn.disabled = true;
    preview(link).then((p) => {
        if (card !== c) return;
        c.dataset.groupId = p.groupId;
        if (p.groupName) title.textContent = t("join.title_named").replace("{name}", p.groupName);
        sub.textContent = t("join.sub").replace("{owner}", p.ownerName).replace("{date}", dateText(p.expires));
        btn.disabled = false;
        if (p.member) {
            btn.textContent = t("join.open");
            btn.onclick = () => openGroup(p.groupId);
            return;
        }
        btn.onclick = () => {
            btn.disabled = true;
            svc("JoinGroupByLink", link).then(() => {
                sub.textContent = t("join.waiting").replace("{owner}", p.ownerName);
            }).catch((err) => {
                sub.textContent = resultText(errCode(err));
                btn.disabled = false;
            });
        };
    }).catch((err) => {
        if (card !== c) return;
        sub.textContent = resultText(errCode(err));
        btn.hidden = true;
    });
}

// joinGroupByLink — из поля «Прямое подключение» в настройках.
export function joinGroupByLink(link) {
    return svc("JoinGroupByLink", link.trim())
        .then(() => showInfo(t("join.sent")))
        .catch((err) => showError(resultText(errCode(err))));
}

Events.On("vox:group-join-result", (ev) => {
    const d = ev.data || {};
    previews.clear();
    Promise.resolve(refreshPeers()).then(() => {
        const g = (state.groups || []).find((x) => x.groupId === d.groupId);
        const text = resultText(d.code, g ? "«" + g.name + "»" : "");
        if (d.code === "ok") showSuccess(text); else showError(text);
        if (card && card.dataset.groupId === d.groupId) {
            card.querySelector(".jc-sub").textContent = d.code === "ok" ? t("join.done") : text;
            const btn = card.querySelector(".jc-btn");
            btn.hidden = false;
            btn.disabled = false;
            if (d.code === "ok") {
                btn.textContent = t("join.open");
                btn.onclick = () => openGroup(d.groupId);
            } else {
                btn.hidden = true;
            }
        }
    });
});
