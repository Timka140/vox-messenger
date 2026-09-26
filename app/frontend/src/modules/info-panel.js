"use strict";

// info-panel.js — правая панель чата (макеты InfoDM, InfoGroup): вкладки,
// открыть/закрыть и вкладка «Инфо». Медиа — info-media.js; вкладки Чат и
// Участники во время сцены звонка ведёт stage.js (видимость вкладок —
// calls.js updateInfoTabs).
//
// Инфо = профиль сверху → ряд из четырёх действий → строки-настройки →
// свёрнутые «Технические данные» → опасные действия внизу. Всё, что есть в
// «…» шапки (chat-menu.js), здесь тоже доступно через «Ещё».

import { svc, flog, state, dom, colorFromId, Events, formatDateLabel } from "./core.js";
import { t, getLocale } from "./i18n.js";
import { refreshPeers, setRefreshInfoPanel, selectGroup, showGroupsList, groupMenuCallbacks, selectPeer, showMessagesList, setShowAddMemberDialog } from "./contacts-list.js";
import { getPeerVolume, setPeerVolume, isPeerMuted, setPeerMuted } from "./peer-volume.js";
import { getChatColor, showChatColorMenu } from "./chat-prefs.js";
import { isBlocked, blockPeer, unblockPeer } from "./blocked.js";
import { showContextMenu } from "./contextmenu.js";
import { buildDialogOverlay, buildFriendCheckboxList, confirmDialog } from "./ui.js";
import { updateInfoTabs, setCallRefreshInfoPanel, setCallSwitchInfoTab, setCallOpenInfoPanel, startCall } from "./calls.js";
import { showSuccess, showError } from "./toast.js";
import { isMuted, toggleMuted, peerKey, groupKey, currentChatMuteKey } from "./mute.js";
import { openSearch } from "./chat-search.js";
import { showRenameGroupDialog } from "./messages.js";
import { loadCommonGroupsFull, loadCalls, renderCalls, loadMeshNodeNum, buildMeshNodeEditor } from "./profile.js";
import { populateMediaTab, resetMediaIfChatChanged, getMediaSummary } from "./info-media.js";
import { applyAvatar } from "./avatar.js";
import { editGroupAvatar } from "./avatar-editor.js";

// --- Панель: открыть / закрыть / вкладки ---

function tabContent(name) { return document.getElementById("info-tab-" + name); }

// tabName — явная вкладка; без него: во время сцены звонка — «Участники»,
// иначе «Инфо».
function openInfoPanel(tabName) {
    dom.infoPanel.classList.add("open");
    var stageActive = document.getElementById("chat-area").classList.contains("stage-active");
    switchInfoTab(tabName || (stageActive ? "participants" : "info"));
    svc("SaveSetting", "info_panel_open", "1").catch(function () {});
}

function closeInfoPanel() {
    dom.infoPanel.classList.remove("open");
    svc("SaveSetting", "info_panel_open", "0").catch(function () {});
}

// refreshInfoPanel — смена чата или его данных (выбор, участники, имя).
// Кнопка «Mesh» в строке ввода зависит от выбранного друга, а не от панели,
// поэтому её состояние обновляем и при закрытой панели.
export function refreshInfoPanel() {
    syncMeshAvailability();
    resetMediaIfChatChanged();
    if (!dom.infoPanel.classList.contains("open")) return;
    updateInfoTabs();
    var active = dom.infoPanel.querySelector(".info-tab.active");
    var name = active ? active.dataset.infoTab : "info";
    if (name === "info") renderInfo();
}
setRefreshInfoPanel(refreshInfoPanel);

function switchInfoTab(tabName) {
    var tabs = dom.infoPanel.querySelectorAll(".info-tab");
    var contents = dom.infoPanel.querySelectorAll(".info-tab-content");
    tabs.forEach(function (x) { x.classList.toggle("active", x.dataset.infoTab === tabName); });
    contents.forEach(function (c) { c.classList.toggle("active", c.id === "info-tab-" + tabName); });

    if (tabName === "info") {
        renderInfo();
    } else if (tabName === "participants") {
        document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh")); // stage.js рендерит список
    } else if (tabName === "chat") {
        document.dispatchEvent(new CustomEvent("vox:ui-chat-tab-shown")); // stage.js прижмёт ленту к низу
    } else if (tabName === "media") {
        populateMediaTab();
    }
}

document.addEventListener("vox:ui-refresh-info-panel", function () { refreshInfoPanel(); });
document.addEventListener("vox:ui-close-info-panel", function () { closeInfoPanel(); });
// stage.js, chat-menu.js: открыть панель на нужной вкладке
document.addEventListener("vox:ui-open-info-panel", function (e) { openInfoPanel(e.detail && e.detail.tab); });
setShowAddMemberDialog(showAddMemberDialog); // ПКМ по строке группы в списке
// Цвет чата / закрепление поменяли — строка «Цвет чата» с новым кружком.
document.addEventListener("vox:ui-chat-prefs-changed", function () { if (infoVisible()) renderInfo(); });
document.addEventListener("vox:ui-blocked-changed", function () { if (infoVisible()) renderInfo(); });

setCallRefreshInfoPanel(refreshInfoPanel);
setCallSwitchInfoTab(switchInfoTab);
setCallOpenInfoPanel(openInfoPanel);

dom.infoToggleBtn.addEventListener("click", function () {
    if (dom.infoPanel.classList.contains("open")) closeInfoPanel(); else openInfoPanel();
});
dom.infoCloseBtn.addEventListener("click", closeInfoPanel);
dom.infoPanel.querySelectorAll(".info-tab").forEach(function (tab) {
    tab.addEventListener("click", function () { switchInfoTab(tab.dataset.infoTab); });
});

svc("GetSetting", "info_panel_open").then(function (val) {
    if (val === "1" && (state.selectedPeerID || state.selectedGroupID)) dom.infoPanel.classList.add("open");
}).catch(function () {});

// --- Кнопка «Mesh» в строке ввода (chat.js слушает vox:peer-mesh-*) ---

var lastMeshPeer = null;

function syncMeshAvailability() {
    var pid = state.selectedGroupID ? "" : String(state.selectedPeerID || "");
    if (pid.indexOf("__mesh:") === 0) pid = "";
    if (pid === lastMeshPeer) return;
    lastMeshPeer = pid;
    if (!pid) { dispatchMesh(false); return; }
    loadMeshNodeNum(pid).then(function (n) {
        if (lastMeshPeer === pid) dispatchMesh(!!n);
    });
}

function dispatchMesh(on) {
    document.dispatchEvent(new CustomEvent(on ? "vox:peer-mesh-available" : "vox:peer-mesh-unavailable"));
}

// Meshtastic включили/выключили в настройках — строка Mesh-узла и кнопка.
Events.On("meshtastic:status-change", function () {
    lastMeshPeer = null;
    syncMeshAvailability();
    if (infoVisible()) renderInfo();
});

// --- Мелкие построители ---

function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
}

function icon(name, size) {
    var i = document.createElement("i");
    i.setAttribute("data-lucide", name);
    i.className = "icon-" + (size || 16);
    return i;
}

function icons(node) {
    if (window.lucide) lucide.createIcons({ nodes: [node] });
}

function iconBtn(name, title, onClick) {
    var b = el("button", "ip-icon-btn");
    b.type = "button";
    b.title = title;
    b.appendChild(icon(name, 14));
    b.addEventListener("click", onClick);
    return b;
}

function hr() { return el("div", "ip-hr"); }

function initials(s) { return (s || "?").substring(0, 2).toUpperCase(); }

function copy(text) {
    try { navigator.clipboard.writeText(text); showSuccess(t("info.copied")); } catch (e) { /* окно без фокуса */ }
}

// plural — «7 участников» / «1 участник» (Intl.PluralRules по языку интерфейса).
export function plural(key, n) {
    var cat = "other";
    try { cat = new Intl.PluralRules(getLocale()).select(n); } catch (e) { /* старый движок */ }
    var s = t(key + "." + cat);
    if (s === key + "." + cat) s = t(key + ".other");
    return s.replace("{n}", n);
}

function shortDate(ms) {
    var d = new Date(ms);
    var opts = { day: "numeric", month: "short" };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
    return d.toLocaleDateString(getLocale() === "ru" ? "ru-RU" : "en-US", opts).replace(".", "");
}

// whenLabel — «сегодня» / «вчера» / «8 сен».
function whenLabel(ms) {
    var d = new Date(ms);
    var now = new Date();
    var yday = new Date(now);
    yday.setDate(yday.getDate() - 1);
    if (d.toDateString() === now.toDateString() || d.toDateString() === yday.toDateString()) {
        return formatDateLabel(ms).toLowerCase();
    }
    return shortDate(ms);
}

function actions(list) {
    var row = el("div", "ip-acts");
    list.forEach(function (a) {
        var b = el("button", "ip-act");
        b.type = "button";
        if (a.mute) b.dataset.muteAct = "1";
        b.appendChild(icon(a.icon, 18));
        b.appendChild(el("span", null, a.label));
        b.addEventListener("click", a.onClick);
        row.appendChild(b);
    });
    return row;
}

// row — строка «иконка · подпись · значение справа»; onClick делает её кнопкой.
function row(iconName, label, value, onClick, cls) {
    var r = el(onClick ? "button" : "div", "ip-row" + (cls ? " " + cls : ""));
    if (onClick) {
        r.type = "button";
        r.addEventListener("click", onClick);
    }
    r.appendChild(icon(iconName, 16));
    r.appendChild(el("span", "ip-row-label", label));
    var v = el("span", "ip-row-value");
    if (value instanceof Node) v.appendChild(value);
    else if (value) v.textContent = value;
    r.appendChild(v);
    return r;
}

function setRowValue(r, text, withChevron, open) {
    var v = r.querySelector(".ip-row-value");
    v.textContent = "";
    if (text) v.appendChild(el("span", "ip-row-text", text));
    if (withChevron) v.appendChild(icon(open ? "chevron-down" : "chevron-right", 14));
    icons(v);
}

function toggleEl(on) {
    var s = el("span", "ip-toggle" + (on ? " on" : ""));
    s.dataset.muteToggle = "1";
    return s;
}

// expandable — строка со значением, по клику раскрывает содержимое под собой.
function expandable(iconName, label) {
    var open = false, text = "…", can = false;
    var wrap = el("div", "ip-exp");
    var body = el("div", "ip-exp-body");
    body.hidden = true;
    var r = row(iconName, label, null, function () {
        if (!can) return;
        open = !open;
        body.hidden = !open;
        paint();
    });
    function paint() {
        setRowValue(r, text, can, open);
        r.classList.toggle("static", !can);
    }
    paint();
    wrap.appendChild(r);
    wrap.appendChild(body);
    return {
        el: wrap,
        setValue: function (tx, c) { text = tx; can = c; paint(); },
        fill: function (fn) { body.innerHTML = ""; fn(body); icons(body); },
    };
}

// techBlock — «Технические данные»: свёрнуты, пока их не раскрыли (макет).
var techOpen = false;
function techBlock(pairs) {
    var wrap = el("div", "ip-tech");
    var body = el("div", "ip-tech-body");
    body.hidden = !techOpen;
    var head = row("info", t("info.tech"), null, function () {
        techOpen = !techOpen;
        body.hidden = !techOpen;
        setRowValue(head, "", true, techOpen);
    });
    setRowValue(head, "", true, techOpen);
    pairs.forEach(function (p) {
        var r = el("div", "ip-tech-row");
        var txt = el("div", "ip-tech-text");
        txt.appendChild(el("span", "ip-tech-label", p[0]));
        txt.appendChild(el("span", "ip-tech-value mono", p[1]));
        r.appendChild(txt);
        r.appendChild(iconBtn("copy", t("info.copy"), function () { copy(p[1]); }));
        body.appendChild(r);
    });
    wrap.appendChild(head);
    wrap.appendChild(body);
    return wrap;
}

function muteParts(key) {
    var muted = isMuted(key);
    return {
        act: { icon: muted ? "bell" : "bell-off", label: t(muted ? "info.act_unmute" : "info.act_mute"), mute: true, onClick: function () { toggleMuted(key); } },
        row: row("bell", t("info.notifications"), toggleEl(!muted), function () { toggleMuted(key); }),
    };
}

// Тумблер и кнопку «Без звука» обновляем на месте — без перерисовки панели.
document.addEventListener("vox:ui-mute-changed", function () {
    var root = tabContent("info");
    if (!root) return;
    var muted = isMuted(currentChatMuteKey());
    root.querySelectorAll("[data-mute-toggle]").forEach(function (tg) { tg.classList.toggle("on", !muted); });
    root.querySelectorAll("[data-mute-act]").forEach(function (b) {
        b.innerHTML = "";
        b.appendChild(icon(muted ? "bell" : "bell-off", 18));
        b.appendChild(el("span", null, t(muted ? "info.act_unmute" : "info.act_mute")));
        icons(b);
    });
});

function moreMenu(e) {
    e.stopPropagation(); // иначе document-click в contextmenu.js сразу закроет меню
    document.dispatchEvent(new CustomEvent("vox:ui-chat-menu", { detail: { anchor: e.currentTarget } }));
}

// mediaSummaryText — «24 фото · 6 файлов · 5 ссылок»; пустые виды не пишем.
function mediaSummaryText(s) {
    var parts = [];
    if (s.photos) parts.push(plural("info.n_photos", s.photos));
    if (s.files) parts.push(plural("info.n_files", s.files));
    if (s.links) parts.push(plural("info.n_links", s.links));
    return parts.length ? parts.join(" · ") : t("info.none");
}

function openGroup(g) {
    document.querySelectorAll(".nav-item").forEach(function (b) { b.classList.toggle("active", b.dataset.nav === "groups"); });
    showGroupsList();
    selectGroup(g.groupId, g);
}

// --- Подтверждения (панель и меню «…» шапки) ---

export function confirmAction(title, text, confirmLabel, onConfirm) {
    confirmDialog(title, text, confirmLabel, onConfirm);
}

export function confirmDeleteChat(peerId, name) {
    confirmAction(t("ctx.delete_conversation"), t("info.confirm_delete_chat").replace("{name}", name), t("ctx.delete_conversation"), function () {
        svc("DeleteConversation", peerId).then(function () {
            if (state.selectedPeerID === peerId) dom.messagesDiv.innerHTML = "";
            delete state.lastMessages[peerId];
            refreshPeers();
            showSuccess(t("info.chat_deleted"));
        }).catch(function (err) { showError(String(err && err.message || err)); });
    });
}

// groupDangerItems — опасные действия группы, как в меню списка
// (buildGroupContextMenu): владелец удаляет, участник покидает.
export function groupDangerItems(g) {
    var cb = groupMenuCallbacks();
    if (g.left) {
        return [
            { label: t("ctx.rejoin_group"), icon: "log-in", action: function () { cb.onRejoin(g); } },
            { label: t("ctx.delete_left_group"), icon: "trash-2", danger: true, action: function () { cb.onDeleteLeft(g); } },
        ];
    }
    if (g.creatorId === state.myID) {
        return [{ label: t("ctx.delete_group"), icon: "trash-2", danger: true, action: function () {
            confirmAction(t("ctx.delete_group"), t("info.confirm_delete_group").replace("{name}", g.name), t("ctx.delete_group"), function () { cb.onDelete(g); });
        } }];
    }
    return [{ label: t("ctx.leave_group"), icon: "log-out", danger: true, action: function () {
        confirmAction(t("ctx.leave_group"), t("info.confirm_leave").replace("{name}", g.name), t("ctx.leave_group"), function () { cb.onLeave(g); });
    } }];
}

// --- Вкладка «Инфо» ---

var infoGen = 0;

function infoVisible() {
    var c = tabContent("info");
    return dom.infoPanel.classList.contains("open") && !!c && c.classList.contains("active");
}

function renderInfo() {
    var root = tabContent("info");
    if (!root) return;
    var gen = ++infoGen;
    if (state.selectedGroupID) renderGroupInfo(root, gen);
    else renderPeerInfo(root, gen);
}

function renderPeerInfo(root, gen) {
    root.innerHTML = "";
    var p = state.selectedPeerInfo;
    var peerId = state.selectedPeerID;
    if (!p || !peerId || String(peerId).indexOf("__mesh:") === 0) return;
    var nick = p.nickname || "";
    var code = p.shortId || peerId.substring(0, 16);
    var name = nick || code;
    var tag = (nick || "user") + "#" + code;

    var head = el("div", "ip-head");
    var av = el("div", "ip-avatar");
    applyAvatar(av, peerId, name);
    head.appendChild(av);
    head.appendChild(el("div", "ip-name", name));
    var tagRow = el("div", "ip-tag");
    tagRow.appendChild(el("span", "mono", tag));
    tagRow.appendChild(iconBtn("copy", t("ctx.copy_tag"), function () { copy(tag); }));
    head.appendChild(tagRow);
    var st = el("div", "ip-status" + (p.online ? " online" : ""));
    st.appendChild(el("span", "ip-dot" + (p.online ? "" : " off")));
    st.appendChild(document.createTextNode(p.online ? t("info.online") : t("info.offline")));
    head.appendChild(st);
    root.appendChild(head);

    var mp = muteParts(peerKey(peerId));
    root.appendChild(actions([
        { icon: "phone", label: t("info.act_call"), onClick: function () { startCall(peerId); } },
        mp.act,
        { icon: "search", label: t("info.act_search"), onClick: function () { openSearch(); } },
        { icon: "ellipsis", label: t("info.act_more"), onClick: moreMenu },
    ]));
    root.appendChild(hr());
    root.appendChild(mp.row);
    // «Цвет чата» (фаза 7a, макет InfoDM): кружок текущего цвета и стрелка
    var colorKey = peerKey(peerId);
    var colorVal = el("span", "ip-color-val");
    var swatch = el("span", "ip-swatch");
    swatch.style.background = getChatColor(colorKey) || "var(--bubble-out)";
    colorVal.appendChild(swatch);
    colorVal.appendChild(icon("chevron-right", 14));
    root.appendChild(row("palette", t("menu.chat_color"), colorVal, function (e) { showChatColorMenu(e, colorKey); }));
    var meshSlot = el("div");
    meshSlot.hidden = true;
    root.appendChild(meshSlot);
    root.appendChild(hr());
    var mediaRow = row("layout-grid", t("info.media_row"), null, function () { switchInfoTab("media"); });
    setRowValue(mediaRow, "…", true, false);
    root.appendChild(mediaRow);
    var groupsExp = expandable("users", t("info.common_groups"));
    root.appendChild(groupsExp.el);
    var callsExp = expandable("history", t("info.calls"));
    root.appendChild(callsExp.el);
    root.appendChild(hr());
    root.appendChild(techBlock([[t("info.node_id"), peerId]]));
    root.appendChild(el("div", "ip-spacer"));
    root.appendChild(hr());
    root.appendChild(row("trash-2", t("ctx.delete_conversation"), null, function () { confirmDeleteChat(peerId, name); }, "danger"));
    // Блокировка (фаза 7b, макет InfoDM): последняя красная строка
    root.appendChild(isBlocked(peerId)
        ? row("user-check", t("block.unblock"), null, function () { unblockPeer(peerId); })
        : row("user-minus", t("block.block"), null, function () { blockPeer(peerId, name); }, "danger"));
    icons(root);

    getMediaSummary().then(function (s) {
        if (gen === infoGen) setRowValue(mediaRow, mediaSummaryText(s), true, false);
    });
    loadCommonGroupsFull(peerId).then(function (groups) {
        if (gen !== infoGen) return;
        groupsExp.setValue(groups.length ? String(groups.length) : t("info.none"), groups.length > 0);
        groupsExp.fill(function (body) {
            groups.forEach(function (g) {
                var r = el("button", "ip-sub-row");
                r.type = "button";
                r.appendChild(icon("users", 14));
                r.appendChild(el("span", null, g.name));
                r.addEventListener("click", function () { openGroup(g); });
                body.appendChild(r);
            });
        });
    });
    loadCalls(peerId).then(function (items) {
        if (gen !== infoGen) return;
        callsExp.setValue(items.length ? t("info.calls_last").replace("{date}", whenLabel(items[0].createdAt * 1000)) : t("info.none"), items.length > 0);
        callsExp.fill(function (body) {
            var card = el("div", "ip-calls");
            card.appendChild(el("div", "profile-calls-list"));
            var all = el("button", "profile-link", t("profile.calls_all"));
            all.type = "button";
            all.hidden = true;
            card.appendChild(all);
            body.appendChild(card);
            renderCalls(card, items, all);
        });
    });
    loadMeshNodeNum(peerId).then(function (n) {
        if (gen !== infoGen || n === null) return;
        var r = row("radio-tower", t("info.mesh_node"), buildMeshNodeEditor(peerId, n, function () {
            lastMeshPeer = null;
            syncMeshAvailability();
        }));
        meshSlot.replaceWith(r);
        icons(r);
    });
}

async function renderGroupInfo(root, gen) {
    var gid = state.selectedGroupID;
    var g = null;
    for (var i = 0; i < state.groups.length; i++) if (state.groups[i].groupId === gid) { g = state.groups[i]; break; }
    var members = [], friends = [], voice = [], activeVoice = "";
    var voiceOn = !!(g && g.voiceEnabled && !g.left);
    try {
        var res = await Promise.all([
            svc("GetGroupMembers", gid),
            svc("GetFriends").catch(function () { return []; }),
            voiceOn ? svc("GetVoiceParticipants", gid).catch(function () { return []; }) : Promise.resolve([]),
            svc("GetActiveVoiceGroup").catch(function () { return ""; }),
        ]);
        members = res[0] || [];
        friends = res[1] || [];
        voice = res[2] || [];
        activeVoice = res[3] || "";
    } catch (err) {
        flog("ERR", "frontend.info", "group info: " + err);
    }
    if (gen !== infoGen || state.selectedGroupID !== gid) return; // переключились, пока ждали
    root.innerHTML = "";

    var name = g ? g.name : "Group";
    var left = !!(g && g.left);
    var isOwner = !!g && g.creatorId === state.myID;
    var inVoice = {};
    voice.forEach(function (v) { inVoice[v.peerId] = true; });
    var friendIds = {};
    friends.forEach(function (f) { friendIds[f.peerId] = true; });
    var online = members.filter(function (m) { return m.online || m.peerId === state.myID; }).length;

    var head = el("div", "ip-head");
    // Аватар группы: иконка, поверх — фото; владелец меняет его по камере.
    var avWrap = el("div", "ip-avatar-wrap");
    var av = el("div", "ip-avatar group");
    av.appendChild(icon("users", 28));
    applyAvatar(av, "g:" + gid, name, { keepContent: true, noColor: true });
    avWrap.appendChild(av);
    if (isOwner && !left) {
        var cam = el("button", "ip-avatar-cam");
        cam.type = "button";
        cam.title = t("avatar.change_group");
        cam.appendChild(icon("camera", 14));
        cam.addEventListener("click", function () { editGroupAvatar(gid); });
        avWrap.appendChild(cam);
    }
    head.appendChild(avWrap);
    var nameRow = el("div", "ip-name");
    nameRow.appendChild(el("span", null, name));
    if (isOwner && !left) nameRow.appendChild(iconBtn("pencil", t("ctx.rename_group"), function () { showRenameGroupDialog(gid, name); }));
    head.appendChild(nameRow);
    var sub = [plural("info.n_members", members.length), t("info.online_n").replace("{n}", online)];
    if (g && g.createdAt) sub.push(t("info.created").replace("{date}", shortDate(g.createdAt)));
    head.appendChild(el("div", "ip-sub", sub.join(" · ")));
    root.appendChild(head);

    var mp = muteParts(groupKey(gid));
    var acts = [];
    if (voiceOn) acts.push({ icon: "audio-lines", label: t("info.act_voice"), onClick: joinVoiceClick });
    acts.push(mp.act);
    acts.push({ icon: "search", label: t("info.act_search"), onClick: function () { openSearch(); } });
    acts.push({ icon: "ellipsis", label: t("info.act_more"), onClick: moreMenu });
    root.appendChild(actions(acts));

    // Голосовой канал — одна строка с составом и кнопкой, не вкладка (макет)
    if (voiceOn) {
        var inIt = activeVoice === gid;
        var vr = el("div", "ip-voice");
        vr.appendChild(icon("audio-lines", 16));
        var txt = el("div", "ip-voice-text");
        txt.appendChild(el("span", "ip-voice-title", t("info.voice_channel")));
        var names = voice.map(function (v) { return v.nickname || v.peerId.substring(0, 8); }).join(", ");
        txt.appendChild(el("span", "ip-voice-sub", names || t("info.voice_empty")));
        vr.appendChild(txt);
        var stack = el("div", "ip-voice-stack");
        voice.slice(0, 3).forEach(function (v) {
            var a = el("div", "ip-mini-av");
            applyAvatar(a, v.peerId, v.nickname || v.peerId, { letters: 1 });
            stack.appendChild(a);
        });
        vr.appendChild(stack);
        var jb = el("button", "ip-voice-btn" + (inIt ? " leave" : ""), t(inIt ? "info.voice_leave" : "info.voice_join"));
        jb.type = "button";
        jb.addEventListener("click", joinVoiceClick);
        vr.appendChild(jb);
        root.appendChild(vr);
    }

    // Участники: владелец, затем в сети, затем остальные; пять — сразу, дальше по кнопке
    var sec = el("div", "ip-sec");
    sec.appendChild(el("span", null, t("info.members_sec").replace("{n}", members.length)));
    if (isOwner && !left) {
        var add = el("button", "ip-sec-link", t("info.add"));
        add.type = "button";
        add.addEventListener("click", function () { showAddMemberDialog(gid, members); });
        sec.appendChild(add);
    }
    root.appendChild(sec);
    var rank = function (m) { return m.role === "admin" ? 0 : (m.online || m.peerId === state.myID) ? 1 : 2; };
    var nameOf = function (m) { return m.nickname || m.peerId; };
    var sorted = members.slice().sort(function (a, b) { return rank(a) - rank(b) || nameOf(a).localeCompare(nameOf(b)); });
    var list = el("div", "ip-members");
    root.appendChild(list);
    var rendered = 0;
    var moreBtn = el("button", "ip-more-members");
    moreBtn.type = "button";
    function renderMembers(limit) {
        var end = Math.min(rendered + limit, sorted.length);
        for (var j = rendered; j < end; j++) list.appendChild(memberRow(sorted[j]));
        rendered = end;
        var rest = sorted.length - rendered;
        moreBtn.hidden = rest <= 0;
        if (rest > 0) {
            moreBtn.textContent = "";
            moreBtn.appendChild(el("span", null, plural("info.more_members", rest)));
            moreBtn.appendChild(icon("chevron-down", 14));
        }
        icons(list);
        icons(moreBtn);
    }
    function memberRow(m) {
        var isMe = m.peerId === state.myID;
        var nm = m.nickname || m.peerId.substring(0, 16);
        var on = m.online || isMe;
        var r = el("div", "ip-mem" + (on ? "" : " off"));
        var a = el("div", "ip-mem-av");
        applyAvatar(a, m.peerId, nm, { letters: 1 });
        r.appendChild(a);
        var n = el("span", "ip-mem-name");
        n.appendChild(el("span", "ip-mem-nick", nm));
        if (isMe) n.appendChild(el("span", "ip-mem-you", t("info.you")));
        if (m.role === "admin") n.appendChild(el("span", "ip-badge", t("info.owner")));
        r.appendChild(n);
        if (inVoice[m.peerId]) {
            var vi = icon("audio-lines", 14);
            vi.classList.add("ip-mem-voice");
            r.appendChild(vi);
        }
        r.appendChild(el("span", "ip-dot" + (on ? "" : " off")));
        if (!isMe) {
            r.addEventListener("contextmenu", function (e) { memberMenu(e, m, isOwner && !left, !!friendIds[m.peerId], gid, !!(inVoice[m.peerId] && inVoice[state.myID])); });
        }
        return r;
    }
    moreBtn.addEventListener("click", function () { renderMembers(50); });
    root.appendChild(moreBtn);
    renderMembers(5);

    root.appendChild(hr());
    root.appendChild(mp.row);
    var mediaRow = row("layout-grid", t("info.media_row"), null, function () { switchInfoTab("media"); });
    setRowValue(mediaRow, "…", true, false);
    root.appendChild(mediaRow);
    root.appendChild(techBlock([[t("info.group_id"), gid]]));
    root.appendChild(el("div", "ip-spacer"));
    root.appendChild(hr());
    if (g) {
        groupDangerItems(g).forEach(function (it) {
            root.appendChild(row(it.icon, it.label, null, it.action, it.danger ? "danger" : ""));
        });
    }
    icons(root);

    getMediaSummary().then(function (s) {
        if (gen === infoGen) setRowValue(mediaRow, mediaSummaryText(s), true, false);
    });
}

// Вход/выход из голосового канала — та же логика, что у кнопки в шапке
// (offcanvas.js): закончить звонок, восстановить громкости, обновить сцену.
function joinVoiceClick() {
    var b = document.getElementById("voice-join-btn");
    if (b) b.click();
}

// memberMenu — то же меню участника, что на сцене звонка (макет ContextMenus
// «Участник звонка / голоса»): написать / в друзья, тег; громкость и звук
// «для меня» — только если он в голосе этой группы вместе со мной.
function memberMenu(e, member, canKick, isFriend, groupId, inMyVoice) {
    var pid = member.peerId;
    var tag = (member.nickname || "user") + "#" + pid.substring(0, 16);
    var items = [];
    if (isFriend) {
        items.push({ label: t("ctx.send_message"), icon: "message-square", action: function () {
            var p = (state.peers || []).find(function (x) { return x.id === pid; }) ||
                { id: pid, nickname: member.nickname, shortId: pid.substring(0, 16) };
            showMessagesList();
            selectPeer(pid, p);
        } });
    } else {
        items.push({ label: t("ctx.add_friend"), icon: "user-plus", action: function () {
            svc("SendFriendRequest", pid).then(function () {
                flog("INF", "frontend", "friend request sent, peer=" + String(pid).substring(0, 16));
            }).catch(function (err) {
                flog("ERR", "frontend", "friend request failed: " + err);
            });
        } });
    }
    items.push({ label: t("ctx.copy_tag"), icon: "copy", action: function () { copy(tag); } });
    if (inMyVoice) {
        var muted = isPeerMuted(groupId, pid);
        items.push({ separator: true });
        items.push({ label: t("ctx.volume_for_me"), volume: { value: muted ? 0 : getPeerVolume(groupId, pid), min: 0, max: 200, onChange: function (v) {
            setPeerVolume(groupId, pid, v);
            if (isPeerMuted(groupId, pid) && v > 0) setPeerMuted(groupId, pid, false);
        } } });
        items.push({ label: muted ? t("ctx.unmute_for_me") : t("ctx.mute_for_me"), icon: muted ? "volume-2" : "volume-x", action: function () {
            setPeerMuted(groupId, pid, !muted);
        } });
    }
    if (canKick) {
        items.push({ separator: true });
        items.push({ label: t("ctx.kick_member"), icon: "user-minus", danger: true, action: function () {
            svc("KickGroupMember", groupId, member.peerId).then(function () {
                renderInfo();
                refreshPeers();
            }).catch(function (err) { showError(String(err && err.message || err)); });
        } });
    }
    showContextMenu(e, items);
}

// Состав голосового канала и наш вход/выход — перерисовать строку канала.
var voiceTimer = null;
function onVoiceChanged() {
    if (!state.selectedGroupID || !infoVisible()) return;
    clearTimeout(voiceTimer);
    voiceTimer = setTimeout(renderInfo, 300);
}
Events.On("vox:voice-join", onVoiceChanged);
Events.On("vox:voice-leave", onVoiceChanged);
Events.On("vox:voice-status", onVoiceChanged);
document.addEventListener("vox:ui-voice-changed", onVoiceChanged);

// inviteLinkBlock — «Ссылка-приглашение · до 22 сен» с «Копировать» и сбросом
// (фаза 5d). Ссылка живёт 7 дней, сброс делает прежнюю недействительной.
function inviteLinkBlock(groupId) {
    var box = el("div", "invite-link");
    box.appendChild(icon("link", 16));
    var txt = el("div", "invite-link-text");
    txt.appendChild(el("span", "invite-link-title", t("invite.title")));
    var sub = el("span", "invite-link-sub", "…");
    txt.appendChild(sub);
    box.appendChild(txt);
    var copyBtn = el("button", "invite-link-btn", t("invite.copy"));
    copyBtn.type = "button";
    copyBtn.disabled = true;
    box.appendChild(copyBtn);
    var link = "";
    function paint(info) {
        link = info.link;
        var short = link.length > 24 ? link.slice(0, 18) + "…" + link.slice(-4) : link;
        sub.textContent = t("invite.sub").replace("{link}", short).replace("{date}", shortDate(info.expires));
        sub.title = link;
        copyBtn.disabled = false;
    }
    var resetBtn = iconBtn("rotate-ccw", t("invite.reset"), function () {
        confirmDialog(t("invite.reset"), t("invite.reset_confirm"), t("invite.reset"), function () {
            svc("ResetGroupInviteLink", groupId).then(function (info) {
                paint(info);
                showSuccess(t("invite.reset_done"));
            }).catch(function (err) { showError(String(err && err.message || err)); });
        }, { danger: false });
    });
    box.appendChild(resetBtn);
    copyBtn.addEventListener("click", function () { if (link) copy(link); });
    svc("GetGroupInviteLink", groupId).then(paint).catch(function (err) {
        sub.textContent = String(err && err.message || err);
        resetBtn.disabled = true;
    });
    requestAnimationFrame(function () { icons(box); });
    return box;
}

// showAddMemberDialog — «Добавить в <группа>» (макет Dialogs): только друзья,
// которых ещё нет в группе; поиск, мультивыбор, «Добавить · N».
export async function showAddMemberDialog(groupId, currentMembers) {
    try {
        if (!currentMembers) currentMembers = (await svc("GetGroupMembers", groupId)) || [];
        var friends = (await svc("GetFriends")) || [];
        var memberIds = {};
        for (var i = 0; i < currentMembers.length; i++) memberIds[currentMembers[i].peerId] = true;
        var available = friends.filter(function (f) { return !memberIds[f.peerId]; });
        var g = null;
        for (var k = 0; k < state.groups.length; k++) if (state.groups[k].groupId === groupId) { g = state.groups[k]; break; }

        var dlg = buildDialogOverlay(t("info.add_to").replace("{name}", g ? g.name : ""), "", t("dialog.cancel"), t("info.add"), null);
        var btn = dlg.confirmBtn;
        btn.disabled = true;
        // Не-друзей приглашают ссылкой (макет Dialogs); её выпускает только владелец.
        var owner = !!g && g.creatorId === state.myID && !g.left;
        if (available.length === 0) {
            dlg.body.appendChild(el("div", "dialog-hint", t(friends.length ? "info.all_friends_in" : "group.no_friends")));
            btn.hidden = true;
            if (owner) dlg.body.appendChild(inviteLinkBlock(groupId));
            return;
        }
        var list = buildFriendCheckboxList(available, {
            onSelectionChange: function (n) {
                btn.disabled = n === 0;
                btn.textContent = n > 0 ? t("info.add_n").replace("{n}", String(n)) : t("info.add");
            }
        });
        dlg.body.appendChild(list);
        icons(list);
        if (owner) dlg.body.appendChild(inviteLinkBlock(groupId));
        btn.addEventListener("click", async function () {
            var ids = Object.keys(list._selectedSet || {});
            if (ids.length === 0) return;
            btn.disabled = true;
            for (var j = 0; j < ids.length; j++) {
                try {
                    await svc("AddMemberToGroup", groupId, ids[j]);
                } catch (err) {
                    showError(String(err && err.message || err));
                }
            }
            dlg.overlay.remove();
            renderInfo();
            refreshPeers();
        });
    } catch (err) {
        console.error("add member:", err);
    }
}
