"use strict";

// contacts.js — режим «Контакты» левой панели (макет Contacts / Requests):
// поиск «Имя или Ник#код», строка «Заявки в друзья» (скрыта при 0) с
// раскрытием списка входящих/исходящих, «Друзья · N». Центр при этом
// показывает profile.js (профиль друга / карточка заявки / карточка
// «Отправить заявку» по тегу) — связь через DOM-события vox:ui-profile-show.
//
// Режим включает contacts-list.js showContactsMode() (viewMode="contacts") и
// просит рендер событием vox:ui-contacts-render; данные (GetFriends,
// GetPendingRequests) кэшируются здесь и обновляются по vox:ui-contacts-refresh
// (events.js шлёт его на vox:friend-*). Бейдж входящих заявок дублируется на
// кнопке nav (#contacts-nav-badge).

import { svc, flog, state, dom, colorFromId, formatTime } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { getViewMode, selectPeer, showMessagesList, refreshPeers } from "./contacts-list.js";
import { showSuccess } from "./toast.js";
import { applyAvatar } from "./avatar.js";

const TAG_RE = /^\s*\S.*#[0-9a-f]{4,}\s*$/i;

let friends = [];
let requests = [];
let loaded = false;
let loading = null;
let reloadAgain = false;    // refresh пришёл во время загрузки — её данные уже могли устареть
let requestsOpen = false;
let activePeer = null;      // peerId выбранного друга
let activeRequest = null;   // id выбранной заявки
let filter = "";
let tagTimer = null;

function isMode() {
    return getViewMode() === "contacts";
}

function incoming() { return requests.filter((r) => r.direction === 0); }
function outgoing() { return requests.filter((r) => r.direction !== 0); }

function initials(name) {
    return (name || "?").substring(0, 2).toUpperCase();
}

function shortTag(nick, peerId) {
    return (nick || "user") + "#" + peerId.substring(0, 16);
}

// --- данные ---

export function loadContacts() {
    if (loading) {
        reloadAgain = true;
        return loading;
    }
    loading = Promise.all([
        svc("GetFriends").catch(() => []),
        svc("GetPendingRequests").catch(() => []),
    ]).then(([f, r]) => {
        friends = f || [];
        requests = r || [];
        loaded = true;
        if (requests.length === 0) requestsOpen = false; // заявок не осталось — обратно к друзьям
        updateContactsBadge();
        if (isMode()) {
            render();
            // Профиль друга в центре держит старый объект — статус «в сети» из него.
            const cur = activePeer && !activeRequest ? getFriend(activePeer) : null;
            if (cur) document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "friend", friend: cur } }));
        }
    }).finally(() => {
        loading = null;
        if (reloadAgain) {
            reloadAgain = false;
            loadContacts();
        }
    });
    return loading;
}

export function updateContactsBadge() {
    const n = incoming().length;
    const badge = document.getElementById("contacts-nav-badge");
    if (!badge) return;
    badge.textContent = n > 99 ? "99+" : String(n);
    badge.hidden = n === 0;
}

export function getFriend(peerId) {
    return friends.find((f) => f.peerId === peerId) || null;
}

// --- рендер левой панели ---

function friendRow(f) {
    const item = document.createElement("div");
    item.className = "contact-item" + (f.peerId === activePeer ? " active" : "");
    item.dataset.peerId = f.peerId;
    const avatar = document.createElement("div");
    avatar.className = "contact-avatar" + (f.online ? "" : " is-offline");
    applyAvatar(avatar, f.peerId, f.nickname || f.peerId);
    if (f.online) {
        const dot = document.createElement("span");
        dot.className = "avatar-online-dot";
        avatar.appendChild(dot);
    }
    const body = document.createElement("div");
    body.className = "contact-body";
    body.innerHTML =
        '<div class="contact-top-row"><span class="contact-name"></span></div>' +
        '<div class="contact-bottom-row"><span class="contact-preview ' + (f.online ? "is-online" : "") + '"></span></div>';
    body.querySelector(".contact-name").textContent = f.nickname || f.peerId.substring(0, 16);
    body.querySelector(".contact-preview").textContent = f.online ? t("contacts.online") : t("contacts.offline");
    item.appendChild(avatar);
    item.appendChild(body);
    item.addEventListener("click", () => {
        activePeer = f.peerId;
        activeRequest = null;
        render();
        document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "friend", friend: f } }));
    });
    item.addEventListener("contextmenu", (e) => {
        showContextMenu(e, [
            { label: t("ctx.send_message"), icon: "message-square", action: () => {
                showMessagesList();
                selectPeer(f.peerId, { id: f.peerId, nickname: f.nickname, shortId: f.peerId.substring(0, 16) });
            } },
            { label: t("ctx.copy_tag"), icon: "copy", action: () => { try { navigator.clipboard.writeText(shortTag(f.nickname, f.peerId)); } catch (err) { /* нет фокуса */ } } },
            { separator: true },
            { label: t("ctx.remove_friend"), icon: "user-minus", danger: true, action: () => {
                document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "friend", friend: f, confirmRemove: true } }));
            } },
        ]);
    });
    return item;
}

function requestRow(r) {
    const item = document.createElement("div");
    item.className = "contact-item" + (r.id === activeRequest ? " active" : "");
    item.dataset.requestId = r.id;
    const avatar = document.createElement("div");
    avatar.className = "contact-avatar";
    applyAvatar(avatar, r.peerId, r.nickname || r.peerId);
    const body = document.createElement("div");
    body.className = "contact-body";
    body.innerHTML =
        '<div class="contact-top-row"><span class="contact-name"></span></div>' +
        '<div class="contact-bottom-row"><span class="contact-preview"></span></div>';
    body.querySelector(".contact-name").textContent = r.nickname || r.peerId.substring(0, 16);
    const when = r.timestamp ? formatTime(r.timestamp) : "";
    body.querySelector(".contact-preview").textContent = r.direction === 0
        ? shortTag(r.nickname, r.peerId) + (when ? " · " + when : "")
        : t("contacts.pending_sent").replace("{t}", when);
    item.appendChild(avatar);
    item.appendChild(body);
    item.addEventListener("click", () => {
        activeRequest = r.id;
        activePeer = null;
        render();
        document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "request", request: r } }));
    });
    return item;
}

function cap(text) {
    const el = document.createElement("div");
    el.className = "contacts-cap";
    el.textContent = text;
    return el;
}

function stackAvatars(list) {
    const wrap = document.createElement("div");
    wrap.className = "contacts-stack";
    list.slice(0, 2).forEach((r) => {
        const a = document.createElement("span");
        a.className = "contacts-stack-avatar";
        applyAvatar(a, r.peerId, r.nickname || r.peerId, { letters: 1 });
        wrap.appendChild(a);
    });
    return wrap;
}

function requestsRow() {
    const inc = incoming();
    const row = document.createElement("div");
    row.className = "contacts-requests-row";
    row.appendChild(stackAvatars(inc));
    const body = document.createElement("div");
    body.className = "contact-body";
    body.innerHTML = '<span class="contact-name"></span><span class="contact-preview"></span>';
    body.querySelector(".contact-name").textContent = t("contacts.requests_row");
    body.querySelector(".contact-preview").textContent = inc.map((r) => r.nickname || r.peerId.substring(0, 8)).join(", ");
    row.appendChild(body);
    const badge = document.createElement("span");
    badge.className = "nav-badge static";
    badge.textContent = String(inc.length);
    row.appendChild(badge);
    row.insertAdjacentHTML("beforeend", '<i data-lucide="chevron-right" class="icon-16 contacts-chev"></i>');
    row.addEventListener("click", () => { requestsOpen = true; render(); });
    return row;
}

function backRow() {
    const row = document.createElement("div");
    row.className = "contacts-requests-row is-back";
    row.innerHTML = '<i data-lucide="arrow-left" class="icon-16 contacts-chev"></i><span class="contact-name"></span>';
    row.querySelector(".contact-name").textContent = t("contacts.requests_row");
    const n = incoming().length;
    if (n > 0) {
        const badge = document.createElement("span");
        badge.className = "nav-badge static";
        badge.style.marginLeft = "auto";
        badge.textContent = String(n);
        row.appendChild(badge);
    }
    row.addEventListener("click", () => { requestsOpen = false; render(); });
    return row;
}

function emptyState() {
    const box = document.createElement("div");
    box.className = "contacts-empty-card";
    box.innerHTML =
        '<i data-lucide="contact" class="contacts-empty-icon"></i>' +
        '<div class="contacts-empty-title"></div>' +
        '<div class="contacts-empty-desc"></div>' +
        '<button type="button" class="profile-btn pri contacts-empty-btn"></button>';
    box.querySelector(".contacts-empty-title").textContent = t("contacts.empty_title");
    box.querySelector(".contacts-empty-desc").textContent = t("contacts.empty_desc");
    const btn = box.querySelector(".contacts-empty-btn");
    btn.textContent = t("contacts.empty_copy");
    btn.addEventListener("click", copyMyLink);
    return box;
}

export async function copyMyLink() {
    try {
        const uri = await svc("GetMyContactURI");
        if (uri) {
            await navigator.clipboard.writeText(uri);
            showSuccess(t("contacts.link_copied"));
        }
    } catch (err) {
        flog("ERR", "contacts", "copy link: " + (err && err.message || err));
    }
}

export function render() {
    if (!isMode()) return;
    if (!loaded) { loadContacts(); }
    const list = dom.contactsList;
    list.innerHTML = "";
    const addBtn = document.getElementById("contacts-add-btn");
    if (addBtn) addBtn.style.display = "";
    if (dom.composeBtn) dom.composeBtn.style.display = "none";

    if (requestsOpen) {
        list.appendChild(backRow());
        const inc = incoming(), out = outgoing();
        if (inc.length > 0) {
            list.appendChild(cap(t("contacts.incoming_n").replace("{n}", String(inc.length))));
            inc.forEach((r) => list.appendChild(requestRow(r)));
        }
        if (out.length > 0) {
            list.appendChild(cap(t("contacts.outgoing_n").replace("{n}", String(out.length))));
            out.forEach((r) => list.appendChild(requestRow(r)));
        }
        if (inc.length === 0 && out.length === 0) {
            list.appendChild(cap(t("contacts.requests.no_incoming")));
        }
    } else {
        if (incoming().length > 0) list.appendChild(requestsRow());
        const f = filter.trim().toLowerCase();
        const shown = f ? friends.filter((x) => (x.nickname || "").toLowerCase().indexOf(f) >= 0) : friends;
        if (friends.length === 0 && loaded) {
            list.appendChild(emptyState());
        } else {
            list.appendChild(cap(t("contacts.friends_n").replace("{n}", String(shown.length))));
            shown.forEach((x) => list.appendChild(friendRow(x)));
        }
    }
    if (window.lucide) lucide.createIcons({ nodes: [list] });
}

// leaveMode — контролы шапки обратно к обычному виду (вызывает contacts-list при смене режима)
export function leaveMode() {
    const addBtn = document.getElementById("contacts-add-btn");
    if (addBtn) addBtn.style.display = "none";
    if (dom.composeBtn) dom.composeBtn.style.display = "";
    const input = document.getElementById("search-input");
    if (input) input.placeholder = t("search.placeholder");
    activePeer = null;
    activeRequest = null;
    requestsOpen = false;
}

export function enterMode() {
    const input = document.getElementById("search-input");
    if (input) {
        input.placeholder = t("contacts.search_placeholder");
        filter = input.value || "";
    }
    render();
}

// --- поиск: имя фильтрует список, тег Ник#код показывает карточку заявки ---

function onSearchInput() {
    if (!isMode()) return;
    const input = document.getElementById("search-input");
    filter = input ? input.value : "";
    render();
    if (tagTimer) clearTimeout(tagTimer);
    if (TAG_RE.test(filter)) {
        tagTimer = setTimeout(() => {
            document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "tag", tag: filter.trim() } }));
        }, 400);
    }
}

export function initContacts() {
    document.addEventListener("vox:ui-contacts-refresh", () => { loaded = false; loadContacts(); });
    document.addEventListener("vox:ui-contacts-render", () => { enterMode(); });
    document.addEventListener("vox:ui-contacts-leave", () => { leaveMode(); });
    // profile.js после accept/reject/remove просит снять выделение
    document.addEventListener("vox:ui-contacts-clear-selection", () => { activePeer = null; activeRequest = null; if (isMode()) render(); });
    const input = document.getElementById("search-input");
    if (input) {
        input.addEventListener("input", onSearchInput);
        input.addEventListener("keydown", (e) => {
            if (e.key === "Enter" && isMode() && TAG_RE.test(input.value)) {
                if (tagTimer) clearTimeout(tagTimer);
                document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "tag", tag: input.value.trim() } }));
            }
        });
    }
    const addBtn = document.getElementById("contacts-add-btn");
    if (addBtn) addBtn.addEventListener("click", () => {
        if (input) { input.focus(); input.select(); }
        document.dispatchEvent(new CustomEvent("vox:ui-profile-show", { detail: { kind: "tag", tag: "" } }));
    });
    // Бейдж заявок при старте (identity грузится асинхронно — с задержкой)
    setTimeout(loadContacts, 2000);
}
