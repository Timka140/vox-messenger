"use strict";

// calls.js — Call management module.
// Handles user-bar states, call lifecycle (start/accept/end, participants),
// history in the friend profile (profile.js), and ringtone. Сцена звонка и панель участников —
// stage.js (связь через DOM-событие vox:ui-stage-refresh). Входящий звонок
// показывает окно уведомлений (NotifyService), in-app модалки больше нет.

import { svc, flog, state, dom, colorFromId, formatMmSs } from "./core.js";
import { t } from "./i18n.js";
import { leaveVoice } from "./voice.js";
import { buildDialogOverlay, buildFriendCheckboxList, getSelectedIds } from "./ui.js";
import { showContextMenu } from "./contextmenu.js";
import { voiceState } from "./voice-state.js";

import { showInfo } from "./toast.js";
import { soundGain } from "./sounds.js";
var _callTimer = null;
var _ringtoneInterval = null;
var _ringtoneCtx = null;
var _outRingInterval = null;
var _outRingCtx = null;

// Callback to refresh info panel (set from main.js to avoid circular deps)
var _refreshInfoPanel = null;
export function setCallRefreshInfoPanel(fn) { _refreshInfoPanel = fn; }

var _switchInfoTab = null;
export function setCallSwitchInfoTab(fn) { _switchInfoTab = fn; }

var _openInfoPanel = null;
export function setCallOpenInfoPanel(fn) { _openInfoPanel = fn; }

// --- Локальные функции обновления кнопок (вызываются из observer, не экспортируются) ---

function syncMuteButtonsLocal(muted) {
    var targets = [
        { el: document.getElementById("user-bar-voice-mute"), size: 16 }
    ];
    var icon = muted ? "mic-off" : "mic";
    for (var i = 0; i < targets.length; i++) {
        var tgt = targets[i];
        if (!tgt.el) continue;
        tgt.el.classList.toggle("muted", muted);
        if (tgt.withLabel) {
            var label = muted ? t("call.unmute") : t("call.mute");
            tgt.el.innerHTML = '<i data-lucide="' + icon + '" class="icon-' + tgt.size + '"></i><span>' + label + '</span>';
        } else {
            tgt.el.innerHTML = '<i data-lucide="' + icon + '" class="icon-' + tgt.size + '"></i>';
        }
        if (window.lucide) lucide.createIcons({ nodes: [tgt.el] });
    }
}

function syncDeafenButtonsLocal(deafened) {
    var targets = [
        { el: document.getElementById("user-bar-voice-deafen"), size: 16 }
    ];
    var icon = deafened ? "headphone-off" : "headphones";
    for (var i = 0; i < targets.length; i++) {
        var tgt = targets[i];
        if (!tgt.el) continue;
        tgt.el.classList.toggle("muted", deafened);
        if (tgt.withLabel) {
            var label = deafened ? t("call.undeafen") : t("call.deafen");
            tgt.el.innerHTML = '<i data-lucide="' + icon + '" class="icon-' + tgt.size + '"></i><span>' + label + '</span>';
        } else {
            tgt.el.innerHTML = '<i data-lucide="' + icon + '" class="icon-' + tgt.size + '"></i>';
        }
        if (window.lucide) lucide.createIcons({ nodes: [tgt.el] });
    }
}

function syncUserBarCallMute(muted) {
    var userBarMute = document.getElementById("user-bar-call-mute");
    if (userBarMute) {
        userBarMute.classList.toggle("muted", muted);
        userBarMute.innerHTML = '<i data-lucide="' + (muted ? "mic-off" : "mic") + '" class="icon-16"></i>';
        if (window.lucide) lucide.createIcons({ nodes: [userBarMute] });
    }
}

// --- Init ---

export function initCalls() {
    // --- VoiceState observers: автоматическое обновление UI при изменении состояния ---
    voiceState.on("self-changed", function (status) {
        syncMuteButtonsLocal(status.muted);
        syncDeafenButtonsLocal(status.deafened);
        syncUserBarCallMute(status.muted);
    });
    // peer-changed слушает stage.js (сцена + панель участников)

    // User-bar call buttons
    var callEndBtn = document.getElementById("user-bar-call-end");
    var callMuteBtn = document.getElementById("user-bar-call-mute");
    var userBarCall = document.getElementById("user-bar-call");

    if (callEndBtn) {
        callEndBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            endCall();
        });
    }
    if (callMuteBtn) {
        callMuteBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            voiceState.toggleMute();
        });
    }
    if (userBarCall) {
        userBarCall.addEventListener("click", function () {
            // К сцене звонка: чат-владелец открывает call-bar.js
            if (state.call) document.dispatchEvent(new CustomEvent("vox:ui-goto-session"));
        });
    }

    // User-bar voice buttons
    var voiceMuteBtn = document.getElementById("user-bar-voice-mute");
    var voiceDisconnectBtn = document.getElementById("user-bar-voice-disconnect");

    if (voiceMuteBtn) {
        voiceMuteBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            voiceState.toggleMute();
        });
    }

    if (voiceDisconnectBtn) {
        voiceDisconnectBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            leaveVoice().then(function () {
                voiceState.resetSelf();
                renderUserBar();
            });
        });
    }

    // User-bar voice deafen
    var voiceDeafenBtn = document.getElementById("user-bar-voice-deafen");
    if (voiceDeafenBtn) {
        voiceDeafenBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            voiceState.toggleDeafen();
        });
    }

    // Call button in chat header
    if (dom.callBtn) {
        dom.callBtn.addEventListener("click", function () {
            if (state.selectedPeerID) {
                startCall(state.selectedPeerID);
            }
        });
    }

    renderUserBar();
}

// --- User Bar Rendering ---

export function renderUserBar() {
    var normal = dom.userBarNormal;
    var call = dom.userBarCall;
    var voice = dom.userBarVoice;
    if (!normal || !call || !voice) return;

    // Hide all states
    normal.style.display = "none";
    call.style.display = "none";
    voice.style.display = "none";

    if (state.call) {
        // Call state (включая дозвон: startedAt === 0)
        call.style.display = "flex";
        updateCallTimer();
        var countEl = document.getElementById("user-bar-call-count");
        if (countEl) {
            var count = state.call.participants ? state.call.participants.length : 0;
            countEl.textContent = count + " " + t("call.participants");
        }
        if (window.lucide) lucide.createIcons({ nodes: [call] });
        // Восстановить состояние кнопок mute/deafen из voiceState
        var s = voiceState.getMyStatus();
        syncMuteButtonsLocal(s.muted);
        syncDeafenButtonsLocal(s.deafened);
        syncUserBarCallMute(s.muted);
    } else {
        // Check if in voice channel
        svc("GetActiveVoiceGroup").then(function (activeGroup) {
            if (activeGroup) {
                voice.style.display = "flex";
                var nameEl = document.getElementById("user-bar-voice-name");
                var grp = state.groups.find(function (g) { return g.groupId === activeGroup; });
                if (nameEl && grp) nameEl.textContent = grp.name;
                if (window.lucide) lucide.createIcons({ nodes: [voice] });
                // Восстановить состояние кнопок mute/deafen из voiceState
                var vs = voiceState.getMyStatus();
                syncMuteButtonsLocal(vs.muted);
                syncDeafenButtonsLocal(vs.deafened);
            } else {
                normal.style.display = "flex";
                if (window.lucide) lucide.createIcons({ nodes: [normal] });
            }
        }).catch(function () {
            normal.style.display = "flex";
        });
    }
}

// --- Call Timer ---

function updateCallTimer() {
    if (!state.call) return;
    var timerEl = document.getElementById("user-bar-call-timer");
    var formatted;
    if (!state.call.startedAt) {
        // Дозвон — таймер ещё не запущен
        formatted = t("call.calling");
    } else {
        var elapsed = Date.now() - state.call.startedAt;
        formatted = formatMmSs(elapsed);
    }
    if (timerEl) timerEl.textContent = formatted;
}

function startCallTimer() {
    stopCallTimer();
    _callTimer = setInterval(updateCallTimer, 1000);
}

function stopCallTimer() {
    if (_callTimer) {
        clearInterval(_callTimer);
        _callTimer = null;
    }
}

// currentChatKey — ключ открытого чата ("peer:<id>" | "group:<id>" | "").
// Записывается в state.call.ownerKey при старте/приёме звонка: сцена (stage.js)
// показывается только в этом чате. У ad-hoc групповых звонков своего чата нет —
// владельцем становится чат, из которого звонок начали (или DM с инициатором).
export function currentChatKey() {
    if (state.selectedGroupID) return "group:" + state.selectedGroupID;
    if (state.selectedPeerID) return "peer:" + state.selectedPeerID;
    return "";
}

function stageRefresh() {
    document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
}

// --- Start Call ---

var _startingCall = false;

export function startCall(peerId) {
    // Guard от двойного клика: без него второй StartCall перезаписывал state.call
    // вторым звонком, а бэк делал leaveCallInternal первого → рассинхрон UI/бэка.
    if (_startingCall || state.call) return;
    _startingCall = true;

    var peer = state.peers.find(function (p) { return p.id === peerId; });
    var nickname = peer ? (peer.nickname || peer.shortId) : peerId.substring(0, 16);

    svc("StartCall", [peerId]).then(function (result) {
        if (!result) return;

        state.call = {
            id: result.callId,
            type: result.type,
            peerId: peerId,
            groupId: null,
            ownerKey: "peer:" + peerId,
            participants: [
                { peerId: state.myID, nickname: state.myNickname || state.myShortID, speaking: false, muted: false },
                { peerId: peerId, nickname: nickname, speaking: false, muted: false }
            ],
            creatorId: state.myID,
            startedAt: 0 // будет выставлен в onCallAccepted
        };

        startCallTimer();
        playOutgoingRing();
        renderUserBar();
        updateInfoTabs();
        stageRefresh();

        stageRefresh();
    }).catch(function (err) {
        flog("ERR", "calls", "startCall error: " + err);
    }).finally(function () {
        _startingCall = false;
    });

    flog("INF", "calls", "startCall to " + peerId.substring(0, 16));
}

export function startGroupCall(participantIds) {
    svc("StartCall", participantIds).then(function (result) {
        if (!result) return;

        var participants = [
            { peerId: state.myID, nickname: state.myNickname || state.myShortID, speaking: false, muted: false }
        ];
        for (var i = 0; i < participantIds.length; i++) {
            var pid = participantIds[i];
            var peer = state.peers.find(function (p) { return p.id === pid; });
            participants.push({
                peerId: pid,
                nickname: peer ? (peer.nickname || peer.shortId) : pid.substring(0, 16),
                speaking: false,
                muted: false
            });
        }

        state.call = {
            id: result.callId,
            type: "group",
            peerId: null,
            groupId: null,
            ownerKey: currentChatKey(),
            participants: participants,
            creatorId: state.myID,
            startedAt: 0 // будет выставлен в onCallAccepted при первом ответе
        };

        startCallTimer();
        playOutgoingRing();
        renderUserBar();
        updateInfoTabs();
        stageRefresh();

        stageRefresh();
    }).catch(function (err) {
        flog("ERR", "calls", "startGroupCall error: " + err);
    });

    flog("INF", "calls", "startGroupCall with " + participantIds.length + " participants");
}

// --- End Call ---

export function endCall() {
    // Если звонок активен — уведомляем бэкенд
    if (state.call) {
        svc("LeaveCall").catch(function (err) {
            flog("ERR", "calls", "leaveCall error: " + err);
        });
    }

    stopCallTimer();
    stopRingtone();
    stopOutgoingRing();
    state.call = null;

    voiceState.resetSelf(); // observer обновит все кнопки автоматически

    renderUserBar();
    updateInfoTabs();
    stageRefresh();

    // Обновляем info panel: показать инфо выбранного чата или закрыть если ничего не выбрано
    if (state.selectedPeerID || state.selectedGroupID) {
        if (_switchInfoTab) _switchInfoTab("info");
        document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    } else {
        document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
    }

    flog("INF", "calls", "endCall");
}

export function endCallForAll() {
    svc("EndCallForAll").catch(function (err) {
        flog("ERR", "calls", "endCallForAll error: " + err);
    });

    stopCallTimer();
    stopRingtone();
    stopOutgoingRing();
    state.call = null;

    voiceState.resetSelf(); // observer обновит все кнопки автоматически

    renderUserBar();
    updateInfoTabs();
    stageRefresh();

    if (state.selectedPeerID || state.selectedGroupID) {
        if (_switchInfoTab) _switchInfoTab("info");
        document.dispatchEvent(new CustomEvent("vox:ui-refresh-info-panel"));
    } else {
        document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
    }

    flog("INF", "calls", "endCallForAll");
}

// toggleMute / toggleDeafen / _mutedBeforeDeafen — перенесены в voice-state.js

// --- Info Panel Tab Visibility ---

// updateInfoTabs — какие вкладки видны в правой панели: во время сцены
// звонка (#chat-area.stage-active, stage.js) — Чат | Участники | Медиа,
// иначе — Инфо | Медиа. Если активная вкладка спряталась — переключаемся.
export function updateInfoTabs() {
    if (!dom.infoPanel) return;
    var chatArea = document.getElementById("chat-area");
    var stageActive = !!chatArea && chatArea.classList.contains("stage-active");
    var visible = { chat: stageActive, participants: stageActive, info: !stageActive, media: true };
    var tabs = dom.infoPanel.querySelectorAll(".info-tab");
    for (var i = 0; i < tabs.length; i++) {
        var name = tabs[i].dataset.infoTab;
        tabs[i].style.display = visible[name] === false ? "none" : "";
    }
    var activeTab = dom.infoPanel.querySelector(".info-tab.active");
    if ((!activeTab || activeTab.style.display === "none") && _switchInfoTab) {
        _switchInfoTab(stageActive ? "participants" : "info");
    }
}

// --- Call Tab Rendering ---

// --- Add participant dialog ---

export async function showAddParticipantDialog() {
    if (!state.call) return;

    // Exclude current participants
    var currentIds = {};
    for (var i = 0; i < state.call.participants.length; i++) {
        currentIds[state.call.participants[i].peerId] = true;
    }

    var friends = await svc("GetFriends") || [];
    // Фильтруем: только те, кто не в звонке
    var available = friends.filter(function (f) { return !currentIds[f.peerId]; });

    var membersList = buildFriendCheckboxList(available);

    var dlg = buildDialogOverlay(
        t("call.add_person"),
        "",
        t("group.cancel"),
        t("call.add_person"),
        function (overlay) {
            var ids = getSelectedIds(membersList);
            if (ids.length === 0) { overlay.remove(); return; }

            svc("AddToCall", ids).then(function () {
                // Участники добавятся через vox:call-participant-joined event
                if (state.call && ids.length > 0) {
                    state.call.type = "group";
                }
            }).catch(function (err) {
                flog("ERR", "calls", "addToCall error: " + err);
            });

            overlay.remove();
            flog("INF", "calls", "addParticipants: " + ids.length);
        }
    );

    dlg.body.appendChild(membersList);
    if (window.lucide) lucide.createIcons({ nodes: [membersList] });
}

// --- Group Call Dialog ---
// Страницы «Звонки» больше нет: вход — «В группу» из профиля друга
// (profile.js) с предвыбранным другом.

export async function showGroupCallDialog(preselectIds) {
    var friends = await svc("GetFriends") || [];
    var membersList = buildFriendCheckboxList(friends);
    if (preselectIds && preselectIds.length) {
        var inputs = membersList.querySelectorAll("input[type=checkbox]");
        for (var pi = 0; pi < inputs.length; pi++) {
            var pid = inputs[pi].value || (inputs[pi].dataset && inputs[pi].dataset.peerId) || "";
            if (preselectIds.indexOf(pid) >= 0 && !inputs[pi].checked) inputs[pi].click();
        }
    }

    var dlg = buildDialogOverlay(
        t("call.group_call"),
        "",
        t("group.cancel"),
        t("call.group_call"),
        function (overlay) {
            var ids = getSelectedIds(membersList);
            if (ids.length > 0) {
                startGroupCall(ids);
            }
            overlay.remove();
        }
    );

    dlg.body.appendChild(membersList);
    if (window.lucide) lucide.createIcons({ nodes: [membersList] });
}

// --- Ringtone ---

// Экспорт для events.js: в режиме своих уведомлений карточка звонка живёт в
// окне notify.html без фокуса, где AudioContext может не стартовать —
// рингтон играет главное окно.
export function playRingtone() {
    stopRingtone();
    var vol = 0.15 * soundGain("call"); // «Звуки событий» → «Звонок»
    if (!(vol > 0)) return;
    try {
        _ringtoneCtx = new (window.AudioContext || window.webkitAudioContext)();
    } catch (e) { return; }

    function beep() {
        if (!_ringtoneCtx) return;
        // Resume if suspended (browser autoplay policy)
        if (_ringtoneCtx.state === "suspended") {
            _ringtoneCtx.resume().catch(function () {});
        }
        try {
            var osc = _ringtoneCtx.createOscillator();
            var gain = _ringtoneCtx.createGain();
            osc.connect(gain);
            gain.connect(_ringtoneCtx.destination);
            osc.frequency.value = 440;
            gain.gain.setValueAtTime(vol, _ringtoneCtx.currentTime);
            gain.gain.exponentialRampToValueAtTime(0.01, _ringtoneCtx.currentTime + 0.8);
            osc.start(_ringtoneCtx.currentTime);
            osc.stop(_ringtoneCtx.currentTime + 0.8);
        } catch (e) {}
    }

    beep();
    _ringtoneInterval = setInterval(beep, 2000);
}

export function stopRingtone() {
    if (_ringtoneInterval) {
        clearInterval(_ringtoneInterval);
        _ringtoneInterval = null;
    }
    if (_ringtoneCtx) {
        _ringtoneCtx.close().catch(function () {});
        _ringtoneCtx = null;
    }
}

// --- Outgoing ring (для звонящего) ---

function playOutgoingRing() {
    stopOutgoingRing();
    var vol = 0.15 * soundGain("call");
    if (!(vol > 0)) return;
    try {
        _outRingCtx = new (window.AudioContext || window.webkitAudioContext)();
    } catch (e) { return; }

    function ringTone() {
        if (!_outRingCtx) return;
        if (_outRingCtx.state === "suspended") {
            _outRingCtx.resume().catch(function () {});
        }
        try {
            var osc = _outRingCtx.createOscillator();
            var gain = _outRingCtx.createGain();
            osc.connect(gain);
            gain.connect(_outRingCtx.destination);
            osc.frequency.value = 440;
            gain.gain.setValueAtTime(vol, _outRingCtx.currentTime);
            gain.gain.exponentialRampToValueAtTime(0.01, _outRingCtx.currentTime + 0.8);
            osc.start(_outRingCtx.currentTime);
            osc.stop(_outRingCtx.currentTime + 0.8);
        } catch (e) {}
    }

    ringTone();
    _outRingInterval = setInterval(ringTone, 2000);
}

function stopOutgoingRing() {
    if (_outRingInterval) {
        clearInterval(_outRingInterval);
        _outRingInterval = null;
    }
    if (_outRingCtx) {
        _outRingCtx.close().catch(function () {});
        _outRingCtx = null;
    }
}

// --- Event handlers (called from events.js) ---

export function onCallAccepted(data) {
    flog("INF", "calls", "onCallAccepted, hasState=" + !!state.call);
    stopOutgoingRing();

    // Acceptor: state.call ещё не установлен в главном окне (был в попапе)
    if (!state.call && data && data.callId) {
        var peerNick = data.nickname || (data.peerId ? data.peerId.substring(0, 16) : "?");
        state.call = {
            id: data.callId,
            type: "dm",
            peerId: data.peerId,
            groupId: null,
            ownerKey: "peer:" + data.peerId,
            participants: [
                { peerId: state.myID, nickname: state.myNickname || state.myShortID, speaking: false, muted: false },
                { peerId: data.peerId, nickname: peerNick, speaking: false, muted: false }
            ],
            creatorId: data.peerId,
            startedAt: Date.now()
        };

        startCallTimer();
        renderUserBar();
        updateInfoTabs();
        stageRefresh();

        stageRefresh();
        return;
    }

    // Initiator: state.call уже есть — запускаем таймер при первом принятии
    if (state.call && !state.call.startedAt) {
        state.call.startedAt = Date.now();
        updateCallTimer();
        renderUserBar();
    }
    stageRefresh();
}

export function onCallRejected(data) {
    var who = (data && data.peerId) || "";
    flog("INF", "calls", "onCallRejected by " + (who || "?").substring(0, 16));
    if (!state.call) return;
    var rejected = state.call.participants.find(function (p) { return p.peerId === who; });
    // Убираем отклонившего из списка. Если остались только мы — бэк сам
    // пришлёт vox:call-ended (endCallInternal("missed")), UI закроется там.
    state.call.participants = state.call.participants.filter(function (p) { return p.peerId !== who; });
    var name = (rejected && rejected.nickname) ? rejected.nickname : who.substring(0, 16);
    showInfo(name + " " + t("call.rejected_by"));
    stageRefresh();
    renderUserBar();
}

export function onParticipantJoined(data) {
    if (!state.call) return;
    // Проверка на дубликат — если уже есть, обновляем nickname если пришёл лучший
    var existing = state.call.participants.find(function (p) { return p.peerId === data.peerId; });
    if (existing) {
        // Обновить nickname если текущий — fallback substring(peerId), а пришёл реальный ник
        if (data.nickname && data.nickname !== data.peerId.substring(0, 16)) {
            var isCurrentFallback = existing.nickname === data.peerId.substring(0, 16) || !existing.nickname;
            if (isCurrentFallback || existing.nickname !== data.nickname) {
                existing.nickname = data.nickname;
                stageRefresh();
            }
        }
        return;
    }
    state.call.participants.push({
        peerId: data.peerId,
        nickname: data.nickname || data.peerId.substring(0, 16),
        speaking: false,
        muted: false
    });
    stageRefresh();
    renderUserBar();
}

export function onParticipantLeft(data) {
    if (!state.call) return;
    state.call.participants = state.call.participants.filter(function (p) {
        return p.peerId !== data.peerId;
    });
    stageRefresh();
    renderUserBar();

    // If only me left, end the call
    if (state.call.participants.length <= 1) {
        endCall();
    }
}

export function onParticipantReconnecting(data) {
    if (!state.call) return;
    var p = state.call.participants.find(function (p) { return p.peerId === data.peerId; });
    if (p) {
        p.reconnecting = true;
        stageRefresh();
        renderUserBar();
    }
    playCallBeep(300, 0.15, 0.4); // низкий тон — отключение
    flog("INF", "calls", "participant reconnecting: " + (data.peerId || "?").substring(0, 16));
}

export function onParticipantReconnected(data) {
    if (!state.call) return;
    var p = state.call.participants.find(function (p) { return p.peerId === data.peerId; });
    if (p) {
        p.reconnecting = false;
        stageRefresh();
        renderUserBar();
    }
    playCallBeep(600, 0.15, 0.3); // высокий тон — вернулся
    flog("INF", "calls", "participant reconnected: " + (data.peerId || "?").substring(0, 16));
}

function playCallBeep(freq, volume, duration) {
    volume *= soundGain("call");
    if (!(volume > 0)) return;
    try {
        var ctx = new (window.AudioContext || window.webkitAudioContext)();
        if (ctx.state === "suspended") ctx.resume().catch(function () {});
        var osc = ctx.createOscillator();
        var gain = ctx.createGain();
        osc.connect(gain);
        gain.connect(ctx.destination);
        osc.frequency.value = freq;
        gain.gain.setValueAtTime(volume, ctx.currentTime);
        gain.gain.exponentialRampToValueAtTime(0.01, ctx.currentTime + duration);
        osc.start(ctx.currentTime);
        osc.stop(ctx.currentTime + duration);
        osc.onended = function () { ctx.close().catch(function () {}); };
    } catch (e) {}
}
