// stage.js — сцена звонка / голосового канала в области чата.
//
// Показывается вместо ленты сообщений, пока открыт чат-владелец звонка
// (state.call.ownerKey === текущий чат) или группа, в чьём голосовом канале мы
// сидим (activeVoiceGroup === state.selectedGroupID). В других чатах сцены нет.
//
// Состав: шапка (имя · тип · N участников · таймер · сигнал), тело — либо
// закреплённая демонстрация экрана (canvas из пула screenshare.js) с полосой
// над ней (кто показывает, чипы при нескольких, «рядом», fullscreen, свернуть),
// либо кластер круглых аватаров; лента 152×86 под сценой только при
// демонстрации; контрол-бар (микрофон ▾, звук ▾, показ экрана, ещё, выйти).
//
// Связь с остальным кодом: старые модули НЕ импортируют stage.js — они шлют
// DOM-событие vox:ui-stage-refresh (calls.js, contacts-list.js). Пул плиток
// сообщает о себе vox:screen-tile-added / vox:screen-tile-removed. Wails-события
// голоса (join/leave/status/activity) сцена слушает сама.

import { svc, state, dom, formatMmSs, colorFromId, flog } from "./core.js";
import { t } from "./i18n.js";
import { voiceState } from "./voice-state.js";
import { getTile, listSharers, addSink, removeSink, setDecodeSuspended, isSelf, peerLabel, toggleScreenShare, isScreenSharing, hasShareAudio, getShareVolume, setShareVolume } from "./screenshare.js";
import { endCall, endCallForAll, renderUserBar, showAddParticipantDialog, updateInfoTabs } from "./calls.js";
import { scrollToBottom } from "./messages.js";
import { selectPeer, showMessagesList } from "./contacts-list.js";
import { showSuccess, showError } from "./toast.js";
import { getPeerVolume, setPeerVolume, isPeerMuted, setPeerMuted } from "./peer-volume.js";
import { leaveVoice, getVoiceQuality } from "./voice.js";
import { updateVoiceHeaderBtn } from "./events.js";
import { showContextMenu } from "./contextmenu.js";
import { escapeHtml } from "./ui.js";
import { applyAvatar, avatarHTML, paintAvatars } from "./avatar.js";
import { showConnStats, refreshConnMode } from "./conn-stats.js";
import { openSettingsTab, startMicTest } from "./settings.js";

// --- состояние модуля ---
let activeVoiceGroup = null;   // groupId голосового канала, в котором мы сидим
let voiceJoinedAt = 0;         // для таймера голосового канала
let voiceParticipants = [];    // [{peerId, nickname, speaking, muted}] из GetVoiceParticipants
let mounted = false;           // сцена показана
let pinned = null;             // peerId демонстрации на сцене
let split = false;             // все демонстрации рядом
let manualCollapse = false;    // «Свернуть» — показать кластер, пока не выберут плитку
const speaking = new Set();    // peerId говорящих
const filmstripPinned = new Set(); // кого принудительно держать в ленте (клик в «Скрыты»)
const sinkCanvases = new Map();    // peerId → canvas превью в ленте
let tickTimer = null;
let panelMode = false;         // #messages/#input-area перенесены в панель
let panelWasOpen = false;      // была ли панель открыта до сцены (вернуть как было)
let stripVisible = null;       // Set peerId в ленте (null — ленты нет)
let friendIds = null;          // Set peerId друзей (для меню «Написать / В друзья»)
const poppedOut = new Set();   // peerId демонстраций, вынесенных в отдельные окна (screen-view.html)
const mutedAllByMenu = new Map(); // scope → Set peerId, заглушённых пунктом «Заглушить всех для меня»

const el = {};
function q(id) { return document.getElementById(id); }

// --- контекст ---

export function currentChatKey() {
    if (state.selectedGroupID) return "group:" + state.selectedGroupID;
    if (state.selectedPeerID) return "peer:" + state.selectedPeerID;
    return "";
}

function callOwnerKey() {
    if (!state.call) return null;
    if (state.call.ownerKey !== undefined) return state.call.ownerKey;
    return state.call.peerId ? "peer:" + state.call.peerId : "";
}

// stageContext — что показывать в текущем чате: {kind:"call"} | {kind:"voice", groupId} | null
export function stageContext() {
    if (state.call) {
        return callOwnerKey() === currentChatKey() ? { kind: "call" } : null;
    }
    if (activeVoiceGroup && activeVoiceGroup === state.selectedGroupID) {
        return { kind: "voice", groupId: activeVoiceGroup };
    }
    return null;
}

// getActiveSession — есть ли вообще звонок/канал (для полоски в чужом чате).
export function getActiveSession() {
    if (state.call) {
        const key = callOwnerKey();
        return {
            kind: "call",
            key: key,
            name: sessionName({ kind: "call" }),
            startedAt: state.call.startedAt,
            sharers: listSharers(),
        };
    }
    if (activeVoiceGroup) {
        return {
            kind: "voice",
            key: "group:" + activeVoiceGroup,
            groupId: activeVoiceGroup,
            name: groupName(activeVoiceGroup),
            startedAt: voiceJoinedAt,
            sharers: listSharers(),
        };
    }
    return null;
}

function groupName(gid) {
    const g = (state.groups || []).find((x) => x.groupId === gid);
    return g ? g.name : gid.slice(0, 8);
}

function peerName(pid) {
    if (isSelf(pid)) return state.myNickname || state.myShortID || t("stage.you");
    const p = (state.peers || []).find((x) => x.id === pid);
    if (p) return p.nickname || p.shortId || pid.slice(0, 8);
    const cp = state.call && state.call.participants.find((x) => x.peerId === pid);
    if (cp && cp.nickname) return cp.nickname;
    const vp = voiceParticipants.find((x) => x.peerId === pid);
    if (vp && vp.nickname) return vp.nickname;
    return pid.slice(0, 8) + "…";
}

function sessionName(ctx) {
    if (ctx.kind === "voice") return groupName(ctx.groupId || activeVoiceGroup);
    if (!state.call) return "";
    if (state.call.peerId) return peerName(state.call.peerId);
    // ad-hoc группа: имена собеседников
    return state.call.participants.filter((p) => !isSelf(p.peerId)).map((p) => p.nickname).join(", ");
}

function participants(ctx) {
    if (ctx.kind === "call") return state.call ? state.call.participants : [];
    return voiceParticipants;
}

function initials(name) {
    return (name || "?").trim().slice(0, 1) || "?";
}

// --- данные голосового канала ---

async function refreshVoice() {
    try {
        const ag = await svc("GetActiveVoiceGroup");
        const next = ag || null;
        if (next !== activeVoiceGroup) {
            activeVoiceGroup = next;
            if (next && !voiceJoinedAt) voiceJoinedAt = Date.now();
            if (!next) voiceJoinedAt = 0;
        }
        if (activeVoiceGroup) {
            voiceParticipants = (await svc("GetVoiceParticipants", activeVoiceGroup)) || [];
        } else {
            voiceParticipants = [];
        }
    } catch (e) {
        flog("WRN", "stage", "refreshVoice: " + (e && e.message || e));
    }
    render();
}

// --- монтирование ---

function hasChat() {
    return !!(state.selectedPeerID || state.selectedGroupID);
}

function mount() {
    if (mounted) return;
    mounted = true;
    q("chat-area").classList.add("stage-active");
    el.stage.hidden = false;
    enterPanelMode();
}

function unmount() {
    if (!mounted) return;
    mounted = false;
    detachStageCanvases();
    removeRinging(el.body);
    clearFilmstrip();
    el.stage.hidden = true;
    q("chat-area").classList.remove("stage-active");
    exitPanelMode();
}

// --- режим панели: лента и строка ввода живут во вкладке «Чат» ---
// Элементы переносятся физически — слушатели messages.js (scroll/wheel/load)
// висят на них и остаются живыми. isNearBottom() у скрытой вкладки даёт
// true → автоскролл вместо индикатора «новые», это приемлемо.

function enterPanelMode() {
    if (panelMode) return;
    panelMode = true;
    panelWasOpen = dom.infoPanel.classList.contains("open");
    q("info-tab-chat").append(dom.messagesDiv, dom.inputArea);
    dom.messagesDiv.style.display = "";
    dom.inputArea.style.display = hasChat() ? "block" : "none";
    dom.infoPanel.classList.add("stage");
    updateInfoTabs();
    if (!friendIds) {
        svc("GetFriends").then((list) => {
            friendIds = new Set((list || []).map((f) => f.peerId));
        }).catch(() => { friendIds = new Set(); });
    }
    // Панель открываем после текущего рендера: openInfoPanel → switchInfoTab
    // («Участники») → vox:ui-stage-refresh → render — не вкладываться в себя.
    setTimeout(() => {
        if (panelMode) document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "participants" } }));
    }, 0);
}

function exitPanelMode() {
    if (!panelMode) return;
    panelMode = false;
    q("messages-anchor").after(dom.messagesDiv, dom.inputArea);
    // Что показывать в области чата — как это делают selectPeer/selectGroup
    dom.messagesDiv.style.display = "";
    dom.inputArea.style.display = hasChat() ? "block" : "none";
    dom.infoPanel.classList.remove("stage");
    q("info-tab-participants").innerHTML = "";
    updateInfoTabs();
    if (!panelWasOpen) document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
    else document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    friendIds = null;
}

function chatTabActive() {
    const tab = q("info-tab-chat");
    return !!tab && tab.classList.contains("active");
}

// --- рендер ---

export function render() {
    const ctx = stageContext();
    syncTick();
    if (!ctx) { unmount(); sessionChanged(); return; }
    mount();
    // Пин: если закреплённый ушёл — следующий шарер или кластер
    const sharers = listSharers();
    if (pinned && sharers.indexOf(pinned) < 0) pinned = null;
    if (!pinned && !manualCollapse && sharers.length > 0) pinned = sharers[0];
    if (sharers.length === 0) { pinned = null; split = false; manualCollapse = false; }

    renderHeader(ctx);
    renderBody(ctx, sharers);
    renderFilmstrip(ctx, sharers);
    renderControls(ctx);
    renderParticipants(ctx, sharers);
    if (window.lucide) lucide.createIcons({ nodes: [el.stage] });
    sessionChanged();
}

// sessionChanged — сигнал call-bar.js (и кому ещё нужно), что сессия/сцена
// перерисована: видимость полоски, имя, шареры.
function sessionChanged() {
    document.dispatchEvent(new CustomEvent("vox:ui-session-changed"));
}

function renderHeader(ctx) {
    q("stage-title").textContent = sessionName(ctx);
    q("stage-sub").textContent = ctx.kind === "call" ? t("stage.voice_call") : t("stage.voice_channel");
    const n = participants(ctx).length;
    q("stage-count").textContent = t("stage.participants_n").replace("{n}", String(n));
    updateTimer(ctx);
    updateSignal(ctx);
    q("stage-invite").style.display = ctx.kind === "call" ? "" : "none";
    q("stage-chat-toggle").classList.toggle("active", dom.infoPanel.classList.contains("open"));
}

// formatSessionTimer — mm:ss от начала сессии; звонок без ответа → «Вызов…».
// Используется и полоской в чужом чате (call-bar.js).
export function formatSessionTimer(session) {
    if (!session.startedAt) return session.kind === "call" ? t("stage.calling") : "00:00";
    return formatMmSs(Date.now() - session.startedAt);
}

function updateTimer(ctx) {
    const timerEl = q("stage-timer");
    if (!timerEl) return;
    const started = ctx.kind === "call" ? (state.call && state.call.startedAt) : voiceJoinedAt;
    timerEl.textContent = formatSessionTimer({ kind: ctx.kind, startedAt: started });
    const rt = el.body && el.body.querySelector(".stage-ring-timer");
    if (rt) rt.textContent = ringText();
}

function updateSignal(ctx) {
    const sig = q("stage-signal");
    if (!sig) return;
    const parts = participants(ctx);
    const reconnecting = parts.some((p) => p.reconnecting);
    const q1 = getVoiceQuality();
    let cls = "good", title = t("stage.signal_good");
    if (reconnecting) { cls = "bad"; title = t("stage.signal_bad"); }
    else if (q1.jitterMs > 100) { cls = "fair"; title = t("stage.signal_fair") + " · " + Math.round(q1.jitterMs) + " ms"; }
    // lucide заменяет <i> на <svg>, ищем по id заново после createIcons
    const node = sig;
    node.classList.remove("good", "fair", "bad");
    node.classList.add(cls);
    node.setAttribute("title", title);
}

function detachStageCanvases() {
    const body = el.body;
    Array.from(body.querySelectorAll("canvas.screen-canvas, .stage-popout-placeholder")).forEach((c) => c.remove());
}

function renderBody(ctx, sharers) {
    const body = el.body;
    const bar = el.bar;
    const wrap = q("stage-body-wrap");
    const showShare = !!pinned || (split && sharers.length > 0);

    if (!showShare) {
        detachStageCanvases();
        bar.hidden = true;
        wrap.classList.remove("has-share");
        body.classList.remove("split");
        if (isRinging(ctx)) {
            renderRinging(ctx, body);
        } else {
            removeRinging(body);
            renderCluster(ctx, body);
        }
        return;
    }

    // Демонстрация на сцене
    wrap.classList.add("has-share");
    bar.hidden = false;
    // убираем кластер, если был
    const cluster = body.querySelector(".stage-cluster");
    if (cluster) cluster.remove();
    removeRinging(body);

    const wanted = split ? sharers : [pinned];
    body.classList.toggle("split", split && sharers.length > 1);
    // снять лишние canvas и старый плейсхолдер
    Array.from(body.querySelectorAll("canvas.screen-canvas")).forEach((c) => {
        if (wanted.indexOf(c.dataset.peer) < 0 || poppedOut.has(c.dataset.peer)) c.remove();
    });
    body.querySelectorAll(".stage-popout-placeholder").forEach((ph) => ph.remove());
    wanted.forEach((pid) => {
        if (poppedOut.has(pid)) { body.appendChild(buildPopoutPlaceholder(pid)); return; }
        const tile = getTile(pid);
        if (!tile) return;
        if (tile.canvas.parentNode !== body) body.appendChild(tile.canvas);
    });
    q("stage-popout").hidden = split || !pinned || poppedOut.has(pinned);

    // Полоса: кто показывает + чипы
    const sharerEl = q("stage-sharer");
    if (split) {
        sharerEl.innerHTML = escapeHtml(t("stage.side_by_side"));
    } else {
        sharerEl.innerHTML = isSelf(pinned)
            ? "<b>" + escapeHtml(t("stage.sharing_by_you")) + "</b>"
            : t("stage.sharing_by").replace("{name}", "<b>" + escapeHtml(peerLabel(pinned)) + "</b>");
    }
    const chips = q("stage-chips");
    chips.innerHTML = "";
    if (sharers.length > 1) {
        sharers.forEach((pid) => {
            const chip = document.createElement("button");
            chip.type = "button";
            chip.className = "stage-chip" + (!split && pid === pinned ? " active" : "");
            chip.textContent = peerLabel(pid);
            chip.addEventListener("click", () => { split = false; manualCollapse = false; pinned = pid; render(); });
            chips.appendChild(chip);
        });
    }
    const splitBtn = q("stage-split");
    splitBtn.hidden = sharers.length < 2;
    splitBtn.classList.toggle("active", split);
}

function clusterSizeClass(n) {
    if (n <= 8) return "";
    if (n <= 16) return "size-m";
    return "size-s";
}

function personState(p) {
    if (p.reconnecting) return t("stage.state_reconnecting");
    if (speaking.has(p.peerId)) return t("stage.state_speaking");
    const st = voiceState.getPeerStatus(p.peerId) || {};
    if (st.deafened) return t("stage.state_deafened");
    if (st.muted) return t("stage.state_muted");
    return "";
}

function renderCluster(ctx, body) {
    const parts = participants(ctx);
    let cluster = body.querySelector(".stage-cluster");
    if (!cluster) {
        cluster = document.createElement("div");
        cluster.className = "stage-cluster";
        body.appendChild(cluster);
    }
    cluster.className = "stage-cluster " + clusterSizeClass(parts.length);
    cluster.innerHTML = "";
    parts.forEach((p) => {
        const st = voiceState.getPeerStatus(p.peerId) || {};
        const me = isSelf(p.peerId);
        const name = me ? (state.myNickname || state.myShortID || t("stage.you")) : (p.nickname || peerName(p.peerId));
        const person = document.createElement("div");
        person.className = "stage-person" + (speaking.has(p.peerId) ? " is-speaking" : "") + (p.reconnecting ? " is-reconnecting" : "");
        person.dataset.peerId = p.peerId;
        let badge = "";
        if (st.deafened) badge = '<div class="stage-person-badge"><i data-lucide="headphone-off" class="icon-14"></i></div>';
        else if (st.muted) badge = '<div class="stage-person-badge"><i data-lucide="mic-off" class="icon-14"></i></div>';
        person.innerHTML =
            '<div class="stage-person-avatar-wrap">' +
                avatarHTML(p.peerId, name, "stage-person-avatar", 1) + badge +
            '</div>' +
            '<div class="stage-person-name">' + escapeHtml(name) + (me ? ' <span class="me">· ' + escapeHtml(t("stage.you")) + '</span>' : '') + '</div>' +
            '<div class="stage-person-state">' + escapeHtml(personState(p)) + '</div>';
        paintAvatars(person);
        if (!me) person.addEventListener("contextmenu", (e) => showPeerMenu(e, p.peerId));
        cluster.appendChild(person);
    });
}

// --- Исходящий личный звонок, пока не ответили (макет CallDM) ---
// Собеседник крупно с пульсирующим кольцом, «Вызов · 0:07», своя маленькая
// плитка в правом нижнем углу. После ответа (startedAt) — обычный кластер.

let ringCallId = null;
let ringStartedAt = 0;

function isRinging(ctx) {
    if (ctx.kind !== "call" || !state.call || state.call.startedAt) return false;
    return participants(ctx).filter((p) => !isSelf(p.peerId)).length === 1;
}

function removeRinging(body) {
    body.querySelectorAll(".stage-ringing, .stage-self-tile").forEach((n) => n.remove());
}

function ringText() {
    const s = Math.floor((Date.now() - ringStartedAt) / 1000);
    return t("stage.ringing").replace("{t}", Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0"));
}

function renderRinging(ctx, body) {
    const cluster = body.querySelector(".stage-cluster");
    if (cluster) cluster.remove();
    if (ringCallId !== state.call.id) { ringCallId = state.call.id; ringStartedAt = Date.now(); }
    const other = participants(ctx).find((p) => !isSelf(p.peerId));
    const name = other.nickname || peerName(other.peerId);
    let ring = body.querySelector(".stage-ringing");
    if (!ring) {
        ring = document.createElement("div");
        ring.className = "stage-ringing";
        body.appendChild(ring);
    }
    ring.innerHTML = avatarHTML(other.peerId, name, "stage-ring-avatar", 1) +
        '<div class="stage-ring-name">' + escapeHtml(name) + '</div>' +
        '<div class="stage-ring-state"><span class="stage-ring-dots"><i></i><i></i><i></i></span>' +
        '<span class="stage-ring-timer">' + escapeHtml(ringText()) + '</span></div>';
    paintAvatars(ring);
    let self = body.querySelector(".stage-self-tile");
    if (!self) {
        self = document.createElement("div");
        self.className = "stage-self-tile";
        body.appendChild(self);
    }
    const my = voiceState.getMyStatus() || {};
    const myName = state.myNickname || state.myShortID || t("stage.you");
    self.innerHTML = avatarHTML(state.myID, myName, "stage-self-avatar", 1) +
        '<i data-lucide="' + (my.muted ? "mic-off" : "mic") + '" class="icon-14 stage-self-mic' + (my.muted ? " red" : "") + '"></i>';
    paintAvatars(self);
}

// Лента: одна плитка на участника; шарер → превью его экрана.
function clearFilmstrip() {
    sinkCanvases.forEach((canvas, pid) => removeSink(pid, canvas));
    sinkCanvases.clear();
    el.filmstrip.innerHTML = "";
}

function orderedParticipants(ctx, sharers) {
    const parts = participants(ctx).slice();
    const rank = (p) => (sharers.indexOf(p.peerId) >= 0 ? 0 : speaking.has(p.peerId) ? 1 : 2);
    return parts.sort((a, b) => rank(a) - rank(b));
}

function renderFilmstrip(ctx, sharers) {
    const strip = el.filmstrip;
    if (sharers.length === 0) {
        clearFilmstrip();
        strip.hidden = true;
        stripVisible = null;
        return;
    }
    strip.hidden = false;
    clearFilmstrip();
    const parts = orderedParticipants(ctx, sharers);
    // сколько влезает: (ширина − паддинги) / (152 + 8); последняя ячейка — «+N» (68px)
    const width = strip.clientWidth || (q("chat-area").clientWidth - 24);
    let capacity = Math.max(1, Math.floor((width - 24 + 8) / 160));
    let visible = parts;
    let hidden = [];
    if (parts.length > capacity) {
        const keep = Math.max(1, capacity - 1);
        const pinnedFirst = parts.filter((p) => filmstripPinned.has(p.peerId));
        const rest = parts.filter((p) => !filmstripPinned.has(p.peerId));
        visible = pinnedFirst.concat(rest).slice(0, keep);
        hidden = parts.filter((p) => visible.indexOf(p) < 0);
    }
    stripVisible = new Set(visible.map((p) => p.peerId));
    visible.forEach((p) => strip.appendChild(buildTile(p, sharers)));
    if (hidden.length > 0) {
        const more = document.createElement("div");
        more.className = "stage-tile more";
        more.title = t("stage.participants_tab");
        more.innerHTML = '<div class="stage-tile-stack">' +
            hidden.slice(0, 3).map((p) => avatarHTML(p.peerId, p.nickname || peerName(p.peerId), "stage-tile-mini", 1, "span")).join("") +
            '</div><div class="stage-tile-more-n">' + escapeHtml(t("stage.more_n").replace("{n}", String(hidden.length))) + '</div>';
        paintAvatars(more);
        more.addEventListener("click", () => document.dispatchEvent(new CustomEvent("vox:ui-open-participants")));
        strip.appendChild(more);
    }
}

function buildTile(p, sharers) {
    const pid = p.peerId;
    const me = isSelf(pid);
    const name = me ? t("stage.you") : (p.nickname || peerName(pid));
    const isShare = sharers.indexOf(pid) >= 0;
    const st = voiceState.getPeerStatus(pid) || {};
    const tileEl = document.createElement("div");
    const inWindow = isShare && poppedOut.has(pid);
    tileEl.className = "stage-tile" + (isShare ? " is-share" : "") + (speaking.has(pid) ? " is-speaking" : "") + (isShare && !split && pinned === pid ? " on-stage" : "") + (inWindow ? " in-window" : "");
    tileEl.dataset.peerId = pid;
    if (isShare) {
        const c = document.createElement("canvas");
        c.className = "stage-tile-preview";
        c.width = 304; c.height = 172; // 2× для чёткости
        tileEl.appendChild(c);
        addSink(pid, c);
        sinkCanvases.set(pid, c);
        tileEl.addEventListener("click", () => { split = false; manualCollapse = false; pinned = pid; render(); });
    } else {
        const av = document.createElement("div");
        av.className = "stage-tile-avatar";
        applyAvatar(av, pid, name, { letters: 1 });
        tileEl.appendChild(av);
    }
    const nameEl = document.createElement("div");
    nameEl.className = "stage-tile-name";
    nameEl.innerHTML = (isShare ? avatarHTML(pid, name, "stage-tile-mini", 1, "span") : '') +
        escapeHtml(name);
    paintAvatars(nameEl);
    tileEl.appendChild(nameEl);
    const stateEl = document.createElement("div");
    stateEl.className = "stage-tile-state";
    // Показывает со звуком системы (фаза 5e)
    const shareAudio = isShare && hasShareAudio(pid) ? '<i data-lucide="volume-2" class="icon-14"></i>' : '';
    if (inWindow || (isShare && !split && pinned === pid)) {
        stateEl.innerHTML = shareAudio + '<span class="on-stage-lbl">' + escapeHtml(t(inWindow ? "stage.in_window" : "stage.on_stage")) + '</span>';
    } else {
        let icons = shareAudio;
        if (st.muted) icons += '<i data-lucide="mic-off" class="icon-14 red"></i>';
        else icons += '<i data-lucide="mic" class="icon-14' + (speaking.has(pid) ? ' green' : '') + '"></i>';
        if (st.deafened) icons += '<i data-lucide="headphone-off" class="icon-14 red"></i>';
        stateEl.innerHTML = icons;
    }
    tileEl.appendChild(stateEl);
    // PiP на плитке чужой демонстрации (макет «Несколько демонстраций»):
    // в отдельное окно / вернуть. Клик не ставит плитку на сцену.
    if (isShare && !me) {
        const pip = document.createElement("button");
        pip.type = "button";
        pip.className = "stage-tile-pip";
        pip.title = t(inWindow ? "stage.popout_back" : "stage.popout");
        pip.innerHTML = '<i data-lucide="' + (inWindow ? "undo-2" : "picture-in-picture-2") + '" class="icon-14"></i>';
        pip.addEventListener("click", (e) => {
            e.stopPropagation();
            if (poppedOut.has(pid)) closePopout(pid); else openPopout(pid);
        });
        tileEl.appendChild(pip);
    }
    // Плитка демонстрации — меню экрана (в т.ч. своя: «Остановить показ»),
    // остальные — меню участника.
    if (isShare) tileEl.addEventListener("contextmenu", (e) => showShareTileMenu(e, pid));
    else if (!me) tileEl.addEventListener("contextmenu", (e) => showPeerMenu(e, pid));
    return tileEl;
}

function renderControls(ctx) {
    const my = voiceState.getMyStatus() || {};
    const mic = q("stage-mic");
    mic.classList.toggle("off", !!my.muted);
    mic.innerHTML = '<i data-lucide="' + (my.muted ? "mic-off" : "mic") + '" class="icon-20"></i>';
    const snd = q("stage-sound");
    snd.classList.toggle("off", !!my.deafened);
    snd.innerHTML = '<i data-lucide="' + (my.deafened ? "headphone-off" : "headphones") + '" class="icon-20"></i>';
    const share = q("stage-share");
    const sharing = isScreenSharing();
    share.classList.toggle("on", sharing);
    share.innerHTML = '<i data-lucide="' + (sharing ? "monitor-off" : "monitor-up") + '" class="icon-20"></i>';
    q("stage-share-lbl").textContent = sharing ? t("call.stop_share") : t("call.share_screen");
    const leaveLbl = q("stage-leave").querySelector("span");
    if (leaveLbl) leaveLbl.textContent = t("stage.leave");
}

// --- панель «Участники» (макет MultiShareParticipants) ---

function volumeScope(ctx) {
    return ctx.kind === "call" ? "call:" + (state.call ? state.call.id : "") : ctx.groupId;
}

function displayName(p) {
    return isSelf(p.peerId) ? (state.myNickname || state.myShortID || t("stage.you")) : (p.nickname || peerName(p.peerId));
}

// stateLine — подстрока состояния: «Показывает экран · на сцене», «Говорит»,
// «Микрофон выключен», «Наушники выключены», «Громкость для меня 120%».
function stateLine(ctx, p, sharers) {
    const pid = p.peerId;
    const parts = [];
    let cls = "";
    if (sharers.indexOf(pid) >= 0) {
        parts.push(t("stage.state_sharing"));
        if (poppedOut.has(pid)) parts.push(t("stage.in_window"));
        else if (!split && pinned === pid) parts.push(t("stage.state_on_stage"));
        cls = "accent";
    }
    if (p.reconnecting) parts.push(t("stage.state_reconnecting"));
    else if (speaking.has(pid)) { parts.push(t("stage.state_speaking")); cls = cls || "green"; }
    else {
        const st = voiceState.getPeerStatus(pid) || {};
        if (st.deafened) parts.push(t("stage.state_deafened"));
        else if (st.muted) parts.push(t("stage.state_muted"));
    }
    if (!isSelf(pid) && parts.length === 0) {
        const scope = volumeScope(ctx);
        if (isPeerMuted(scope, pid)) parts.push(t("stage.state_muted_by_me"));
        else {
            const vol = getPeerVolume(scope, pid);
            if (vol !== 100) parts.push(t("stage.volume_for_me").replace("{n}", String(vol)));
        }
    }
    return { text: parts.join(" · "), cls };
}

function buildRow(ctx, p, sharers, hiddenInStrip) {
    const pid = p.peerId;
    const me = isSelf(pid);
    const name = displayName(p);
    const st = voiceState.getPeerStatus(pid) || {};
    const row = document.createElement("div");
    row.className = "stage-prow" + (speaking.has(pid) ? " is-speaking" : "") + (hiddenInStrip ? " is-hidden" : "");
    row.dataset.peerId = pid;
    const line = stateLine(ctx, p, sharers);
    let icons = "";
    if (sharers.indexOf(pid) >= 0) icons += '<i data-lucide="monitor" class="icon-14 accent"></i>';
    if (!me) {
        const scope = volumeScope(ctx);
        const vol = getPeerVolume(scope, pid);
        if (vol !== 100 && !isPeerMuted(scope, pid)) icons += '<span class="stage-prow-vol">' + vol + '%</span>';
    }
    if (st.muted) icons += '<i data-lucide="mic-off" class="icon-14 red"></i>';
    else icons += '<i data-lucide="mic" class="icon-14' + (speaking.has(pid) ? " green" : "") + '"></i>';
    if (st.deafened) icons += '<i data-lucide="headphone-off" class="icon-14 red"></i>';
    row.innerHTML =
        avatarHTML(pid, name, "stage-prow-avatar", 1) +
        '<div class="stage-prow-info">' +
            '<div class="stage-prow-name"><span>' + escapeHtml(name) + '</span>' + (me ? '<span class="me">' + escapeHtml(t("stage.you")) + '</span>' : "") + '</div>' +
            (line.text ? '<span class="stage-prow-state ' + line.cls + '">' + escapeHtml(line.text) + '</span>' : "") +
        '</div>' +
        '<div class="stage-prow-right">' + icons +
            (me ? '<span class="stage-prow-more-spacer"></span>' : '<button type="button" class="stage-prow-more" title="…"><i data-lucide="ellipsis" class="icon-14"></i></button>') +
        '</div>';
    paintAvatars(row);
    if (hiddenInStrip) {
        row.title = t("stage.hidden_hint");
        row.addEventListener("click", () => { filmstripPinned.add(pid); render(); });
    }
    if (!me) {
        row.addEventListener("contextmenu", (e) => showPeerMenu(e, pid));
        const more = row.querySelector(".stage-prow-more");
        more.addEventListener("click", (e) => {
            e.stopPropagation();
            const r = more.getBoundingClientRect();
            showPeerMenu({ clientX: r.left, clientY: r.bottom + 2, preventDefault() {}, stopPropagation() {} }, pid);
        });
    }
    return row;
}

function plistHead(text, hint) {
    const h = document.createElement("div");
    h.className = "stage-plist-head";
    h.innerHTML = "<span>" + escapeHtml(text) + "</span>" + (hint ? '<span class="hint">' + escapeHtml(hint) + "</span>" : "");
    return h;
}

function renderParticipants(ctx, sharers) {
    const box = q("info-tab-participants");
    if (!box) return;
    const parts = orderedParticipants(ctx, sharers);
    const countEl = q("info-tab-participants-count");
    if (countEl) countEl.textContent = String(parts.length);
    box.innerHTML = "";
    if (!stripVisible) {
        box.appendChild(plistHead(t("stage.participants_head").replace("{n}", String(parts.length))));
        parts.forEach((p) => box.appendChild(buildRow(ctx, p, sharers, false)));
    } else {
        const inStrip = parts.filter((p) => stripVisible.has(p.peerId));
        const hidden = parts.filter((p) => !stripVisible.has(p.peerId));
        box.appendChild(plistHead(t("stage.in_strip").replace("{n}", String(inStrip.length))));
        inStrip.forEach((p) => box.appendChild(buildRow(ctx, p, sharers, false)));
        if (hidden.length > 0) {
            box.appendChild(plistHead(t("stage.hidden_in_strip").replace("{n}", String(hidden.length)), t("stage.hidden_hint")));
            hidden.forEach((p) => box.appendChild(buildRow(ctx, p, sharers, true)));
        }
    }
    if (window.lucide) lucide.createIcons({ nodes: [box] });
}

// --- меню участника: написать / в друзья, тег, громкость, mute, исключить ---

function showPeerMenu(e, pid) {
    const ctx = stageContext();
    if (!ctx) return;
    const scope = volumeScope(ctx);
    const known = (state.peers || []).find((x) => x.id === pid);
    const p = participants(ctx).find((x) => x.peerId === pid) || { peerId: pid };
    const nick = known ? (known.nickname || known.shortId) : (p.nickname || pid.slice(0, 16));
    const shortId = known && known.shortId ? known.shortId : pid.substring(0, 16);
    const isFriend = friendIds ? friendIds.has(pid) : !!known;
    const muted = isPeerMuted(scope, pid);
    const items = [];
    if (isFriend) {
        items.push({ label: t("ctx.send_message"), icon: "message-square", action: () => {
            showMessagesList();
            selectPeer(pid, known || { id: pid, nickname: nick, shortId: shortId });
        } });
    } else {
        items.push({ label: t("ctx.add_friend"), icon: "user-plus", action: () => {
            svc("SendFriendRequest", pid).then(() => {
                showSuccess(t("toast.friend_request_sent").replace("{name}", nick));
            }).catch((err) => showError(String(err && err.message || err)));
        } });
    }
    items.push({ label: t("ctx.copy_tag"), icon: "copy", action: () => {
        try { navigator.clipboard.writeText(nick + "#" + shortId); } catch (err) { /* окно не в фокусе */ }
    } });
    items.push({ separator: true });
    items.push({
        label: t("ctx.volume_for_me"),
        volume: { value: muted ? 0 : getPeerVolume(scope, pid), min: 0, max: 200, onChange: (v) => {
            setPeerVolume(scope, pid, v);
            if (isPeerMuted(scope, pid) && v > 0) setPeerMuted(scope, pid, false);
        } },
    });
    items.push({
        label: muted ? t("ctx.unmute_for_me") : t("ctx.mute_for_me"),
        icon: muted ? "volume-2" : "volume-x",
        action: () => { setPeerMuted(scope, pid, !muted); render(); },
    });
    // Звук его демонстрации — отдельно от голоса (фаза 5e).
    if (hasShareAudio(pid)) {
        items.push({
            label: t("stage.share_volume"),
            volume: { value: getShareVolume(pid), min: 0, max: 200, onChange: (v) => setShareVolume(pid, v) },
        });
    }
    if (ctx.kind === "call" && state.call && state.call.creatorId === state.myID) {
        items.push({ separator: true });
        items.push({ label: t("info.kick"), icon: "user-minus", danger: true, action: () => {
            svc("KickFromCall", pid).catch((err) => flog("ERR", "stage", "KickFromCall: " + err));
            if (state.call) state.call.participants = state.call.participants.filter((x) => x.peerId !== pid);
            renderUserBar();
            render();
        } });
    }
    showContextMenu(e, items);
}

// --- контрол-бар ---

async function showDeviceMenu(e, kind) {
    const listMethod = kind === "mic" ? "GetMicrophones" : "GetSpeakers";
    const selMethod = kind === "mic" ? "GetSelectedMicrophone" : "GetSelectedSpeaker";
    const setMethod = kind === "mic" ? "SetMicrophone" : "SetSpeaker";
    let devices = [], selected = null;
    try {
        devices = (await svc(listMethod)) || [];
        selected = await svc(selMethod);
    } catch (err) { /* нет устройств */ }
    const items = devices.map((d) => ({
        label: (String(d.index) === String(selected) ? "✓ " : "") + d.name + (d.isDefault ? " · " + t("stage.device_default") : ""),
        action: () => svc(setMethod, d.index).catch(() => {}),
    }));
    if (items.length === 0) items.push({ label: "—", action: () => {} });
    showContextMenu(e, items);
}

// --- меню звонка: «…» в шапке и «Ещё» на панели (макет HeaderMenus) ---

function chatShown() {
    return dom.infoPanel.classList.contains("open") && chatTabActive();
}

function toggleChat() {
    if (chatShown()) document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
    else document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "chat" } }));
}

function openParticipants() {
    document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "participants" } }));
}

function otherIds(ctx) {
    return participants(ctx).filter((p) => !isSelf(p.peerId)).map((p) => p.peerId);
}

// «Заглушить всех для меня»: включён, когда заглушены все остальные.
// Выключение снимает заглушение с тех, кого заглушил этот пункт (если он
// никого не заглушал — со всех), ручные заглушения до пункта не трогаем.
function allMuted(ctx) {
    const scope = volumeScope(ctx);
    const others = otherIds(ctx);
    return others.length > 0 && others.every((pid) => isPeerMuted(scope, pid));
}

function toggleMuteAll(ctx) {
    const scope = volumeScope(ctx);
    if (allMuted(ctx)) {
        const set = mutedAllByMenu.get(scope);
        (set && set.size > 0 ? Array.from(set) : otherIds(ctx)).forEach((pid) => setPeerMuted(scope, pid, false));
        mutedAllByMenu.delete(scope);
    } else {
        const set = new Set();
        otherIds(ctx).forEach((pid) => {
            if (!isPeerMuted(scope, pid)) { setPeerMuted(scope, pid, true); set.add(pid); }
        });
        mutedAllByMenu.set(scope, set);
    }
    render();
}

// Шумоподавление тумблером: выкл = уровень 0, вкл = прежний уровень (или
// лучший доступный). Выбор уровня — в настройках «Звук».
const NS_PREV_KEY = "vox.ns_prev_level";

function toggleNoise(ns) {
    const cur = ns ? ns.level : 0;
    if (cur > 0) {
        try { localStorage.setItem(NS_PREV_KEY, String(cur)); } catch (err) { /* storage недоступен */ }
        svc("SetNoiseSuppression", 0).catch(() => {});
        return;
    }
    let prev = 0;
    try { prev = parseInt(localStorage.getItem(NS_PREV_KEY) || "", 10) || 0; } catch (err) { prev = 0; }
    if (!prev) prev = ns && ns.dfnAvailable ? 3 : ns && ns.rnnoiseAvailable ? 2 : 1;
    svc("SetNoiseSuppression", prev).catch(() => {});
}

function statsItem(ctx, mode) {
    return { label: t("call_menu.stats"), icon: "signal", sub: mode, action: () => showConnStats(() => participants(stageContext() || ctx).length) };
}

function soundSettingsItem() {
    return { label: t("call_menu.sound_settings"), icon: "headphones", action: () => openSettingsTab("sound") };
}

function chatItem() {
    return { label: t("call_menu.show_chat"), icon: "message-square", toggle: chatShown(), action: toggleChat };
}

function participantsItem(ctx) {
    return { label: t("call_menu.participants"), icon: "users", sub: String(participants(ctx).length), action: openParticipants };
}

// menuUnder — меню под кнопкой, правым краем к ней (как «…» шапки чата).
function menuUnder(anchor, items) {
    const r = anchor.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.bottom + 2, preventDefault() {}, stopPropagation() {} }, items);
    const menu = document.getElementById("context-menu");
    if (menu) menu.style.left = Math.max(4, r.right - menu.offsetWidth) + "px";
}

async function showHeaderMenu(anchor, ctx) {
    const mode = await refreshConnMode();
    const sharers = listSharers();
    const items = [];
    if (ctx.kind === "call") {
        items.push({ label: t("call_menu.invite"), icon: "user-plus", action: () => showAddParticipantDialog() });
    }
    items.push(participantsItem(ctx), chatItem(), { separator: true });
    const sharing = isScreenSharing();
    items.push({ label: sharing ? t("call.stop_share") : t("call.share_screen"), icon: sharing ? "monitor-off" : "monitor-up", action: () => toggleScreenShare() });
    if (sharers.length >= 2) {
        items.push({ label: t("stage.side_by_side"), icon: "layout-grid", toggle: split, action: () => { split = !split; manualCollapse = false; render(); } });
    }
    if (pinned && !split && !poppedOut.has(pinned) && !isSelf(pinned)) {
        items.push({ label: t("call_menu.popout_stage"), icon: "picture-in-picture-2", action: () => openPopout(pinned) });
    }
    items.push({ separator: true }, statsItem(ctx, mode), soundSettingsItem());
    if (ctx.kind === "call" && state.call && state.call.creatorId === state.myID) {
        items.push({ separator: true });
        items.push({ label: t("stage.end_for_all"), icon: "phone-off", danger: true, action: () => endCallForAll() });
    }
    menuUnder(anchor, items);
}

async function showMoreMenu(e, ctx) {
    const [ns, master, mode] = await Promise.all([
        svc("GetNoiseSuppression").catch(() => null),
        svc("GetMasterVoiceVolume").catch(() => 1),
        refreshConnMode(),
    ]);
    const items = [
        { label: t("call_menu.noise"), icon: "audio-lines", toggle: !!(ns && ns.level > 0), action: () => toggleNoise(ns) },
        { label: t("call_menu.mic_test"), icon: "mic", action: () => { openSettingsTab("sound"); startMicTest(); } },
    ];
    if (otherIds(ctx).length > 0) {
        items.push({ label: t("call_menu.mute_all"), icon: "volume-x", toggle: allMuted(ctx), action: () => toggleMuteAll(ctx) });
    }
    items.push({
        label: t("stage.master_volume"),
        volume: { value: Math.round((typeof master === "number" ? master : 1) * 100), min: 0, max: 140, onChange: (v) => svc("SetMasterVoiceVolume", v / 100).catch(() => {}) },
    });
    items.push({ separator: true }, statsItem(ctx, mode), soundSettingsItem());
    items.push({ separator: true }, chatItem(), participantsItem(ctx));
    showContextMenu(e, items);
}

// Меню плитки демонстрации (макет ContextMenus «Плитка экрана»).
function showShareTileMenu(e, pid) {
    const me = isSelf(pid);
    const toStage = () => { split = false; manualCollapse = false; pinned = pid; render(); };
    const items = [{ label: t("tile_menu.on_stage"), icon: "maximize", action: toStage }];
    if (!me) {
        items.push(poppedOut.has(pid)
            ? { label: t("stage.popout_back"), icon: "undo-2", action: () => closePopout(pid) }
            : { label: t("stage.popout"), icon: "picture-in-picture-2", action: () => openPopout(pid) });
    }
    items.push({ label: t("stage.fullscreen"), icon: "maximize-2", action: () => { toStage(); stageFullscreen(); } });
    if (me) {
        items.push({ separator: true });
        items.push({ label: t("call.stop_share"), icon: "monitor-off", danger: true, action: () => toggleScreenShare() });
    }
    showContextMenu(e, items);
}

function leave(ctx) {
    if (poppedOut.size) svc("CloseAllViewerWindows").catch(() => {});
    if (ctx.kind === "call") { endCall(); return; }
    leaveVoice().then(() => {
        voiceState.resetSelf();
        activeVoiceGroup = null;
        voiceJoinedAt = 0;
        voiceParticipants = [];
        renderUserBar();
        updateVoiceHeaderBtn();
        render();
    });
}

// --- отдельное окно демонстрации (screen-view.html, ScreenShareService.OpenViewerWindow) ---
// Окно получает те же vox:screen-frame и декодирует само; здесь декодер пира
// приостанавливаем (setDecodeSuspended), на сцене — плейсхолдер. Состояние
// голоса остаётся в этом окне: окно шлёт команды через vox:screen-viewer-cmd,
// мы отдаём ему muted/deafened/таймер через ViewerState.

function buildPopoutPlaceholder(pid) {
    const ph = document.createElement("div");
    ph.className = "stage-popout-placeholder";
    ph.dataset.peer = pid;
    ph.innerHTML = '<i data-lucide="picture-in-picture-2" class="icon-32"></i>' +
        '<span>' + escapeHtml(t("stage.popout_placeholder")) + '</span>' +
        '<button type="button" class="stage-popout-return"><i data-lucide="undo-2" class="icon-14"></i>' + escapeHtml(t("stage.popout_return")) + '</button>';
    ph.querySelector(".stage-popout-return").addEventListener("click", () => closePopout(pid));
    return ph;
}

// Окон может быть несколько — по одному на пира (фаза 7e).
function openPopout(pid) {
    const ctx = stageContext();
    if (!ctx || !pid) return;
    const startedAt = ctx.kind === "call" ? ((state.call && state.call.startedAt) || 0) : voiceJoinedAt;
    const theme = document.documentElement.getAttribute("data-theme") || "light";
    svc("OpenViewerWindow", pid, peerLabel(pid), sessionName(ctx), startedAt, theme).then(() => {
        poppedOut.add(pid);
        setDecodeSuspended(pid, true);
        render();
        pushViewerState();
    }).catch((err) => flog("ERR", "stage", "OpenViewerWindow: " + (err && err.message || err)));
}

function closePopout(pid) {
    svc("CloseViewerWindow", pid).catch(() => {});
}

// onPopoutClosed — окно пира закрыто любым путём (vox:screen-viewer-closed).
function onPopoutClosed(pid) {
    if (!pid || !poppedOut.has(pid)) return;
    poppedOut.delete(pid);
    setDecodeSuspended(pid, false);
    if (mounted) render();
}

// pushViewerState — общее состояние всем окнам; подпись у каждого своя (URL).
function pushViewerState() {
    if (!poppedOut.size) return;
    const ctx = stageContext() || (state.call ? { kind: "call" } : (activeVoiceGroup ? { kind: "voice", groupId: activeVoiceGroup } : null));
    const my = voiceState.getMyStatus() || {};
    const startedAt = !ctx ? 0 : ctx.kind === "call" ? ((state.call && state.call.startedAt) || 0) : voiceJoinedAt;
    svc("ViewerState", {
        muted: !!my.muted,
        deafened: !!my.deafened,
        session: ctx ? sessionName(ctx) : "",
        startedAt: startedAt,
    }).catch(() => {});
}

function onViewerCommand(cmd, pid) {
    const ctx = stageContext() || (state.call ? { kind: "call" } : (activeVoiceGroup ? { kind: "voice", groupId: activeVoiceGroup } : null));
    switch (cmd) {
        case "mute": voiceState.toggleMute(); break;
        case "deafen": voiceState.toggleDeafen(); break;
        case "back": if (pid) closePopout(pid); break;
        case "leave": if (ctx) leave(ctx); break;
    }
}

function stageFullscreen() {
    const c = el.body.querySelector("canvas.screen-canvas");
    if (c && c.requestFullscreen) c.requestFullscreen();
}

// --- таймер ---
// Секундный тик живёт, пока есть звонок/канал (не только пока сцена видна):
// в чужом чате по нему обновляется таймер полоски (call-bar.js).

function syncTick() {
    if (getActiveSession()) startTick(); else stopTick();
}

function startTick() {
    if (tickTimer) return;
    tickTimer = setInterval(() => {
        if (!getActiveSession()) { stopTick(); return; }
        const ctx = stageContext();
        if (ctx && mounted) {
            updateTimer(ctx);
            updateSignal(ctx);
        }
        if (poppedOut.size) pushViewerState();
        document.dispatchEvent(new CustomEvent("vox:ui-stage-tick"));
    }, 1000);
}

function stopTick() {
    if (tickTimer) { clearInterval(tickTimer); tickTimer = null; }
}

// --- init ---

export function initStage(events) {
    el.stage = q("call-stage");
    el.body = q("stage-body");
    el.bar = q("stage-bar");
    el.filmstrip = q("stage-filmstrip");
    if (!el.stage) return;

    q("stage-mic").addEventListener("click", () => voiceState.toggleMute());
    q("stage-sound").addEventListener("click", () => voiceState.toggleDeafen());
    q("stage-mic-menu").addEventListener("click", (e) => showDeviceMenu(e, "mic"));
    q("stage-sound-menu").addEventListener("click", (e) => showDeviceMenu(e, "speaker"));
    // #stage-share кликом занимается screenshare.js (SHARE_BTN_IDS)
    q("stage-more").addEventListener("click", (e) => { const ctx = stageContext(); if (ctx) showMoreMenu(e, ctx); });
    q("stage-leave").addEventListener("click", () => { const ctx = stageContext(); if (ctx) leave(ctx); });
    q("stage-invite").addEventListener("click", () => showAddParticipantDialog());
    q("stage-header-more").addEventListener("click", (e) => {
        e.stopPropagation();
        const ctx = stageContext();
        if (ctx) showHeaderMenu(q("stage-header-more"), ctx);
    });
    q("stage-chat-toggle").addEventListener("click", () => {
        const btn = dom.infoToggleBtn;
        if (btn) btn.click();
        q("stage-chat-toggle").classList.toggle("active", dom.infoPanel.classList.contains("open"));
    });
    q("stage-fullscreen").addEventListener("click", stageFullscreen);
    q("stage-popout").addEventListener("click", () => { if (pinned && !split) openPopout(pinned); });
    q("stage-collapse").addEventListener("click", () => { manualCollapse = true; pinned = null; split = false; render(); });
    q("stage-split").addEventListener("click", () => { split = !split; manualCollapse = false; render(); });

    document.addEventListener("vox:ui-stage-refresh", () => {
        // обновление может прийти до того, как GetActiveVoiceGroup ответил — рендерим сразу и после
        render();
        if (!state.call) refreshVoice();
    });
    document.addEventListener("vox:screen-tile-added", () => { manualCollapse = false; render(); });
    document.addEventListener("vox:screen-tile-removed", (e) => {
        const pid = e.detail && e.detail.peerId;
        if (pid && sinkCanvases.has(pid)) sinkCanvases.delete(pid);
        if (pid && poppedOut.has(pid)) closePopout(pid); // окно само закроется по vox:screen-share-ended, это страховка
        render();
    });

    // «+N» в ленте → вкладка «Участники»
    document.addEventListener("vox:ui-open-participants", () => {
        document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "participants" } }));
    });
    // Вкладка «Чат» показана / панель доехала по ширине → лента к низу
    document.addEventListener("vox:ui-chat-tab-shown", () => { if (panelMode) scrollToBottom(); });
    dom.infoPanel.addEventListener("transitionend", (e) => {
        if (e.propertyName === "width" && panelMode && chatTabActive()) scrollToBottom();
    });

    voiceState.on("self-changed", () => { if (mounted) render(); if (poppedOut.size) pushViewerState(); });
    voiceState.on("peer-changed", () => { if (mounted) render(); });
    voiceState.on("peer-removed", () => { if (mounted) render(); });

    if (events && events.On) {
        events.On("vox:voice-activity", (e) => {
            const d = e && e.data;
            if (!d || !d.peerId) return;
            if (d.speaking) speaking.add(d.peerId); else speaking.delete(d.peerId);
            if (state.call) {
                const p = state.call.participants.find((x) => x.peerId === d.peerId);
                if (p) p.speaking = !!d.speaking;
            }
            if (!mounted) return;
            el.stage.querySelectorAll('[data-peer-id="' + d.peerId + '"]').forEach((node) => {
                node.classList.toggle("is-speaking", !!d.speaking);
                const st = node.querySelector(".stage-person-state");
                if (st && !node.classList.contains("is-reconnecting")) {
                    const p = participants(stageContext() || { kind: "voice" }).find((x) => x.peerId === d.peerId) || { peerId: d.peerId };
                    st.textContent = personState(p);
                }
            });
        });
        events.On("vox:voice-join", (e) => {
            const d = e && e.data;
            if (d && d.peerId === state.myID && !voiceJoinedAt) voiceJoinedAt = Date.now();
            refreshVoice();
        });
        events.On("vox:voice-leave", (e) => {
            const d = e && e.data;
            if (d && d.peerId) speaking.delete(d.peerId);
            refreshVoice();
        });
        events.On("vox:voice-status", () => { if (activeVoiceGroup) refreshVoice(); });
        events.On("vox:call-ended", () => { speaking.clear(); if (poppedOut.size) svc("CloseAllViewerWindows").catch(() => {}); render(); });
        // Окно демонстрации пира (screen-view.html) закрыто любым путём → вернуть на сцену
        events.On("vox:screen-viewer-closed", (e) => onPopoutClosed(e && e.data && e.data.peerId));
        events.On("vox:screen-viewer-cmd", (e) => { const d = e && e.data; if (d && d.cmd) onViewerCommand(d.cmd, d.peerId); });
    }

    // Стартовое состояние (перезапуск приложения в голосовом канале)
    refreshVoice();
}
