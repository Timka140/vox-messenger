"use strict";

import { Events, svc, flog, logEvent, state, dom, colorFromId, SINGLE_CHECK_SVG, DOUBLE_CHECK_SVG, cssEscape, formatMmSs } from "./core.js";
import { isBlocked } from "./blocked.js";
import { t } from "./i18n.js";
import { appendMessage, isNearBottom, scrollToBottom, markEdited, forgetMessage, markSendFailed } from "./messages.js";
import { playIconSvg, togglePlayVoice } from "./voice.js";
import { refreshPeers, selectGroup, selectPeer, showMessagesList, showGroupsList } from "./contacts-list.js";
import { loadSignalServers, loadTURNConfig, applyBitrateSnapshot } from "./settings.js";
import { refreshInfoPanel } from "./info-panel.js";
import { applyGroupVolumes } from "./peer-volume.js";
import { endCall, onCallAccepted, onCallRejected, onParticipantJoined, onParticipantLeft, onParticipantReconnecting, onParticipantReconnected, renderUserBar, playRingtone, stopRingtone } from "./calls.js";
import { voiceState } from "./voice-state.js";
import { playNotificationSound, playVoiceJoinSound, playVoiceLeaveSound } from "./sounds.js";
import * as uploads from "./uploads.js";
import * as downloads from "./downloads.js";
import { openApprovalModal } from "./attach_approve.js";
import { isMuted, peerKey, groupKey } from "./mute.js";
import { avatarURL } from "./avatar.js";
import "./signal-banner.js";
import "./narrow.js";

export { playVoiceLeaveSound };

// showToast — уведомление вне окна. NotifyService по настройке notify_mode
// показывает своё окно (notify.html, правый нижний угол), системный toast
// или ничего. Дебаунс с «N новых уведомлений» убран: своё окно держит
// стек карточек само. opts: { kind, peerId, groupId } — куда вести по клику.
function showToast(text, title, opts) {
    var o = opts || {};
    var id = o.peerId || o.groupId || "";
    var name = title || "Vox";
    svc("ShowToast", {
        kind: o.kind || "message",
        title: name,
        body: text || "",
        peerId: o.peerId || "",
        groupId: o.groupId || "",
        avatar: name.substring(0, 2).toUpperCase(),
        color: id ? colorFromId(id) : "",
        avatarUrl: avatarURL(o.peerId || (o.groupId ? "g:" + o.groupId : "")),
        theme: document.documentElement.getAttribute("data-theme") || "light",
    }).catch(function (err) {
        console.warn("notify:", err);
    });
}

// toastPreview — текст карточки уведомления (макет Notifications): голосовое —
// «Голосовое сообщение · 0:42», остальное — начало текста.
function toastPreview(m) {
    var type = m.msgType || m.type;
    if (type === "voice") return t("msg.voice") + (m.duration ? " · " + formatMmSs(m.duration) : "");
    return m.content ? m.content.substring(0, 80) : "";
}

// Заменяет X (cancel) на play в доставленных voice сообщениях
function updateVoiceButtons() {
    var btns = document.querySelectorAll('.message-row.outbound .voice-play-btn');
    for (var i = 0; i < btns.length; i++) {
        var btn = btns[i];
        var row = btn.closest('.message-row');
        if (row && !row.querySelector('.bubble-status.pending')) {
            // Сообщение доставлено — заменяем X на play. cloneNode сбрасывает
            // старый addEventListener(CancelMessage): иначе клик по play
            // одновременно отменял/удалял уже доставленное сообщение.
            var fresh = btn.cloneNode(false);
            fresh.innerHTML = playIconSvg;
            fresh.title = "";
            if (btn.parentNode) btn.parentNode.replaceChild(fresh, btn);
            var msgId = fresh.dataset.msgId;
            fresh.onclick = (function (id, b) {
                return function () { togglePlayVoice(id, b); };
            })(msgId, fresh);
        }
    }
}

export function registerEvents() {
    Events.On("vox:message", async function (event) {
        var msg = event.data;
        if (msg.msgType === "attachment") {
            var attCount = (msg.attachments && msg.attachments.length) || 0;
            flog("INF", "events", "📥 ATTACH MSG: id=" + msg.id + " from=" + (msg.senderId || "").substring(0, 16) + " atts=" + attCount + " caption_len=" + (msg.content || "").length);
        }
        // outbound-события (например системная запись о нашем исходящем
        // звонке): senderId = мы, чат определяет peerId.
        var forOpenChat = msg.senderId === state.selectedPeerID ||
            (msg.outbound && msg.peerId && msg.peerId === state.selectedPeerID);
        if (forOpenChat) {
            // Не дёргаем скролл, если пользователь читает историю выше.
            var stick = isNearBottom();
            appendMessage(msg);
            if (stick) scrollToBottom(); else scrollToBottom(true);
            // Auto-mark as read since chat is open — await before refresh
            await svc("MarkMessagesRead", msg.senderId).catch(function (e) { flog("ERR", "events", "MarkMessagesRead: " + e); });
        }
        // Play notification sound only for real-time incoming messages (not old/offline).
        // Системные сообщения о звонках (msgType "call") звука и тоста не дают:
        // о звонке уже уведомил popup/рингтон.
        if (msg.msgType !== "call" && !msg.outbound && msg.timestamp && (Date.now() - msg.timestamp) < 10000) {
            // Не играть звук если чат с этим пиром открыт или заглушён (mute.js)
            if (msg.senderId !== state.selectedPeerID && !isMuted(peerKey(msg.senderId)) && !isBlocked(msg.senderId)) {
                playNotificationSound();
                var name = msg.nickname || (msg.senderId || "").substring(0, 8);
                showToast(toastPreview(msg), name, { kind: "message", peerId: msg.senderId });
            }
        }
        // Track last message for DM contact list preview (not group messages)
        if (msg.senderId && !msg.groupId) {
            state.lastMessages[msg.senderId] = msg;
        }
        await refreshPeers();
    });

    // Attachment chunked-transfer events (Phase 3 backend, Phase 4 UI).
    Events.On("vox:attach-progress", function (event) {
        var d = event.data || {};
        if (!d.msgId) return;
        uploads.updateProgress(d.msgId, d.peerId, d.sent || 0, d.total || 0);
    });
    Events.On("vox:attach-complete", function (event) {
        var d = event.data || {};
        if (!d.msgId) return;
        uploads.markPeerComplete(d.msgId, d.peerId, d.attachments || null);
    });
    Events.On("vox:attach-failed", function (event) {
        var d = event.data || {};
        if (!d.msgId) return;
        uploads.markPeerFailed(d.msgId, d.peerId, d.error || "");
    });
    Events.On("vox:attach-needs-approval", function (event) {
        var d = event.data || {};
        if (!d.msgId) return;
        openApprovalModal(d);
    });
    // ─── FilesService (CAS pipeline) события на receiver-стороне ───
    // vox:file-message — пришёл manifest, скачка ещё впереди (можно сразу
    // показать bubble-skeleton с известным размером).
    Events.On("vox:file-message", function (event) {
        var d = event.data || {};
        if (!d.msgId) return;
        // Для DM/group обновляем conversation-list. messages.js сам подтянет
        // на следующем GetMessages/GetGroupMessages при открытии чата.
        refreshPeers();
    });
    // vox:file-progress — chunk пришёл, обновляем downloads-state.
    Events.On("vox:file-progress", function (event) {
        var d = event.data || {};
        if (!d.fileSha) return;
        downloads.upsertProgress(d.fileSha, d.received || 0, d.total || 1, d.bytes || 0);
    });
    // vox:file-received — bitmap полный, файл готов. Markируем complete →
    // bubble видит overlay "complete" → force-reloadят img.src.
    Events.On("vox:file-received", function (event) {
        var d = event.data || {};
        if (!d.fileSha) return;
        downloads.markComplete(d.fileSha);
        refreshPeers();
    });
    Events.On("vox:file-cancelled", function (event) {
        var d = event.data || {};
        if (!d.fileSha) return;
        downloads.markCancelled(d.fileSha);
    });

    // Delivery confirmation — update clocks to single-check
    Events.On("vox:messages-delivered", function (event) {
        if (!event.data || !event.data.peerId) return;
        refreshPeers();
        if (event.data.peerId !== state.selectedPeerID) return;
        var pending = document.querySelectorAll('.message-row.outbound .bubble-status.pending');
        for (var i = 0; i < pending.length; i++) {
            pending[i].innerHTML = SINGLE_CHECK_SVG;
            pending[i].classList.remove("pending");
        }
        // Обновляем voice кнопки: X → play
        updateVoiceButtons();
    });

    // Read receipt from peer — update all outbound checkmarks to double-check
    Events.On("vox:messages-read", function (event) {
        if (!event.data || !event.data.peerId) return;
        if (event.data.peerId !== state.selectedPeerID) return;
        var statuses = document.querySelectorAll('.message-row.outbound .bubble-status');
        for (var i = 0; i < statuses.length; i++) {
            statuses[i].innerHTML = DOUBLE_CHECK_SVG;
            statuses[i].classList.remove("pending");
        }
    });

    // Заголовок DM-чата не трогаем напрямую: событие приходит и от сигналки
    // (peer_online), и от P2P, а список/Info считают online по P2P — заголовок
    // синхронизирует updateContacts после refreshPeers (syncChatPeerStatus).
    Events.On("vox:peer-connected", function () {
        refreshPeers();
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh")); // contacts.js: статус друзей
    });

    // Push-event битрейта голосовой сессии (раз в 2с пока voice активен).
    // Заменил собой setInterval(2000) в settings.js. Если вкладка settings
    // не открыта, applyBitrateSnapshot ничего не делает (нет элемента в DOM).
    Events.On("vox:voice-bitrate", function (event) {
        applyBitrateSnapshot(event.data);
    });

    Events.On("vox:signal-updated", function () {
        refreshPeers();
        svc("GetSignalServers").then(function (servers) {
            var online = servers && servers.some(function (s) { return s.online; });
            if (online) {
                dom.statusEl.textContent = t("chat.online");
                dom.statusEl.style.color = "var(--green-text)";
            } else {
                dom.statusEl.textContent = t("chat.offline");
                dom.statusEl.style.color = "var(--yellow-text)";
            }
        });
        var networkTab = document.getElementById("settings-tab-network");
        if (networkTab && networkTab.style.display !== "none") {
            loadSignalServers();
            loadTURNConfig();
        }
    });

    Events.On("vox:nickname-updated", function (event) {
        refreshPeers();
        // Обновить ник в активном звонке если там есть этот пир
        if (event.data && event.data.peerId && event.data.nickname && state.call) {
            var p = state.call.participants.find(function (pp) { return pp.peerId === event.data.peerId; });
            if (p && p.nickname !== event.data.nickname) {
                p.nickname = event.data.nickname;
                document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
            }
        }
    });

    Events.On("vox:peer-disconnected", function () {
        refreshPeers();
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
    });

    // Клик по карточке в окне уведомлений: NotifyService поднял главное
    // окно и прислал, какой чат открыть.
    Events.On("vox:open-chat", function (event) {
        var d = event.data || {};
        if (d.groupId) {
            var grp = null;
            for (var gi = 0; gi < state.groups.length; gi++) if (state.groups[gi].groupId === d.groupId) { grp = state.groups[gi]; break; }
            showGroupsList();
            selectGroup(d.groupId, grp);
        } else if (d.peerId) {
            var known = null;
            for (var pi = 0; pi < state.peers.length; pi++) if (state.peers[pi].id === d.peerId) { known = state.peers[pi]; break; }
            showMessagesList();
            selectPeer(d.peerId, known || { id: d.peerId, shortId: d.peerId.substring(0, 16) });
        }
    });

    Events.On("vox:friend-request", function (event) {
        logEvent("vox:friend-request", event.data);
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh")); // contacts.js: список и бейдж
        var data = event.data || {};
        // Sound only for real-time events
        if (data.timestamp && (Date.now() - data.timestamp) < 10000) {
            playNotificationSound();
            showToast(t("toast.friend_request").replace("{name}", data.nickname || data.peerId.substring(0, 16)), "Vox", { kind: "friend" });
        }
    });

    Events.On("vox:friend-accepted", function (event) {
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
        refreshPeers();
        var data = event.data || {};
        if ((data.nickname || data.peerId) && data.timestamp && (Date.now() - data.timestamp) < 10000) {
            showToast(t("toast.friend_accepted").replace("{name}", data.nickname || data.peerId.substring(0, 16)), "Vox", { kind: "friend", peerId: data.peerId });
        }
    });

    Events.On("vox:friend-rejected", function () {
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
    });

    Events.On("vox:friend-removed", function () {
        document.dispatchEvent(new CustomEvent("vox:ui-contacts-refresh"));
        refreshPeers();
    });

    Events.On("vox:message-deleted", function (event) {
        if (event.data && event.data.id) {
            var row = document.querySelector('.message-row[data-msg-id="' + cssEscape(event.data.id) + '"]');
            if (row) row.remove();
            forgetMessage(event.data.id);
        }
        refreshPeers();
    });

    Events.On("vox:group-message", async function (event) {
        if (event.data && event.data.groupId === state.selectedGroupID) {
            var stickG = isNearBottom();
            appendMessage(event.data.message);
            if (stickG) scrollToBottom(); else scrollToBottom(true);
            // Дождаться MarkGroupRead перед refreshPeers, чтобы бейдж обновился
            await svc("MarkGroupRead", event.data.groupId).catch(function (e) { flog("ERR", "events", "MarkGroupRead: " + e); });
        }
        if (event.data && event.data.message && !event.data.message.outbound && event.data.message.timestamp && (Date.now() - event.data.message.timestamp) < 10000) {
            // Заблокированный (фаза 7b) — без звука и карточки
            if (event.data.groupId !== state.selectedGroupID && !isMuted(groupKey(event.data.groupId)) && !isBlocked(event.data.message.senderId)) {
                playNotificationSound();
                var gMsg = event.data.message;
                var senderName = gMsg.senderName || (gMsg.senderId || "").substring(0, 8);
                var groupName = event.data.groupName || (event.data.groupId || "").substring(0, 8);
                showToast(senderName + ": " + toastPreview(gMsg), groupName, { kind: "group", groupId: event.data.groupId });
            }
        }
        await refreshPeers();
    });

    // Server relay: пришли сообщения через group_sync (догон при реконнекте).
    // Отдельные vox:group-message события уже отрисовали сообщения через
    // appendMessage — этот хук только обновляет список групп (бейдж
    // непрочитанных) и сбрасывает скролл, если пользователь в этой группе.
    Events.On("vox:group-messages-synced", async function (event) {
        if (event.data && event.data.groupId === state.selectedGroupID) {
            // Прокрутить вниз — все сообщения уже в DOM через appendMessage.
            dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
            await svc("MarkGroupRead", state.selectedGroupID).catch(function () {});
        }
        await refreshPeers();
    });

    // Server relay: режим доставки группы изменился.
    Events.On("vox:group-mode-changed", function () {
        refreshPeers();
    });

    // Group delivery confirmation — update clocks to single-check
    Events.On("vox:group-messages-delivered", function (event) {
        // Wails отдаёт { data: {...} } — groupIds лежат в event.data.
        var data = (event && event.data) || {};
        // Сверяем groupIds: подтверждение доставки в группе A не должно
        // превращать «часики» сообщений открытой группы B в галочки.
        if (data.groupIds && data.groupIds.length &&
            data.groupIds.indexOf(state.selectedGroupID) < 0) return;
        if (state.selectedGroupID) {
            var pending = document.querySelectorAll('.message-row.outbound .bubble-status.pending');
            for (var i = 0; i < pending.length; i++) {
                pending[i].innerHTML = SINGLE_CHECK_SVG;
                pending[i].classList.remove("pending");
            }
            // Обновляем voice кнопки: X → play
            updateVoiceButtons();
        }
    });

    Events.On("vox:group-invite", function () {
        refreshPeers();
    });

    Events.On("vox:group-member-left", function () {
        refreshPeers();
        refreshInfoPanel();
    });

    Events.On("vox:group-member-joined", function () {
        refreshPeers();
        refreshInfoPanel();
    });

    Events.On("vox:group-renamed", function () {
        refreshPeers();
        refreshInfoPanel();
    });

    Events.On("vox:group-member-added", function () {
        refreshPeers();
        refreshInfoPanel();
    });

    Events.On("vox:group-voice-toggle", function () {
        refreshPeers();
    });

    Events.On("vox:group-kicked", function (event) {
        refreshPeers();
        if (event.data && event.data.groupId === state.selectedGroupID && !event.data.peerId) {
            // Кикнули нас — очищаем UI
            state.selectedGroupID = null;
            dom.messagesDiv.innerHTML = "";
            dom.chatHeader.style.display = "none";
            dom.inputArea.style.display = "none";
            dom.infoPanel.classList.remove("open");
        } else {
            // Кикнули другого участника — обновляем Info-панель
            refreshInfoPanel();
        }
    });

    Events.On("vox:group-deleted", function (event) {
        refreshPeers();
        if (event.data && event.data.groupId === state.selectedGroupID) {
            state.selectedGroupID = null;
            dom.messagesDiv.innerHTML = "";
            dom.chatHeader.style.display = "none";
            dom.inputArea.style.display = "none";
            // Закрыть info-панель если показывала удалённую группу
            dom.infoPanel.classList.remove("open");
        }
    });

    // Сервер отклонил своё сообщение в группе, или отказ снят (code пуст).
    Events.On("vox:group-message-failed", function (event) {
        var d = event.data;
        if (d && d.id && d.groupId === state.selectedGroupID) {
            markSendFailed(d.id, d.code || "", !!d.delivered);
        }
        refreshPeers();
    });

    Events.On("vox:message-edited", function (event) {
        if (event.data && event.data.id && event.data.content) {
            markEdited(event.data.id, event.data.content);
        }
        refreshPeers();
    });

    // --- Voice channel events ---

    Events.On("vox:voice-join", function (event) {
        // Синхронизировать с активным звонком: если пир появился в голосе
        // (через gossip discovery, не обычный call_invite) — добавить в участников звонка.
        // onParticipantJoined проверяет дубликаты внутри.
        if (state.call && event.data && event.data.peerId && event.data.peerId !== state.myID) {
            onParticipantJoined({
                peerId: event.data.peerId,
                nickname: event.data.nickname || event.data.peerId.substring(0, 16),
            });
        }

        // Восстановить сохранённые громкости/mute для нового пира
        svc("GetActiveVoiceGroup").then(function (ag) {
            if (ag) applyGroupVolumes(ag);
        }).catch(function () {});

        // Звук join (не играть для ACK/RE-ANNOUNCE — пир уже был в канале).
        // Только вход в МОЙ канал: join в голос другой группы (в т.ч. повторы
        // старых клиентов раз в 3 с) раньше показывался как «зашёл в канал».
        svc("GetActiveVoiceGroup").then(function (ag) {
            if (ag && event.data && event.data.groupId === ag && !event.data.wasKnown) {
                playVoiceJoinSound();
                // В звонке участники входят в его voice-группу — это не
                // «зашёл в канал», карточка тут лишняя.
                if (event.data.peerId !== state.myID && !state.call) {
                    var name = event.data.nickname || (event.data.peerId || "").substring(0, 16);
                    showToast(name + " " + t("toast.voice_joined"), t("toast.voice"), { kind: "voice", groupId: ag });
                }
            }
        }).catch(function () {});
        updateVoiceHeaderBtn();
        renderUserBar();
    });

    Events.On("vox:voice-leave", function (event) {
        if (event.data && event.data.peerId) {
            voiceState.removePeer(event.data.peerId);
            // UI обновится автоматически через observers
            //
            // ВАЖНО: НЕ вызываем onParticipantLeft здесь. voice-leave приходит
            // из ЛЮБОГО voice-канала, в т.ч. группового. Если пир выходит из
            // группового voice (например чтобы освободить микрофон и принять
            // DM-звонок), то этот хак ошибочно убирал его из активного звонка
            // ещё до того как call-accepted дойдёт. Реальный выход из звонка
            // приходит через vox:call-participant-left (call_end signal).
        }
        // Сцена/панель участников (stage.js) обновятся через peer-removed observer.

        svc("GetActiveVoiceGroup").then(function (ag) {
            if (ag && event.data && event.data.groupId === ag && event.data.peerId !== state.myID) {
                playVoiceLeaveSound();
                var name = event.data.nickname || (event.data.peerId || "").substring(0, 16);
                if (!state.call) showToast(name + " " + t("toast.voice_left"), t("toast.voice"), { kind: "voice", groupId: ag });
            }
        }).catch(function () {});
        updateVoiceHeaderBtn();
        renderUserBar();
    });

    // vox:voice-activity (кто говорит) слушает stage.js — кольца на сцене и в панели.

    Events.On("vox:voice-status", function (event) {
        if (!event.data) return;
        try {
            var status = JSON.parse(event.data.status);
            voiceState.setPeerStatus(event.data.peerId, status);
            // UI обновится автоматически через observers (self-changed / peer-changed)
        } catch (e) {}
        updateVoiceHeaderBtn();
    });

    // --- Call events ---

    Events.On("vox:call-incoming", function (event) {
        logEvent("vox:call-incoming", event.data);
        // Карточка звонка живёт в окне уведомлений (или в popup-окне при
        // системном режиме); рингтон играем здесь — у главного окна уже есть
        // разрешение на звук, у окна без фокуса AudioContext может молчать.
        playRingtone();
    });
    Events.On("vox:toast-call-end", function () { stopRingtone(); });

    Events.On("vox:call-accepted", function (event) {
        logEvent("vox:call-accepted", event.data);
        stopRingtone();
        onCallAccepted(event.data);
    });

    Events.On("vox:call-rejected", function (event) {
        logEvent("vox:call-rejected", event.data);
        stopRingtone();
        onCallRejected(event.data);
    });

    Events.On("vox:call-ended", function (event) {
        logEvent("vox:call-ended", event.data);
        stopRingtone();
        endCall();
    });

    Events.On("vox:call-participant-joined", function (event) {
        logEvent("vox:call-participant-joined", event.data);
        onParticipantJoined(event.data);
    });

    Events.On("vox:call-participant-left", function (event) {
        logEvent("vox:call-participant-left", event.data);
        onParticipantLeft(event.data);
    });

    Events.On("vox:call-participant-reconnecting", function (event) {
        logEvent("vox:call-participant-reconnecting", event.data);
        onParticipantReconnecting(event.data);
    });

    Events.On("vox:call-participant-reconnected", function (event) {
        logEvent("vox:call-participant-reconnected", event.data);
        onParticipantReconnected(event.data);
    });
}

// --- Voice header button ---

function updateVoiceHeaderBtn() {
    var btn = document.getElementById("voice-join-btn");
    if (!btn) return;
    svc("GetActiveVoiceGroup").then(function (ag) {
        var inChannel = ag === state.selectedGroupID;
        btn.classList.toggle("active", inChannel);
        // Обновить count
        if (state.selectedGroupID) {
            svc("GetVoiceParticipants", state.selectedGroupID).then(function (p) {
                var count = document.getElementById("voice-count");
                if (count) count.textContent = (p && p.length > 0) ? p.length : "";
            }).catch(function () {});
        }
    }).catch(function () {});
}

export { updateVoiceHeaderBtn, playVoiceJoinSound };

