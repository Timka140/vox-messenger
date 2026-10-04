"use strict";

// contacts-list.js — Sidebar contact/group/voice channel lists.
// Manages viewMode, list rendering, peer/group selection, and refresh polling.

import { renderLastMessagePreview } from "./callfmt.js";
import { svc, flog, state, dom, colorFromId, formatTime } from "./core.js";
import { joinVoice, leaveVoice } from "./voice.js";
import { showContextMenu } from "./contextmenu.js";
import { buildGroupContextMenu } from "./ui.js";
import { t } from "./i18n.js";
import { updateVoiceHeaderBtn } from "./events.js";
import { renderUserBar, endCall, startCall } from "./calls.js";
import { isMuted, peerKey, groupKey, toggleMuted } from "./mute.js";
import { applyAvatar } from "./avatar.js";
import { emptyState } from "./ui.js";
import { showSuccess } from "./toast.js";
import { isInviteLink, renderJoinCard, removeJoinCard } from "./group-join.js";
import { isPinned, pinRank, togglePinned } from "./chat-prefs.js";
import { isBlocked } from "./blocked.js";

// copyMyContactLink — «Скопировать мою ссылку» из пустого списка (макет
// EmptyStates «Первый запуск · нет друзей»).
function copyMyContactLink() {
    svc("GetMyContactURI").then(function (uri) {
        try { navigator.clipboard.writeText(uri); showSuccess(t("info.copied")); } catch (e) { /* окно без фокуса */ }
    }).catch(function () {});
}

// Значок «Без звука» у имени чата в списке (mute.js).
function mutedIcon() {
    var i = document.createElement("i");
    i.setAttribute("data-lucide", "bell-off");
    i.className = "icon-12 contact-muted";
    return i;
}
document.addEventListener("vox:ui-mute-changed", function () {
    if (viewMode === "messages" || viewMode === "groups") updateContacts(state.peers);
});

// Значок «Закреплён» у имени (фаза 7a, chat-prefs.js): закреплённые — сверху.
function pinnedIcon() {
    var i = document.createElement("i");
    i.setAttribute("data-lucide", "pin");
    i.className = "icon-12 contact-pinned";
    return i;
}
document.addEventListener("vox:ui-chat-prefs-changed", function () {
    if (viewMode === "messages" || viewMode === "groups") updateContacts(state.peers);
});

// Значок «Заблокирован» у имени (фаза 7b, blocked.js).
function blockedIcon() {
    var i = document.createElement("i");
    i.setAttribute("data-lucide", "ban");
    i.className = "icon-12 contact-blocked";
    return i;
}
document.addEventListener("vox:ui-blocked-changed", function () {
    if (viewMode === "messages") updateContacts(state.peers);
});

// byPinned — сначала закреплённые в порядке закрепления, 0 — оба не закреплены.
function byPinned(ka, kb) {
    var pa = pinRank(ka), pb = pinRank(kb);
    if (pa === pb) return 0;
    if (pa < 0) return 1;
    if (pb < 0) return -1;
    return pa - pb;
}

var viewMode = "messages"; // "messages" | "groups" | "voice" | "mesh" | "contacts"

// --- Public: switch views ---

// All non-mesh nav helpers dispatch vox:close-mesh-chat so mesh-chat-view
// (which lives inside #chat-area as a sibling that swaps with the regular
// chat) doesn't remain stuck on top after the user switches tabs.
// leaveContactsMode — уход из режима «Контакты»: спрятать профиль в центре
// (profile.js) и вернуть шапке панели обычные контролы (contacts.js).
function leaveContactsMode() {
    if (viewMode !== "contacts") return;
    document.dispatchEvent(new CustomEvent("vox:ui-profile-hide"));
    document.dispatchEvent(new CustomEvent("vox:ui-contacts-leave"));
}

export function showGroupsList() {
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    leaveContactsMode();
    viewMode = "groups";
    renderGroupsList();
}

export function showMessagesList() {
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    leaveContactsMode();
    viewMode = "messages";
    updateContacts(state.peers);
}

export function showVoiceChannels() {
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    leaveContactsMode();
    viewMode = "voice";
    renderVoiceChannels();
}

export function showMeshList() {
    leaveContactsMode();
    viewMode = "mesh";
    document.dispatchEvent(new CustomEvent("vox:render-mesh-list"));
}

// showContactsMode — «Контакты» как режим левой панели (макет Contacts):
// список рендерит contacts.js по vox:ui-contacts-render, центр — profile.js.
export function showContactsMode() {
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    viewMode = "contacts";
    document.dispatchEvent(new CustomEvent("vox:ui-contacts-render"));
}

export function getViewMode() {
    return viewMode;
}

// --- Refresh from backend ---

var _refreshTimer = null;
var _refreshResolves = [];
// Generation-guard для renderVoiceChannels: функция async с await'ами внутри
// цикла и appendChild между ними. Два параллельных рендера (updateContacts на
// каждое событие + клик) без счётчика перемешивали каналы/участников (второй
// чистит список, первый продолжает appendChild). Устаревший рендер выходит.
var _voiceChannelsGen = 0;

export function refreshPeers() {
    return new Promise(function (resolve) {
        _refreshResolves.push(resolve);
        if (_refreshTimer) clearTimeout(_refreshTimer);
        _refreshTimer = setTimeout(function () {
            _refreshTimer = null;
            var resolves = _refreshResolves;
            _refreshResolves = [];
            _doRefreshPeers().then(function () {
                for (var i = 0; i < resolves.length; i++) resolves[i]();
            }).catch(function () {
                for (var i = 0; i < resolves.length; i++) resolves[i]();
            });
        }, 150);
    });
}

async function _doRefreshPeers() {
    try {
        var conversations = await svc("GetConversations");
        var groupList = await svc("GetGroups");
        state.groups = groupList || [];
        state.peers = (conversations || []).map(function (c) {
            return { id: c.id, shortId: c.shortId, nickname: c.nickname, online: c.online, unread: c.unread || 0 };
        });
        for (var ci = 0; ci < (conversations || []).length; ci++) {
            var conv = conversations[ci];
            if (conv.lastMessage) {
                state.lastMessages[conv.id] = conv.lastMessage;
            }
        }

        // Логируем состояние UI
        var totalDmUnread = 0, onlineCount = 0;
        for (var si = 0; si < state.peers.length; si++) {
            totalDmUnread += state.peers[si].unread;
            if (state.peers[si].online) onlineCount++;
        }
        var totalGroupUnread = 0;
        for (var gi = 0; gi < state.groups.length; gi++) {
            totalGroupUnread += (state.groups[gi].unread || 0);
        }
        flog("DBG", "frontend.state", "refreshPeers: peers=" + state.peers.length +
            " online=" + onlineCount +
            " dm_unread=" + totalDmUnread +
            " groups=" + state.groups.length +
            " group_unread=" + totalGroupUnread +
            " selected_peer=" + (state.selectedPeerID ? state.selectedPeerID.substring(0, 16) : "none") +
            " selected_group=" + (state.selectedGroupID ? state.selectedGroupID.substring(0, 8) : "none"));

        // Бейджи nav (макет NavMap): «Чаты» — непрочитанные личные (в т.ч.
        // пропущенные звонки), «Группы» — непрочитанные в группах.
        setNavBadge("chats-nav-badge", totalDmUnread);
        setNavBadge("groups-nav-badge", totalGroupUnread);

        updateContacts(state.peers);
        updateGroupHeader();
    } catch (err) {
        // Service may not be ready yet
    }
}

function setNavBadge(id, n) {
    var badge = document.getElementById(id);
    if (!badge) return;
    badge.textContent = n > 99 ? "99+" : String(n);
    badge.hidden = n === 0;
}

// Polling убран — refreshPeers() вызывается из событий (events.js)
// и при подключении к серверу (setup.js)

// --- Select peer (DM) ---

// syncChatPeerStatus — статус в заголовке DM-чата из state.selectedPeerInfo.
// Единый источник правды — state.peers (online = IsP2PConnected из Go), тот
// же, что у зелёной точки в списке и панели Info. Вызывается при выборе пира
// и при каждом updateContacts, чтобы события vox:peer-connected/disconnected
// (сигналка ИЛИ P2P) не оставляли заголовок в состоянии, противоречащем списку.
export function syncChatPeerStatus() {
    if (!state.selectedPeerID || state.selectedGroupID || !dom.chatPeerStatus) return;
    var isOnline = !!(state.selectedPeerInfo && state.selectedPeerInfo.online);
    dom.chatPeerStatus.innerHTML = '<span class="status-dot ' + (isOnline ? "online" : "offline") + '"></span> ' + (isOnline ? t("chat.online") : t("chat.offline"));
}

// selectPeer and selectGroup need renderMessages + appendMessage from messages.js.
// Import lazily to avoid circular deps.
var _renderMessages = null;
var _cancelEdit = null;
var _refreshInfoPanel = null;
var _updateInfoTabs = null;
export function setRenderMessages(fn) { _renderMessages = fn; }
export function setCancelEdit(fn) { _cancelEdit = fn; }
export function setUpdateInfoTabs(fn) { _updateInfoTabs = fn; }
export function setRefreshInfoPanel(fn) { _refreshInfoPanel = fn; }

export async function selectPeer(peerID, peerObj) {
    if (_cancelEdit) _cancelEdit();
    document.dispatchEvent(new CustomEvent("vox:ui-chat-changed")); // поиск, выбор, вложения в строке ввода
    // Pseudo-peer for mesh: ID format "__mesh:0xNNN__" (or "__mesh:bcast__")
    // routes to per-node mesh chat instead of normal DM.
    if (typeof peerID === "string" && peerID.indexOf("__mesh:") === 0) {
        var items = dom.contactsList.querySelectorAll(".contact-item");
        for (var k = 0; k < items.length; k++) {
            items[k].classList.toggle("active", items[k].dataset.peerId === peerID);
        }
        state.selectedPeerID = peerID;
        state.selectedPeerInfo = null;
        state.selectedGroupID = null;
        document.dispatchEvent(new CustomEvent("vox:open-mesh-chat", { detail: { peerID: peerID } }));
        return;
    }
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    flog("INF", "frontend.ui", "selectPeer" +
        " id=" + peerID.substring(0, 16) +
        " online=" + (peerObj ? peerObj.online : "?") +
        " unread=" + (peerObj ? peerObj.unread : "?"));
    dom.messagesDiv.innerHTML = "";
    // Вызывающие не всегда передают полный объект (contacts.js → «Send
    // message» даёт только id/nickname, без online) — заголовок тогда писал
    // Offline при зелёной точке в списке. Достраиваем из state.peers.
    var known = null;
    for (var ki = 0; ki < state.peers.length; ki++) {
        if (state.peers[ki].id === peerID) { known = state.peers[ki]; break; }
    }
    if (known) peerObj = Object.assign({}, known, peerObj || {}, { online: known.online, unread: known.unread });
    state.selectedPeerID = peerID;
    state.selectedPeerInfo = peerObj || null;
    state.selectedGroupID = null;

    var items = dom.contactsList.querySelectorAll(".contact-item");
    for (var i = 0; i < items.length; i++) {
        items[i].classList.toggle("active",
            items[i].dataset.peerId === peerID && !items[i].dataset.groupId);
    }

    dom.chatHeader.style.display = "flex";
    dom.inputArea.style.display = "block";
    var displayName = peerObj ? (peerObj.nickname || peerObj.shortId) : peerID.substring(0, 16);
    dom.chatPeerName.textContent = displayName;
    syncChatPeerStatus();

    // Скрыть voice button для DM, показать call button
    var voiceBtn = document.getElementById("voice-join-btn");
    if (voiceBtn) voiceBtn.style.display = "none";
    var callBtn = document.getElementById("call-btn");
    if (callBtn) callBtn.style.display = "";

    document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    if (_updateInfoTabs) _updateInfoTabs();
    document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh")); // stage.js: показать/скрыть сцену

    try {
        var msgs = await svc("GetMessages", peerID);
        if (state.selectedPeerID !== peerID) return; // peer switched during await
        if (_renderMessages) _renderMessages(msgs, { unread: (peerObj && peerObj.unread) || 0 });
        await svc("MarkMessagesRead", peerID);
        refreshPeers();
    } catch (err) {
        console.error("getMessages:", err);
    }
}

// --- Select group ---

export async function selectGroup(groupID, groupObj) {
    if (_cancelEdit) _cancelEdit();
    document.dispatchEvent(new CustomEvent("vox:ui-chat-changed"));
    document.dispatchEvent(new CustomEvent("vox:close-mesh-chat"));
    flog("INF", "frontend.ui", "selectGroup" +
        " id=" + groupID.substring(0, 8) +
        " members=" + (groupObj ? groupObj.members : "?") +
        " unread=" + (groupObj ? groupObj.unread : "?"));
    dom.messagesDiv.innerHTML = "";
    state.selectedGroupID = groupID;
    state.selectedPeerID = null;
    state.selectedPeerInfo = null;

    var items = dom.contactsList.querySelectorAll(".contact-item");
    for (var i = 0; i < items.length; i++) {
        items[i].classList.toggle("active", items[i].dataset.groupId === groupID);
    }

    dom.chatHeader.style.display = "flex";
    dom.inputArea.style.display = (groupObj && groupObj.left) ? "none" : "block";
    dom.chatPeerName.textContent = groupObj ? groupObj.name : "Group";
    var memberCount = groupObj ? groupObj.members : 0;
    dom.chatPeerStatus.innerHTML = '<i data-lucide="users" class="icon-14" style="margin-right:4px;"></i> ' + memberCount + ' ' + t("chat.members");
    if (window.lucide) lucide.createIcons({ nodes: [dom.chatPeerStatus] });

    // Показать/скрыть voice button, скрыть call button
    var voiceBtn = document.getElementById("voice-join-btn");
    if (voiceBtn) {
        voiceBtn.style.display = (groupObj && groupObj.voiceEnabled && !groupObj.left) ? "" : "none";
        updateVoiceHeaderBtn();
    }
    var callBtn = document.getElementById("call-btn");
    if (callBtn) callBtn.style.display = "none";

    document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    if (_updateInfoTabs) _updateInfoTabs();
    document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh")); // stage.js: показать/скрыть сцену

    try {
        var msgs = await svc("GetGroupMessages", groupID);
        if (state.selectedGroupID !== groupID) return; // group switched during await
        if (_renderMessages) _renderMessages(msgs, { unread: (groupObj && groupObj.unread) || 0 });
        await svc("MarkGroupRead", groupID).catch(function (e) { flog("ERR", "contacts-list", "MarkGroupRead: " + e); });
        refreshPeers();
    } catch (err) {
        console.error("getGroupMessages:", err);
    }
}

// --- Update group header member count ---

// deselectChat — снять выделение чата (пустая область). Нужен ad-hoc
// групповому звонку без чата-владельца: «К звонку» из полоски ведёт сюда,
// и сцена (stage.js) занимает пустую область. Паттерн — как vox:group-deleted.
export function deselectChat() {
    document.dispatchEvent(new CustomEvent("vox:ui-chat-changed"));
    state.selectedPeerID = null;
    state.selectedPeerInfo = null;
    state.selectedGroupID = null;
    var items = dom.contactsList.querySelectorAll(".contact-item");
    for (var i = 0; i < items.length; i++) items[i].classList.remove("active");
    dom.messagesDiv.innerHTML = "";
    dom.chatHeader.style.display = "none";
    dom.inputArea.style.display = "none";
    document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
    if (_updateInfoTabs) _updateInfoTabs();
    document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
}
document.addEventListener("vox:ui-deselect-chat", deselectChat);

export function updateGroupHeader() {
    if (!state.selectedGroupID) return;
    for (var i = 0; i < state.groups.length; i++) {
        if (state.groups[i].groupId === state.selectedGroupID) {
            var g = state.groups[i];
            dom.chatPeerName.textContent = g.name;
            dom.chatPeerStatus.innerHTML = '<i data-lucide="users" class="icon-14" style="margin-right:4px;"></i> ' + g.members + ' ' + t("chat.members") +
                '<div id="voice-participants" style="display:none;"></div>';
            if (window.lucide) lucide.createIcons({ nodes: [dom.chatPeerStatus] });
            // Обновить voice button при изменении voiceEnabled
            var voiceBtn = document.getElementById("voice-join-btn");
            if (voiceBtn) {
                voiceBtn.style.display = (g.voiceEnabled && !g.left) ? "" : "none";
            }
            return;
        }
    }
}

// --- Contacts list (Messages tab) ---

export function updateContacts(peerList) {
    state.peers = peerList || [];
    if (viewMode === "groups") {
        renderGroupsList();
        return;
    }
    if (viewMode === "voice") {
        renderVoiceChannels();
        return;
    }
    if (viewMode === "mesh") {
        document.dispatchEvent(new CustomEvent("vox:render-mesh-list"));
        return;
    }
    if (viewMode === "contacts") {
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-render"));
        return;
    }
    dom.contactsList.innerHTML = "";

    // Сортировка: свежие сообщения — выше, без сообщений — внизу
    state.peers.sort(function (a, b) {
        var pin = byPinned(peerKey(a.id), peerKey(b.id));
        if (pin !== 0) return pin;
        var ta = state.lastMessages[a.id] ? state.lastMessages[a.id].timestamp : 0;
        var tb = state.lastMessages[b.id] ? state.lastMessages[b.id].timestamp : 0;
        return tb - ta;
    });

    if (state.selectedPeerID) {
        for (var k = 0; k < state.peers.length; k++) {
            if (state.peers[k].id === state.selectedPeerID) {
                state.selectedPeerInfo = state.peers[k];
                break;
            }
        }
        syncChatPeerStatus();
    }

    if (state.peers.length === 0) {
        dom.contactsList.appendChild(emptyState("contact", t("contacts.empty_title"), t("contacts.empty_desc"),
            { label: t("contacts.empty_copy"), icon: "copy", onClick: copyMyContactLink }));
        // Карточка «Вступить» (ссылка в поиске) переживает перерисовку и пустой список.
        applySidebarFilter();
        return;
    }

    for (var i = 0; i < state.peers.length; i++) {
        var peer = state.peers[i];

        var item = document.createElement("div");
        item.className = "contact-item" + (peer.id === state.selectedPeerID ? " active" : "");
        item.dataset.peerId = peer.id;

        var avatar = document.createElement("div");
        avatar.className = "contact-avatar";
        var displayName = peer.nickname || peer.shortId;
        applyAvatar(avatar, peer.id, displayName);

        if (peer.online) {
            var dot = document.createElement("span");
            dot.className = "avatar-online-dot";
            avatar.appendChild(dot);
        }

        var body = document.createElement("div");
        body.className = "contact-body";

        var topRow = document.createElement("div");
        topRow.className = "contact-top-row";
        var nameSpan = document.createElement("span");
        nameSpan.className = "contact-name";
        nameSpan.textContent = displayName;
        var timeSpan = document.createElement("span");
        timeSpan.className = "contact-time";
        var lastMsg = state.lastMessages[peer.id];
        timeSpan.textContent = lastMsg ? formatTime(lastMsg.timestamp) : "";
        topRow.appendChild(nameSpan);
        if (isMuted(peerKey(peer.id))) topRow.appendChild(mutedIcon());
        if (isPinned(peerKey(peer.id))) topRow.appendChild(pinnedIcon());
        if (isBlocked(peer.id)) topRow.appendChild(blockedIcon());
        topRow.appendChild(timeSpan);

        var bottomRow = document.createElement("div");
        bottomRow.className = "contact-bottom-row";
        var preview = document.createElement("span");
        preview.className = "contact-preview";
        if (lastMsg) {
            renderLastMessagePreview(preview, lastMsg, false);
        } else {
            preview.textContent = peer.online ? t("chat.online") : "";
        }
        bottomRow.appendChild(preview);

        if (peer.unread > 0) {
            var badge = document.createElement("span");
            badge.className = "unread-badge";
            badge.textContent = peer.unread > 99 ? "99+" : peer.unread;
            bottomRow.appendChild(badge);
        }

        body.appendChild(topRow);
        body.appendChild(bottomRow);
        item.appendChild(avatar);
        item.appendChild(body);

        item.addEventListener("click", (function (p) {
            return function () { selectPeer(p.id, p); };
        })(peer));

        item.addEventListener("contextmenu", (function (p) {
            return function (e) {
                var tag = (p.nickname || "user") + "#" + p.id.substring(0, 16);
                var key = peerKey(p.id);
                // Макет ContextMenus «Чат в списке · личный»: профиль (правая
                // панель «Инфо»), позвонить, тег, без звука, удалить.
                showContextMenu(e, [
                    { label: t("menu.profile"), icon: "user", action: function () {
                        Promise.resolve(selectPeer(p.id, p)).then(function () {
                            document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "info" } }));
                        });
                    }},
                    { label: t("ctx.call"), icon: "phone", action: function () { startCall(p.id); } },
                    { label: t("ctx.copy_tag"), icon: "copy", action: function () {
                        navigator.clipboard.writeText(tag);
                    }},
                    { separator: true },
                    { label: t("menu.pin"), icon: "pin", toggle: isPinned(key), action: function () { togglePinned(key); } },
                    { label: t("menu.mute"), icon: "bell-off", toggle: isMuted(key), action: function () { toggleMuted(key); } },
                    { separator: true },
                    { label: t("ctx.delete_conversation"), icon: "trash-2", danger: true, action: function () {
                        svc("DeleteConversation", p.id).then(function () {
                            if (state.selectedPeerID === p.id) {
                                dom.messagesDiv.innerHTML = "";
                            }
                            delete state.lastMessages[p.id];
                            refreshPeers();
                        });
                    }}
                ]);
            };
        })(peer));

        dom.contactsList.appendChild(item);
    }
    if (window.lucide) lucide.createIcons({ nodes: [dom.contactsList] });
    applySidebarFilter();
}

// --- Groups list ---

var _showRenameGroupDialog = null;
export function setShowRenameGroupDialog(fn) { _showRenameGroupDialog = fn; }

// Диалог «Добавить участника» живёт в info-panel.js (он импортирует этот
// модуль) — ставится оттуда, чтобы не замыкать импорты по кругу. Без
// инициализатора: info-panel выполняется раньше этого модуля (круговой
// импорт), и «= null» здесь стирал уже поставленную функцию.
var _showAddMemberDialog;
export function setShowAddMemberDialog(fn) { _showAddMemberDialog = fn; }

// groupRowMenu — ПКМ по строке группы (макет ContextMenus «Чат в списке ·
// группа»): переименовать, добавить участника, ID; без звука; покинуть/удалить.
// «…» шапки и панель берут buildGroupContextMenu без этих добавок — там
// «Добавить участника» и «Без звука» уже есть своими пунктами.
function groupRowMenu(g) {
    var items = buildGroupContextMenu(g, state.myID, groupMenuCallbacks());
    if (g.left) return items;
    var owner = g.creatorId === state.myID;
    if (owner && _showAddMemberDialog) {
        items.splice(1, 0, { label: t("info.add_member"), icon: "user-plus", action: function () { _showAddMemberDialog(g.groupId); } });
    }
    var key = groupKey(g.groupId);
    var sep = items.findIndex(function (it) { return it.separator; });
    items.splice(sep + 1, 0,
        { label: t("menu.mute"), icon: "bell-off", toggle: isMuted(key), action: function () { toggleMuted(key); } },
        { separator: true });
    return items;
}

// groupMenuCallbacks — действия над группой для buildGroupContextMenu: одни и
// те же в списке групп, в меню «…» шапки и внизу правой панели (chat-menu.js,
// info-panel.js), чтобы пункты не расходились.
export function groupMenuCallbacks() {
    return {
        onRename: function (grp) { if (_showRenameGroupDialog) _showRenameGroupDialog(grp.groupId, grp.name); },
        onDelete: function (grp) { svc("DeleteGroupAsAdmin", grp.groupId).then(refreshPeers); },
        onLeave: function (grp) { svc("LeaveGroup", grp.groupId).then(refreshPeers); },
        onRejoin: function (grp) { svc("RejoinGroup", grp.groupId).then(refreshPeers); },
        onDeleteLeft: function (grp) { svc("DeleteLeftGroup", grp.groupId).then(refreshPeers); }
    };
}

function renderGroupsList() {
    dom.contactsList.innerHTML = "";

    var textGroups = state.groups.filter(function (g) { return !g.type || g.type === 0; });
    // Сортировка: свежие сообщения — выше, без сообщений — внизу
    textGroups.sort(function (a, b) {
        var pin = byPinned(groupKey(a.groupId), groupKey(b.groupId));
        if (pin !== 0) return pin;
        return (b.lastMessageAt || 0) - (a.lastMessageAt || 0);
    });

    for (var gi = 0; gi < textGroups.length; gi++) {
        var group = textGroups[gi];

        var gItem = document.createElement("div");
        gItem.className = "contact-item" + (group.groupId === state.selectedGroupID ? " active" : "");
        gItem.dataset.groupId = group.groupId;

        var gAvatar = document.createElement("div");
        gAvatar.className = "contact-avatar";
        gAvatar.innerHTML = '<i data-lucide="users" class="icon-18"></i>';
        // Иконка группы остаётся под фото: без фото видна она, как раньше.
        applyAvatar(gAvatar, "g:" + group.groupId, group.name, { keepContent: true });

        var gBody = document.createElement("div");
        gBody.className = "contact-body";

        var gTopRow = document.createElement("div");
        gTopRow.className = "contact-top-row";
        var gName = document.createElement("span");
        gName.className = "contact-name";
        gName.textContent = group.name;
        gTopRow.appendChild(gName);
        if (isMuted(groupKey(group.groupId))) gTopRow.appendChild(mutedIcon());
        if (isPinned(groupKey(group.groupId))) gTopRow.appendChild(pinnedIcon());

        var gBottomRow = document.createElement("div");
        gBottomRow.className = "contact-bottom-row";
        var gPreview = document.createElement("span");
        gPreview.className = "contact-preview";
        if (group.left) {
            gPreview.textContent = t("chat.group_left");
            gPreview.style.color = "var(--yellow-text)";
        } else if (group.lastMessage) {
            // Превью последнего сообщения (фаза 6c): «Ник: текст», голосовое, фото
            renderLastMessagePreview(gPreview, group.lastMessage, true);
        } else {
            gPreview.textContent = group.members + " " + t("chat.members");
        }
        gBottomRow.appendChild(gPreview);

        if (!group.left && group.unread > 0) {
            var gBadge = document.createElement("span");
            gBadge.className = "unread-badge";
            gBadge.textContent = group.unread > 99 ? "99+" : group.unread;
            gBottomRow.appendChild(gBadge);
        }

        gBody.appendChild(gTopRow);
        gBody.appendChild(gBottomRow);
        gItem.appendChild(gAvatar);
        gItem.appendChild(gBody);

        gItem.addEventListener("click", (function (g) {
            return function () { selectGroup(g.groupId, g); };
        })(group));

        gItem.addEventListener("contextmenu", (function (g) {
            return function (e) {
                showContextMenu(e, groupRowMenu(g));
            };
        })(group));

        dom.contactsList.appendChild(gItem);
    }

    // Empty-state как у контактов/voice-каналов: пустая панель без подсказки
    // выглядела как «не загрузилось».
    if (textGroups.length === 0) {
        dom.contactsList.appendChild(emptyState("users", t("groups.no_groups"), t("empty.groups_text")));
    }

    if (window.lucide) lucide.createIcons({ nodes: [dom.contactsList] });
    applySidebarFilter();
}

// --- Voice channels list ---

// Voice panel removed — user-bar now handles voice state via renderUserBar() in calls.js

async function renderVoiceChannels() {
    var gen = ++_voiceChannelsGen;
    dom.contactsList.innerHTML = "";

    var channels = state.groups.filter(function (g) { return g.type === 1; });

    var activeVoice = "";
    try { activeVoice = await svc("GetActiveVoiceGroup"); } catch (e) {}
    if (gen !== _voiceChannelsGen) return; // более свежий рендер уже идёт

    for (var i = 0; i < channels.length; i++) {
        var ch = channels[i];
        var item = document.createElement("div");
        item.className = "voice-channel-item" + (ch.groupId === activeVoice ? " active" : "");
        item.dataset.groupId = ch.groupId;

        var icon = document.createElement("div");
        icon.className = "voice-channel-icon";
        icon.innerHTML = '<i data-lucide="volume-2" class="icon-20"></i>';

        var body = document.createElement("div");
        body.className = "voice-channel-body";
        var nameEl = document.createElement("span");
        nameEl.className = "voice-channel-name";
        nameEl.textContent = ch.name;
        body.appendChild(nameEl);

        item.appendChild(icon);
        item.appendChild(body);

        item.addEventListener("click", (function (channel) {
            return async function () {
                if (state.call) {
                    await svc("LeaveCall").catch(function () {});
                    state.call = null;
                    endCall();
                }
                joinVoice(channel.groupId).then(function () {
                    renderVoiceChannels();
                    renderUserBar();
                });
            };
        })(ch));

        item.addEventListener("contextmenu", (function (g) {
            return function (e) {
                showContextMenu(e, groupRowMenu(g));
            };
        })(ch));

        dom.contactsList.appendChild(item);

        try {
            var participants = await svc("GetVoiceParticipants", ch.groupId);
            if (gen !== _voiceChannelsGen) return; // устаревший рендер — выходим
            if (participants && participants.length > 0) {
                var usersDiv = document.createElement("div");
                usersDiv.className = "voice-channel-users";
                for (var j = 0; j < participants.length; j++) {
                    var userEl = document.createElement("div");
                    userEl.className = "voice-channel-user";
                    // nickname приходит от недоверенного пира — только textContent,
                    // никакого innerHTML (иначе ник <img onerror> = XSS у всех).
                    var dot = document.createElement("span");
                    dot.className = "voice-dot";
                    userEl.appendChild(dot);
                    userEl.appendChild(document.createTextNode(
                        " " + (participants[j].nickname || participants[j].peerId.substring(0, 16))));
                    usersDiv.appendChild(userEl);
                }
                dom.contactsList.appendChild(usersDiv);
            }
        } catch (e) {}
    }

    if (channels.length === 0) {
        var empty = document.createElement("div");
        empty.className = "contacts-empty";
        empty.innerHTML = '<i data-lucide="headphones" style="width:40px;height:40px;color:#ccc"></i><span>' + t("voice.no_channels") + '</span>';
        dom.contactsList.appendChild(empty);
    }

    if (window.lucide) lucide.createIcons({ nodes: [dom.contactsList] });
    applySidebarFilter();
}

// Voice panel controls moved to calls.js (user-bar-voice-mute / user-bar-voice-disconnect)

// --- Sidebar search filter ---

var searchInput = document.getElementById("search-input");
if (searchInput) {
    searchInput.addEventListener("input", applySidebarFilter);
}

// applySidebarFilter — применяет текущий поисковый запрос к списку. Вызывается
// и на input, и после каждого ре-рендера (refreshPeers на каждое событие
// перерисовывал список и сбрасывал фильтр, пока пользователь искал).
function applySidebarFilter() {
    if (!searchInput) return;
    var raw = searchInput.value.trim();
    // Ссылка-приглашение в группу: карточка «Вступить» вместо фильтра (group-join.js).
    if (isInviteLink(raw) && (viewMode === "messages" || viewMode === "groups")) {
        renderJoinCard(dom.contactsList, raw);
        dom.contactsList.querySelectorAll(".contact-item, .voice-channel-item").forEach(function (it) { it.style.display = ""; });
        return;
    }
    removeJoinCard();
    var query = raw.toLowerCase();
    var items = dom.contactsList.querySelectorAll(".contact-item, .voice-channel-item");
    for (var i = 0; i < items.length; i++) {
        var nameEl = items[i].querySelector(".contact-name, .voice-channel-name");
        if (!nameEl) { items[i].style.display = ""; continue; }
        var name = nameEl.textContent.toLowerCase();
        items[i].style.display = (!query || name.indexOf(query) !== -1) ? "" : "none";
    }
}
