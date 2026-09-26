"use strict";

// chat-menu.js — меню «…» в шапке чата и по правому клику на шапке (макет
// HeaderMenus). Правило макета: в шапке три кнопки-действия (поиск,
// позвонить, инфо), всё остальное — здесь. Пункты с меткой «новое», которых
// нет в коде (закрепить, цвет чата, экспорт, заблокировать), не показываем.
// Меню «Ещё» правой панели открывает то же самое (vox:ui-chat-menu).

import { svc, state, dom } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { refreshPeers } from "./contacts-list.js";
import { openSearch } from "./chat-search.js";
import { showRenameGroupDialog } from "./messages.js";
import { currentChatMuteKey, isMuted, toggleMuted } from "./mute.js";
import { confirmDeleteChat, groupDangerItems, showAddMemberDialog } from "./info-panel.js";
import { showSuccess, showError } from "./toast.js";
import { confirmDialog } from "./ui.js";
import { isPinned, togglePinned, chatColorName, showChatColorMenu } from "./chat-prefs.js";
import { isBlocked, blockPeer, unblockPeer } from "./blocked.js";

// «Очистить историю» (фаза 7a, макет HeaderMenus): сообщения и вложения —
// только у меня, чат остаётся в списке.
function clearHistory(pid, name) {
    confirmDialog(t("menu.clear_history"), t("confirm.clear_history").replace("{name}", name), t("menu.clear_history"), function () {
        svc("ClearChatMedia", pid, "").catch(function () {}).then(function () {
            return svc("DeleteConversation", pid);
        }).then(function () {
            if (state.selectedPeerID === pid) dom.messagesDiv.innerHTML = "";
            delete state.lastMessages[pid];
            refreshPeers();
            showSuccess(t("menu.history_cleared"));
        }).catch(function (err) { showError(String(err && err.message || err)); });
    }, { danger: true });
}

function openPanel(tab) {
    document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: tab } }));
}

function copy(text) {
    try { navigator.clipboard.writeText(text); showSuccess(t("info.copied")); } catch (e) { /* окно без фокуса */ }
}

function currentGroup() {
    for (var i = 0; i < state.groups.length; i++) {
        if (state.groups[i].groupId === state.selectedGroupID) return state.groups[i];
    }
    return null;
}

// Тумблер «Голосовой канал» — переехал сюда из блока настроек панели (макет).
function setVoiceEnabled(g, on) {
    svc("SetVoiceEnabled", g.groupId, on).then(function () {
        var b = document.getElementById("voice-join-btn");
        if (b && state.selectedGroupID === g.groupId) b.style.display = on ? "" : "none";
        return refreshPeers();
    }).then(function () {
        document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    }).catch(function (err) {
        showError(String(err && err.message || err));
    });
}

export function buildChatMenu() {
    var items = [];
    var muteKey = currentChatMuteKey();
    var search = { label: t("menu.search"), icon: "search", hint: "Ctrl+F", action: function () { openSearch(); } };
    var media = { label: t("menu.media"), icon: "layout-grid", action: function () { openPanel("media"); } };
    var mute = { label: t("menu.mute"), icon: "bell-off", toggle: isMuted(muteKey), action: function () { toggleMuted(muteKey); } };
    var pin = { label: t("menu.pin_chat"), icon: "pin", toggle: isPinned(muteKey), action: function () { togglePinned(muteKey); } };

    if (state.selectedGroupID) {
        var g = currentGroup();
        if (!g) return items;
        var owner = g.creatorId === state.myID && !g.left;
        items.push({ label: t("menu.about_group"), icon: "info", action: function () { openPanel("info"); } });
        items.push(search, media, { separator: true });
        if (owner) {
            items.push({ label: t("info.add_member"), icon: "user-plus", action: function () { showAddMemberDialog(g.groupId); } });
            items.push({ label: t("ctx.rename_group"), icon: "pencil", action: function () { showRenameGroupDialog(g.groupId, g.name); } });
        }
        items.push({ label: t("ctx.copy_group_id"), icon: "copy", action: function () { copy(g.groupId); } });
        items.push({ separator: true }, mute, pin);
        if (owner) {
            items.push({ label: t("menu.voice_channel"), icon: "audio-lines", toggle: !!g.voiceEnabled, action: function () { setVoiceEnabled(g, !g.voiceEnabled); } });
        }
        items.push({ separator: true });
        return items.concat(groupDangerItems(g));
    }

    var pid = state.selectedPeerID;
    if (!pid || String(pid).indexOf("__mesh:") === 0) return items;
    var p = state.selectedPeerInfo;
    var nick = p ? (p.nickname || "") : "";
    var code = (p && p.shortId) || pid.substring(0, 16);
    items.push({ label: t("menu.profile"), icon: "user", action: function () { openPanel("info"); } });
    var color = { label: t("menu.chat_color"), icon: "palette", sub: chatColorName(muteKey), action: function () { showChatColorMenu(null, muteKey); } };
    items.push(search, media, { separator: true }, mute, pin, color, { separator: true });
    items.push({ label: t("ctx.copy_tag"), icon: "copy", action: function () { copy((nick || "user") + "#" + code); } });
    items.push({ separator: true });
    items.push({ label: t("menu.clear_history"), icon: "trash-2", danger: true, action: function () { clearHistory(pid, nick || code); } });
    // Блокировка (фаза 7b): полный игнор, дружба и история сохраняются
    items.push(isBlocked(pid)
        ? { label: t("block.unblock"), icon: "user-check", action: function () { unblockPeer(pid); } }
        : { label: t("block.block"), icon: "user-minus", danger: true, action: function () { blockPeer(pid, nick || code); } });
    return items;
}

// showChatMenu — у курсора (правый клик) или под кнопкой, правым краем к ней.
export function showChatMenu(e, anchor) {
    var items = buildChatMenu();
    if (!items.length) return;
    if (!anchor) { showContextMenu(e, items); return; }
    var r = anchor.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.bottom + 2, preventDefault: function () {}, stopPropagation: function () {} }, items);
    var menu = document.getElementById("context-menu");
    if (menu) menu.style.left = Math.max(4, r.right - menu.offsetWidth) + "px";
}

var moreBtn = document.getElementById("chat-more-btn");
if (moreBtn) {
    moreBtn.addEventListener("click", function (e) {
        e.stopPropagation(); // иначе document-click в contextmenu.js сразу закроет меню
        showChatMenu(null, moreBtn);
    });
}

document.addEventListener("vox:ui-chat-menu", function (e) {
    showChatMenu(null, e.detail && e.detail.anchor);
});

// Правый клик по шапке — то же меню (раньше там был только «Копировать ID»).
if (dom.chatHeader) {
    dom.chatHeader.addEventListener("contextmenu", function (e) {
        if (e.target.closest && e.target.closest(".chat-search")) return; // в поле поиска — системное меню
        var items = buildChatMenu();
        if (!items.length) return;
        showContextMenu(e, items);
    });
}
