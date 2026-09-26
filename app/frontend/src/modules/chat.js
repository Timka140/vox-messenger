"use strict";

// chat.js — Message input, send, and edit logic.
// List rendering moved to contacts-list.js, message bubbles to messages.js,
// voice recording to recording.js.

import { svc, state, dom, autoGrowTextarea, formatBytes, cssEscape } from "./core.js";
import { appendMessage, setStartEditMessage, setStartReply, setRetrySend, markEdited, getCurrentMsgs } from "./messages.js";
import { updateContacts, refreshPeers, setCancelEdit } from "./contacts-list.js";
import { initRecording } from "./recording.js";
import { showContextMenu } from "./contextmenu.js";
import { t } from "./i18n.js";
// attach.js теперь self-contained: sendPathAttachments экспортируется ОТСЮДА
// и зовётся из composer-attach.js. Compose-strip-flow удалён.
import { showError, showInfo } from "./toast.js";
import * as uploads from "./uploads.js";

// Правый клик по шапке чата — меню «…» (chat-menu.js).

// --- Re-export for backward compatibility (used by events.js, contacts.js, etc.) ---
export { updateContacts, refreshPeers } from "./contacts-list.js";
export { showGroupsList, showMessagesList, showVoiceChannels, selectPeer, selectGroup } from "./contacts-list.js";
export { renderMessages, appendMessage } from "./messages.js";

// --- Message editing state ---

var editState = null; // {msgId, peerId, groupId, wrapper, originalContent, contentEl}

function startEditMessage(wrapper, msg) {
    if (editState) cancelEdit();
    cancelReply();

    var contentEl = wrapper.querySelector(".bubble-content, .attach-caption");
    if (!contentEl) return;

    editState = {
        msgId: msg.id,
        peerId: state.selectedPeerID,
        groupId: state.selectedGroupID,
        wrapper: wrapper,
        originalContent: msg.content,
        contentEl: contentEl,
    };

    wrapper.classList.add("editing");
    // Полоса одна на всё: вложения из строки ввода уходят (макет ComposerStates)
    document.dispatchEvent(new CustomEvent("vox:ui-composer-clear-attach"));
    showEditStrip(msg.content);
    dom.messageInput.value = msg.content;
    dom.messageInput.focus();
    dom.messageInput.setSelectionRange(dom.messageInput.value.length, dom.messageInput.value.length);
    autoGrowTextarea();
}

function finishEdit() {
    if (!editState) return;
    var es = editState;
    editState = null;
    hideEditStrip();

    es.wrapper.classList.remove("editing");

    var newContent = dom.messageInput.value.trim();
    dom.messageInput.value = "";
    dom.messageInput.style.height = "auto";

    if (!newContent || newContent === es.originalContent) return;

    es.contentEl.textContent = newContent;
    markEdited(es.msgId, newContent);

    svc("EditMessage", es.msgId, es.peerId || "", es.groupId || "", newContent).catch(function (err) {
        console.error("edit message:", err);
        es.contentEl.textContent = es.originalContent;
    });
}

function cancelEdit() {
    if (!editState) return;
    var es = editState;
    editState = null;
    hideEditStrip();
    es.wrapper.classList.remove("editing");
    dom.messageInput.value = "";
    dom.messageInput.style.height = "auto";
}

// Полоса «Редактирование» над полем: карандаш, исходный текст, ×. Справа
// вместо микрофона/отправки — одна кнопка с галочкой (#edit-confirm-btn).
function showEditStrip(text) {
    var strip = document.getElementById("composer-strip");
    if (!strip) return;
    strip.dataset.mode = "edit";
    strip.innerHTML = '<i data-lucide="pencil" class="icon-16 composer-strip-ico"></i>' +
        '<div class="composer-quote"><b></b><span></span></div>' +
        '<button type="button" class="composer-strip-close"><i data-lucide="x" class="icon-16"></i></button>';
    strip.querySelector(".composer-quote b").textContent = t("composer.editing");
    strip.querySelector(".composer-quote span").textContent = text || "";
    var close = strip.querySelector(".composer-strip-close");
    close.title = t("dialog.cancel");
    close.addEventListener("click", cancelEdit);
    strip.hidden = false;
    dom.inputArea.classList.add("is-editing");
    if (window.lucide) lucide.createIcons({ nodes: [strip] });
}

function hideEditStrip() {
    var strip = document.getElementById("composer-strip");
    if (strip && strip.dataset.mode === "edit") {
        strip.hidden = true;
        strip.innerHTML = "";
        strip.dataset.mode = "";
    }
    dom.inputArea.classList.remove("is-editing");
}

// --- Ответ с цитатой (макет ComposerStates: полоса «Ответ») ---
// ref — снимок исходного сообщения {id, senderId, senderName, snippet, kind},
// собирает messages.js (buildReplyRef). Полоса одна на всё: ответ снимает
// редактирование и вложения, а они — ответ.

var replyState = null; // {ref, peerId, groupId}

function startReply(ref) {
    if (!ref || !ref.id) return;
    if (editState) cancelEdit();
    document.dispatchEvent(new CustomEvent("vox:ui-composer-clear-attach"));
    replyState = { ref: ref, peerId: state.selectedPeerID, groupId: state.selectedGroupID };
    showReplyStrip(ref);
    try { dom.messageInput.focus(); } catch (e) { /* поле скрыто */ }
}

function cancelReply() {
    if (!replyState) return;
    replyState = null;
    var strip = document.getElementById("composer-strip");
    if (strip && strip.dataset.mode === "reply") {
        strip.hidden = true;
        strip.innerHTML = "";
        strip.dataset.mode = "";
    }
}

function showReplyStrip(ref) {
    var strip = document.getElementById("composer-strip");
    if (!strip) return;
    strip.dataset.mode = "reply";
    strip.innerHTML = '<i data-lucide="reply" class="icon-16 composer-strip-ico"></i>' +
        '<div class="composer-quote"><b></b><span></span></div>' +
        '<button type="button" class="composer-strip-close"><i data-lucide="x" class="icon-16"></i></button>';
    strip.querySelector(".composer-quote b").textContent =
        ref.senderId && ref.senderId === state.myID ? t("composer.you") : (ref.senderName || "");
    strip.querySelector(".composer-quote span").textContent = ref.snippet || "";
    var close = strip.querySelector(".composer-strip-close");
    close.title = t("dialog.cancel");
    close.addEventListener("click", cancelReply);
    strip.hidden = false;
    if (window.lucide) lucide.createIcons({ nodes: [strip] });
}

// Register callbacks (avoids circular dep)
setStartEditMessage(startEditMessage);
setStartReply(startReply);
setRetrySend(function (text, replyTo) { sendMessage(text, replyTo); });
setCancelEdit(cancelEdit);

// --- Send message ---

// sendMessage(textOverride, replyOverride) — без аргументов берёт текст из поля
// ввода и цитату из полосы «Ответ»; с аргументами (повтор неотправленного из
// ленты) поле и полосу не трогает.
export async function sendMessage(textOverride, replyOverride) {
    var fromInput = typeof textOverride !== "string";
    var text = (fromInput ? dom.messageInput.value : textOverride).trim();
    if (!text || (!state.selectedPeerID && !state.selectedGroupID)) return;

    var replyRef = null;
    if (fromInput) {
        if (replyState && replyState.peerId === state.selectedPeerID && replyState.groupId === state.selectedGroupID) {
            replyRef = replyState.ref;
        }
    } else if (replyOverride && replyOverride.id) {
        replyRef = replyOverride;
    }

    // Нельзя отправлять в покинутую группу
    if (state.selectedGroupID) {
        var grp = state.groups.find(function (g) { return g.groupId === state.selectedGroupID; });
        if (grp && grp.left) return;
    }

    var target = state.selectedGroupID
        ? "group:" + state.selectedGroupID.substring(0, 8)
        : "peer:" + state.selectedPeerID.substring(0, 16);
    console.log("[SEND] → " + target + " len=" + text.length);

    // Запоминаем чат ДО await: при медленной доставке пользователь может
    // переключиться в другой чат, и appendMessage отрисовал бы «своё»
    // сообщение среди сообщений уже открытого чужого чата.
    var sentPeerID = state.selectedPeerID;
    var sentGroupID = state.selectedGroupID;

    if (fromInput) {
        dom.messageInput.value = "";
        dom.messageInput.style.height = "auto";
        cancelReply();
    }
    var opts = replyRef ? { replyTo: replyRef } : null;
    try {
        var result;
        if (sentGroupID) {
            result = opts
                ? await svc("SendGroupMessageWithOptions", sentGroupID, text, opts)
                : await svc("SendGroupMessage", sentGroupID, text);
        } else {
            result = opts
                ? await svc("SendMessageWithOptions", sentPeerID, text, opts)
                : await svc("SendMessage", sentPeerID, text);
        }
        var statusIcon = result.delivered ? "✓" : "⏳";
        console.log("[SEND] ← " + target + " id=" + result.id + " status=" + statusIcon);
        var stillOnChat = sentGroupID
            ? state.selectedGroupID === sentGroupID
            : state.selectedPeerID === sentPeerID;
        if (stillOnChat) {
            appendMessage({
                id: result.id,
                senderId: state.myID,
                content: text,
                timestamp: Date.now(),
                outbound: true,
                delivered: result.delivered,
                replyTo: replyRef || undefined,
            });
            dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
        }
        updateContacts(state.peers);
    } catch (err) {
        console.error("[SEND] ✗ " + target + " error:", err);
        var stillOnChat = sentGroupID
            ? state.selectedGroupID === sentGroupID
            : state.selectedPeerID === sentPeerID;
        if (stillOnChat) {
            // Причина (макет MessagesText): собеседник не в сети — «нет связи с {ник}».
            var failReason = "";
            // ErrRelayTooLarge (groups_relay_retry.go): не влезает в лимит группы.
            if (sentGroupID && String(err).indexOf("слишком длинное для группы") >= 0) {
                failReason = t("msg.too_long");
            }
            if (!sentGroupID) {
                var fp = (state.peers || []).find(function (x) { return x.id === sentPeerID; });
                if (fp && !fp.online) failReason = t("msg.no_connection").replace("{name}", fp.nickname || fp.shortId || "");
            }
            // Не ушло: настоящий пузырь с текстом и ✕, клик — повтор (messages.js).
            appendMessage({
                id: "failed-" + Date.now(),
                senderId: state.myID,
                content: text,
                timestamp: Date.now(),
                outbound: true,
                failed: true,
                failReason: failReason || undefined,
                replyTo: replyRef || undefined,
            });
            dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
        }
    }
}

// sendPathAttachments — единственная точка отправки вложений из модалки.
// Зовётся из composer-attach.js по клику «Отправить» с уже-собранными items
// и подписью. Сразу шлёт SendAttachmentsByPath / SendGroupAttachmentsByPath
// с текущим выделенным peer/group.
//
// items: [{ kind:"photo"|"file", name, size, mime, path, compress, previewUrl|null }]
// caption: string (может быть пустым)
export async function sendPathAttachments(items, caption) {
    if (!items || items.length === 0) return;

    var peerId = state.selectedPeerID;
    var groupId = state.selectedGroupID;
    if (!peerId && !groupId) {
        showError(t("attach.send_failed"));
        return;
    }
    if (groupId) {
        var grp = state.groups.find(function (g) { return g.groupId === groupId; });
        if (grp && grp.left) {
            showError(t("attach.send_failed"));
            return;
        }
    }
    var target = groupId
        ? "group:" + groupId.substring(0, 8)
        : "peer:" + (peerId || "").substring(0, 16);

    var totalBytes = 0;
    for (var i = 0; i < items.length; i++) totalBytes += items[i].size;
    console.log("[SEND-ATT] path → " + target + " count=" + items.length +
        " bytes=" + totalBytes + " caption_len=" + (caption || "").length);

    if (totalBytes > 50 * 1024 * 1024) {
        showInfo(
            t("attach.large_warning").replace("{size}", formatBytes(totalBytes)),
            { title: t("attach.title"), duration: 5000 }
        );
    }

    // backend ожидает {path, kind, compress, name} — лишние поля игнорируются.
    var sendItems = items.map(function (it) {
        return { path: it.path, kind: it.kind, compress: !!it.compress, name: it.name };
    });

    var previews = items.map(function (it) {
        return {
            name: it.name,
            kind: it.kind,
            mime: it.mime,
            size: it.size,
            // localUrl — превью для bubble во время uploading. Для path-photo
            // это /vox-local/<token>; после ATTACH READY FOR UI swap'нется на
            // /vox-attach/<sha256> через swapBubbleToRealAttachments.
            localUrl: it.kind === "photo" ? (it.previewUrl || null) : null,
        };
    });

    // Используем новый CAS pipeline (FilesService). Старый MessagingService.SendAttachmentsByPath
    // оставлен для совместимости, но новые сообщения идут через CAS — pull-модель, resume,
    // dedup на уровне chunks. См. internal/services/files/.
    var method = groupId ? "SendGroupFilesByPath" : "SendFilesByPath";
    var result;
    try {
        if (groupId) {
            result = await svc(method, groupId, sendItems, caption);
        } else {
            result = await svc(method, peerId, sendItems, caption);
        }
    } catch (err) {
        console.error("[SEND-ATT] ✗ " + target + " error:", err);
        showError(String(err && err.message || err), { title: t("attach.send_failed") });
        return;
    }
    if (!result || !result.id) return;
    // Пользователь мог уйти в другой чат за время await — не рисуем чужой bubble.
    var stillOnChat = groupId ? state.selectedGroupID === groupId : state.selectedPeerID === peerId;
    if (!stillOnChat) return;

    // CAS pipeline: реальной скачки на sender'e нет (receiver делает pull сам).
    // Поэтому НЕ регистрируем uploads — bubble сразу финальный без 0B/X overlay.
    // Manifest доставлен при возврате SendResult, дальше — забота receiver'а.
    appendMessage({
        id: result.id,
        senderId: state.myID,
        content: caption,
        msgType: "attachment",
        attachments: previews,
        timestamp: Date.now(),
        outbound: true,
        delivered: true, // manifest доставлен — DM считаем delivered
    });
    dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
}

// collectGroupPeerIds возвращает список peer IDs группы (исключая себя).
function collectGroupPeerIds(groupId) {
    var grp = state.groups.find(function (g) { return g.groupId === groupId; });
    if (!grp || !grp.members) return [];
    var out = [];
    for (var i = 0; i < grp.members.length; i++) {
        var m = grp.members[i];
        if (m.peerId && m.peerId !== state.myID) out.push(m.peerId);
    }
    return out;
}

// --- Input event listeners ---

dom.messageInput.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && editState) {
        e.preventDefault();
        cancelEdit();
        return;
    }
    if (e.key === "Escape" && replyState) {
        e.preventDefault();
        cancelReply();
        return;
    }
    if (e.key === "ArrowUp" && !editState && !dom.messageInput.value && !e.shiftKey && !e.altKey && !e.ctrlKey &&
        !dom.inputArea.classList.contains("has-attach")) {
        var list = getCurrentMsgs();
        for (var li = list.length - 1; li >= 0; li--) {
            var lm = list[li];
            if (lm.outbound && !lm.failed && (!lm.msgType || lm.msgType === "text")) {
                var lrow = dom.messagesDiv.querySelector('.message-row[data-msg-id="' + cssEscape(String(lm.id)) + '"]');
                if (lrow) { e.preventDefault(); startEditMessage(lrow, lm); }
                break;
            }
        }
        return;
    }
    if (e.key === "Enter" && !e.shiftKey) {
        e.preventDefault();
        if (editState) {
            finishEdit();
        } else {
            sendMessage();
        }
    }
});

dom.messageInput.addEventListener("input", autoGrowTextarea);

document.getElementById("edit-confirm-btn").addEventListener("click", function () { finishEdit(); });
// composer-attach.js: добавили вложения во время редактирования — отмена
document.addEventListener("vox:ui-edit-cancel", function () { cancelEdit(); cancelReply(); });
document.addEventListener("vox:ui-chat-changed", function () { cancelReply(); });

document.getElementById("send-btn").addEventListener("click", function () {
    if (editState) {
        finishEdit();
    } else {
        sendMessage();
    }
});

// --- Send via Mesh button (visible only when friend has mesh node mapped) ---

document.getElementById("send-mesh-btn").addEventListener("click", async function () {
    var text = dom.messageInput.value.trim();
    if (!text || !state.selectedPeerID) return;
    var btn = this;
    btn.disabled = true;
    dom.messageInput.value = "";
    dom.messageInput.style.height = "auto";
    try {
        var meshPeer = state.selectedPeerID;
        var result = await svc("SendMessageViaMesh", meshPeer, text);
        if (state.selectedPeerID !== meshPeer) return; // ушли в другой чат
        appendMessage({
            id: result.id,
            senderId: state.myID,
            content: text,
            timestamp: Date.now(),
            outbound: true,
            delivered: false,
        });
        dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
    } catch (err) {
        console.error("[SEND-MESH] failed:", err);
        if (state.selectedPeerID !== meshPeer) return;
        appendMessage({
            // Уникальный id: с константным "err" dedup в appendMessage
            // подавлял все последующие ошибки.
            id: "err-mesh-" + Date.now(),
            senderId: "",
            content: "Mesh send failed: " + err,
            timestamp: Date.now(),
            outbound: true,
        });
    } finally {
        btn.disabled = false;
    }
});

// Show/hide "Send via Mesh" button when a peer is selected. Driven by the
// peer-info panel's mesh mapping query — when it finds a mapped nodeNum,
// it dispatches vox:peer-mesh-available; otherwise vox:peer-mesh-unavailable.
document.addEventListener("vox:peer-mesh-available", function () {
    var btn = document.getElementById("send-mesh-btn");
    if (btn) btn.style.display = "";
});
document.addEventListener("vox:peer-mesh-unavailable", function () {
    var btn = document.getElementById("send-mesh-btn");
    if (btn) btn.style.display = "none";
});

// --- Init recording module ---
initRecording();
