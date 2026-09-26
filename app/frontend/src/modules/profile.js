// profile.js — центр области чата в режиме «Контакты» (макеты Contacts,
// Requests, gen-more «поиск по тегу»): подменяет ленту, как mesh-chat-view.
//
// Три вида по vox:ui-profile-show {kind}:
//   friend  — профиль друга: аватар, тег, статус, Написать / Позвонить /
//             В группу; факты (в друзьях с, общие группы, Mesh-узел, ключ);
//             звонки с этим человеком (страница «Звонки» удалена — история
//             живёт здесь); удалить из друзей / удалить переписку.
//   request — карточка заявки: Принять / Отклонить (входящая) или статус
//             ожидания (исходящая).
//   tag     — карточка «Отправить заявку» по введённому Ник#код (SearchUser) —
//             единственный путь добавления в друзья.
// vox:ui-profile-hide — прячет вид (contacts-list при смене режима/чата).

import { svc, flog, state, dom, colorFromId, formatMmSs, formatDateLabel, formatTime } from "./core.js";
import { t } from "./i18n.js";
import { selectPeer, showMessagesList, deselectChat, refreshPeers } from "./contacts-list.js";
import { startCall, showGroupCallDialog } from "./calls.js";
import { buildDialogOverlay, escapeHtml, confirmDialog as uiConfirmDialog } from "./ui.js";
import { showSuccess, showError } from "./toast.js";
import { applyAvatar } from "./avatar.js";
import { isMuted, toggleMuted, peerKey } from "./mute.js";

// Тумблер «Уведомления от этого контакта» открытого профиля — обновление по
// vox:ui-mute-changed (тот же «Без звука», что в «…» и панели).
let notifySync = null;
document.addEventListener("vox:ui-mute-changed", () => { if (notifySync) notifySync(); });

const el = {};
let current = null;      // {kind, ...detail}
let renderGen = 0;

function q(id) { return document.getElementById(id); }

function initials(name) { return (name || "?").substring(0, 2).toUpperCase(); }
function shortTag(nick, peerId) { return (nick || "user") + "#" + peerId.substring(0, 16); }

function show(detail) {
    current = detail;
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    deselectChat();
    if (dom.chatHeader) dom.chatHeader.style.display = "none";
    if (dom.messagesDiv) dom.messagesDiv.style.display = "none";
    if (dom.inputArea) dom.inputArea.style.display = "none";
    el.view.hidden = false;
    render();
}

function hide() {
    if (el.view.hidden) return;
    el.view.hidden = true;
    current = null;
    if (dom.messagesDiv) dom.messagesDiv.style.display = "";
    el.body.innerHTML = "";
}

function render() {
    if (!current) return;
    const gen = ++renderGen;
    el.body.innerHTML = "";
    if (current.kind === "friend") renderFriend(current.friend, gen, !!current.confirmRemove);
    else if (current.kind === "request") renderRequest(current.request, gen);
    else if (current.kind === "tag") renderTag(current.tag, gen);
}

// --- общие кусочки ---

function card(cls) {
    const c = document.createElement("div");
    c.className = "profile-card" + (cls ? " " + cls : "");
    return c;
}

function btn(label, icon, cls, onClick) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "profile-btn " + cls;
    b.innerHTML = (icon ? '<i data-lucide="' + icon + '" class="icon-16"></i>' : "") + "<span></span>";
    b.querySelector("span").textContent = label;
    if (onClick) b.addEventListener("click", onClick);
    return b;
}

function identity(peerId, nick, statusHTML) {
    const c = card("center");
    const av = document.createElement("div");
    av.className = "profile-avatar";
    applyAvatar(av, peerId, nick || peerId);
    const name = document.createElement("div");
    name.className = "profile-name";
    name.textContent = nick || peerId.substring(0, 16);
    const tag = document.createElement("div");
    tag.className = "profile-tag";
    tag.innerHTML = '<span class="mono"></span><button type="button" class="profile-icon-btn" title=""><i data-lucide="copy" class="icon-14"></i></button>';
    tag.querySelector(".mono").textContent = shortTag(nick, peerId);
    tag.querySelector("button").title = t("ctx.copy_tag");
    tag.querySelector("button").addEventListener("click", () => {
        try { navigator.clipboard.writeText(shortTag(nick, peerId)); showSuccess(t("profile.tag_copied")); } catch (err) { /* нет фокуса */ }
    });
    c.appendChild(av);
    c.appendChild(name);
    c.appendChild(tag);
    if (statusHTML) {
        const st = document.createElement("div");
        st.className = "profile-status";
        st.innerHTML = statusHTML;
        c.appendChild(st);
    }
    return c;
}

function kv(label, valueNode) {
    const row = document.createElement("div");
    row.className = "profile-kv";
    const l = document.createElement("span");
    l.className = "profile-kv-label";
    l.textContent = label;
    row.appendChild(l);
    if (typeof valueNode === "string") {
        const b = document.createElement("b");
        b.textContent = valueNode;
        row.appendChild(b);
    } else {
        row.appendChild(valueNode);
    }
    return row;
}

// confirmDialog — общий шаблон подтверждения (ui.js, макет Dialogs).
function confirmDialog(title, text, confirmLabel, onConfirm) {
    uiConfirmDialog(title, text, confirmLabel, onConfirm);
}

// --- профиль друга ---

async function renderFriend(f, gen, confirmRemove) {
    const peerId = f.peerId;
    const nick = f.nickname;
    const online = !!f.online;
    const statusHTML = '<span class="profile-dot' + (online ? "" : " off") + '"></span>' + escapeHtml(online ? t("contacts.online") : t("contacts.offline"));
    const head = identity(peerId, nick, statusHTML);
    const actions = document.createElement("div");
    actions.className = "profile-actions";
    actions.appendChild(btn(t("profile.write"), "message-square", "pri", () => {
        showMessagesList();
        selectPeer(peerId, { id: peerId, nickname: nick, shortId: peerId.substring(0, 16) });
        const navMsg = document.querySelector('.nav-item[data-nav="messages"]');
        document.querySelectorAll(".nav-item").forEach((b) => b.classList.remove("active"));
        if (navMsg) navMsg.classList.add("active");
    }));
    actions.appendChild(btn(t("profile.call"), "phone", "sec", () => {
        showMessagesList();
        selectPeer(peerId, { id: peerId, nickname: nick, shortId: peerId.substring(0, 16) }).then(() => startCall(peerId));
    }));
    actions.appendChild(btn(t("profile.group_call"), "users", "sec", () => showGroupCallDialog([peerId])));
    head.appendChild(actions);
    el.body.appendChild(head);

    // Факты
    const facts = card("facts");
    if (f.since) facts.appendChild(kv(t("profile.since"), formatDateLabel(f.since * 1000)));
    const groupsVal = document.createElement("b");
    groupsVal.textContent = "…";
    facts.appendChild(kv(t("profile.common_groups"), groupsVal));
    const meshRow = document.createElement("div");
    meshRow.hidden = true;
    facts.appendChild(meshRow);
    const keyVal = document.createElement("span");
    keyVal.className = "mono muted";
    keyVal.textContent = "ed25519 · " + peerId.substring(0, 4) + "…" + peerId.substring(peerId.length - 4);
    facts.appendChild(kv(t("profile.key"), keyVal));
    el.body.appendChild(facts);

    // Звонки
    const calls = card("calls");
    calls.innerHTML = '<div class="profile-calls-head"><span class="profile-h3"></span><button type="button" class="profile-link"></button></div><div class="profile-calls-list"></div>';
    calls.querySelector(".profile-h3").textContent = t("profile.calls");
    const allBtn = calls.querySelector(".profile-link");
    allBtn.textContent = t("profile.calls_all");
    allBtn.hidden = true;
    el.body.appendChild(calls);

    // «Уведомления от этого контакта» (фаза 7a, макет Contacts): вкл. = не заглушён
    const notifyRow = document.createElement("div");
    notifyRow.className = "profile-toggle-row";
    const notifyLbl = document.createElement("span");
    notifyLbl.textContent = t("profile.notify_contact");
    const notifyTg = document.createElement("span");
    notifyTg.className = "ip-toggle";
    notifyRow.append(notifyLbl, notifyTg);
    const nKey = peerKey(peerId);
    notifySync = () => { if (notifyTg.isConnected) notifyTg.classList.toggle("on", !isMuted(nKey)); };
    notifyRow.addEventListener("click", () => toggleMuted(nKey));
    el.body.appendChild(notifyRow);
    notifySync(); // после вставки: isConnected до неё false

    // Опасные действия
    const danger = document.createElement("div");
    danger.className = "profile-danger";
    danger.appendChild(btn(t("profile.remove_friend"), "user-minus", "dng", () => askRemoveFriend(peerId, nick)));
    danger.appendChild(btn(t("profile.delete_chat"), "trash-2", "dng", () => {
        confirmDialog(t("profile.delete_chat"), t("profile.confirm_delete").replace("{name}", nick || peerId.substring(0, 8)), t("profile.delete_chat"), () => {
            svc("DeleteConversation", peerId).then(() => { refreshPeers(); showSuccess(t("profile.chat_deleted")); })
                .catch((err) => showError(String(err && err.message || err)));
        });
    }));
    el.body.appendChild(danger);
    if (window.lucide) lucide.createIcons({ nodes: [el.body] });

    if (confirmRemove) askRemoveFriend(peerId, nick);

    // Асинхронные части
    loadCommonGroups(peerId).then((names) => {
        if (gen !== renderGen) return;
        groupsVal.textContent = names.length ? names.join(", ") : t("profile.none");
    });
    loadMeshRow(peerId).then((node) => {
        if (gen !== renderGen || !node) return;
        meshRow.replaceWith(node);
        if (window.lucide) lucide.createIcons({ nodes: [node] });
    });
    loadCalls(peerId).then((items) => {
        if (gen !== renderGen) return;
        renderCalls(calls, items, allBtn);
    });
}

function askRemoveFriend(peerId, nick) {
    confirmDialog(t("profile.remove_friend"), t("profile.confirm_remove").replace("{name}", nick || peerId.substring(0, 8)), t("profile.remove_friend"), () => {
        svc("RemoveFriend", peerId).then(() => {
            refreshPeers();
            document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
            document.dispatchEvent(new CustomEvent("vox:ui-contacts-clear-selection"));
            hide();
        }).catch((err) => showError(String(err && err.message || err)));
    });
}

// loadCommonGroupsFull — общие с другом группы (объекты из state.groups),
// по имени. Профиль показывает имена, правая панель — ссылки на группы.
export async function loadCommonGroupsFull(peerId) {
    const found = [];
    // type 0 — текстовые группы, 1 — голосовые каналы; type 2 — служебные группы звонков, их не показываем
    const groups = (state.groups || []).filter((g) => !g.left && (g.type === 0 || g.type === 1 || !g.type));
    await Promise.all(groups.map(async (g) => {
        try {
            const members = (await svc("GetGroupMembers", g.groupId)) || [];
            if (members.some((m) => m.peerId === peerId)) found.push(g);
        } catch (err) { /* группа недоступна */ }
    }));
    return found.sort((a, b) => a.name.localeCompare(b.name));
}

async function loadCommonGroups(peerId) {
    return (await loadCommonGroupsFull(peerId)).map((g) => g.name);
}

// loadMeshNodeNum — nodeNum Mesh-узла друга; null, если Meshtastic выключен.
export async function loadMeshNodeNum(peerId) {
    try { const st = await svc("Status"); if (!(st && st.enabled)) return null; } catch (err) { return null; }
    try {
        const m = await svc("GetPeerMeshNode", peerId);
        return (m && (m.nodeNum || m.NodeNum)) || 0;
    } catch (err) { return 0; }
}

async function loadMeshRow(peerId) {
    const nodeNum = await loadMeshNodeNum(peerId);
    if (nodeNum === null) return null;
    return kv(t("profile.mesh_node"), buildMeshNodeEditor(peerId, nodeNum));
}

// buildMeshNodeEditor — «!acaaba94 ✎»: карандаш превращает значение в поле,
// Enter/blur сохраняет (SetPeerMeshNode), Esc отменяет. onSaved(n) — после
// сохранения (правая панель переключает кнопку «Mesh» в строке ввода).
export function buildMeshNodeEditor(peerId, nodeNum, onSaved) {
    const val = document.createElement("span");
    val.className = "profile-mesh";
    const text = document.createElement("span");
    text.className = "mono";
    text.textContent = nodeNum ? "!" + nodeNum.toString(16).padStart(8, "0") : t("profile.none");
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "profile-icon-btn";
    edit.title = t("info.mesh_save");
    edit.innerHTML = '<i data-lucide="pencil" class="icon-14"></i>';
    edit.addEventListener("click", () => {
        const input = document.createElement("input");
        input.type = "text";
        input.className = "profile-mesh-input mono";
        input.placeholder = "!acaaba94";
        input.maxLength = 9;
        input.value = nodeNum ? "!" + nodeNum.toString(16).padStart(8, "0") : "";
        text.replaceWith(input);
        input.focus();
        const commit = async () => {
            const v = input.value.trim().replace(/^!/, "");
            const n = v ? parseInt(v, 16) : 0;
            if (v && (isNaN(n) || n <= 0)) { showError(t("info.mesh_hint")); return; }
            try {
                await svc("SetPeerMeshNode", peerId, n);
                nodeNum = n;
                text.textContent = n ? "!" + n.toString(16).padStart(8, "0") : t("profile.none");
                input.replaceWith(text);
                if (onSaved) onSaved(n);
            } catch (err) { showError(String(err && err.message || err)); }
        };
        input.addEventListener("keydown", (e) => { if (e.key === "Enter") commit(); if (e.key === "Escape") input.replaceWith(text); });
        input.addEventListener("blur", commit);
    });
    val.appendChild(text);
    val.appendChild(edit);
    return val;
}

export async function loadCalls(peerId) {
    try {
        const items = (await svc("GetCallHistory")) || [];
        return items.filter((x) => x.peerId === peerId);
    } catch (err) {
        return [];
    }
}

// renderCalls — строки звонков в .profile-calls-list внутри cardEl; allBtn —
// «Все звонки» / «Свернуть» (правая панель тоже его использует).
export function renderCalls(cardEl, items, allBtn) {
    const list = cardEl.querySelector(".profile-calls-list");
    list.innerHTML = "";
    if (items.length === 0) {
        list.innerHTML = '<div class="profile-calls-empty">' + escapeHtml(t("profile.calls_empty")) + "</div>";
        return;
    }
    let expanded = false;
    const draw = () => {
        list.innerHTML = "";
        const shown = expanded ? items : items.slice(0, 5);
        shown.forEach((it) => {
            const row = document.createElement("div");
            row.className = "profile-call-row";
            let icon = it.outbound ? "phone-outgoing" : "phone-incoming";
            let label = it.outbound ? t("calls.outgoing") : t("calls.incoming");
            let cls = "";
            if (it.status === "missed") { icon = "phone-missed"; label = t("calls.missed"); cls = " missed"; }
            const dur = it.duration > 0 ? formatMmSs(it.duration * 1000) : "";
            row.innerHTML = '<i data-lucide="' + icon + '" class="icon-16' + cls + '"></i><span class="profile-call-kind"></span><span class="profile-call-when"></span><span class="profile-call-dur"></span>';
            row.querySelector(".profile-call-kind").textContent = label;
            row.querySelector(".profile-call-when").textContent = formatDateLabel(it.createdAt * 1000) + " · " + formatTime(it.createdAt * 1000);
            row.querySelector(".profile-call-dur").textContent = dur;
            list.appendChild(row);
        });
        if (expanded) {
            const clear = btn(t("profile.calls_clear"), "trash-2", "dng small", () => {
                confirmDialog(t("profile.calls_clear"), t("profile.calls_clear_confirm"), t("profile.calls_clear"), () => {
                    svc("ClearCallHistory").then(() => { items = []; expanded = false; allBtn.hidden = true; draw(); })
                        .catch((err) => showError(String(err && err.message || err)));
                });
            });
            list.appendChild(clear);
        }
        if (window.lucide) lucide.createIcons({ nodes: [list] });
    };
    allBtn.hidden = items.length <= 5;
    allBtn.onclick = () => { expanded = !expanded; allBtn.textContent = expanded ? t("profile.calls_less") : t("profile.calls_all"); draw(); };
    draw();
}

// --- карточка заявки ---

function renderRequest(r, gen) {
    const when = r.timestamp ? formatTime(r.timestamp) : "";
    const sub = r.direction === 0
        ? t("contacts.wants").replace("{t}", when)
        : t("profile.request_sent_you").replace("{t}", when);
    const head = identity(r.peerId, r.nickname, escapeHtml(sub));
    const groupsLine = document.createElement("div");
    groupsLine.className = "profile-status muted";
    head.appendChild(groupsLine);
    if (r.direction === 0) {
        const actions = document.createElement("div");
        actions.className = "profile-actions";
        actions.appendChild(btn(t("profile.accept"), "check", "pri", async () => {
            try {
                await svc("AcceptFriendRequest", r.id);
                refreshPeers();
                document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
                document.dispatchEvent(new CustomEvent("vox:ui-contacts-clear-selection"));
                show({ kind: "friend", friend: { peerId: r.peerId, nickname: r.nickname, online: false } });
            } catch (err) { showError(String(err && err.message || err)); }
        }));
        actions.appendChild(btn(t("profile.decline"), "x", "sec", async () => {
            try {
                await svc("RejectFriendRequest", r.id);
                document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
                document.dispatchEvent(new CustomEvent("vox:ui-contacts-clear-selection"));
                hide();
            } catch (err) { showError(String(err && err.message || err)); }
        }));
        head.appendChild(actions);
        const hint = document.createElement("div");
        hint.className = "profile-desc";
        hint.textContent = t("profile.decline_hint");
        head.appendChild(hint);
    }
    el.body.appendChild(head);
    if (window.lucide) lucide.createIcons({ nodes: [el.body] });
    loadCommonGroups(r.peerId).then((names) => {
        if (gen !== renderGen) return;
        groupsLine.textContent = t("profile.common_groups") + ": " + (names.length ? names.join(", ") : t("profile.none"));
    });
}

// --- поиск по тегу ---

async function renderTag(tag, gen) {
    const c = card("tag");
    el.body.appendChild(c);
    if (!tag) {
        c.innerHTML = '<div class="profile-desc"></div>';
        c.querySelector(".profile-desc").textContent = t("profile.tag_hint");
        return;
    }
    c.innerHTML = '<div class="profile-desc"></div>';
    c.querySelector(".profile-desc").textContent = t("contacts.find.searching");
    let result = null, error = "";
    try {
        result = await svc("SearchUser", tag);
    } catch (err) {
        const msg = (err && err.message) || String(err);
        error = msg.indexOf("your own tag") >= 0 ? t("contacts.find.self_error") : msg;
    }
    if (gen !== renderGen) return;
    c.innerHTML = "";
    if (error || !result || !result.found) {
        c.innerHTML = '<div class="profile-desc"></div>';
        c.querySelector(".profile-desc").textContent = error || t("contacts.find.not_found");
        return;
    }
    c.classList.add("found");
    const av = document.createElement("div");
    av.className = "contact-avatar";
    applyAvatar(av, result.peerId, result.nickname || result.shortId);
    const info = document.createElement("div");
    info.className = "profile-tag-info";
    info.innerHTML = '<div class="contact-name"></div><div class="profile-desc mono"></div>';
    info.querySelector(".contact-name").textContent = result.nickname || result.shortId;
    info.querySelector(".profile-desc").textContent = tag + " · " + t("profile.found_by_tag");
    const send = btn(t("profile.send_request"), "user-plus", "pri", async () => {
        send.disabled = true;
        try {
            await svc("SendFriendRequest", result.peerId);
            send.querySelector("span").textContent = t("contacts.find.sent");
            document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
        } catch (err) {
            send.disabled = false;
            showError(String(err && err.message || err));
        }
    });
    c.appendChild(av);
    c.appendChild(info);
    c.appendChild(send);
    if (window.lucide) lucide.createIcons({ nodes: [c] });
}

export function initProfile() {
    el.view = q("profile-view");
    el.body = q("profile-body");
    if (!el.view) return;
    document.addEventListener("vox:ui-profile-show", (e) => show(e.detail || {}));
    document.addEventListener("vox:ui-profile-hide", hide);
}
