"use strict";

// messages.js — Message rendering (text + voice bubbles) and rename dialog.

import { svc, state, dom, colorFromId, formatMmSs, formatDateLabel, formatBytes, CLOCK_SVG, SINGLE_CHECK_SVG, DOUBLE_CHECK_SVG, cssEscape } from "./core.js";
import { playIconSvg, downloadIconSvg, togglePlayVoice, seekVoice, needsDownload, estimateSize, cycleVoiceRate, voiceRateLabel } from "./voice.js";
import { showContextMenu } from "./contextmenu.js";
import { buildDialogOverlay, emptyState } from "./ui.js";
import { t, getLocale } from "./i18n.js";
import { refreshPeers } from "./contacts-list.js";
import * as uploads from "./uploads.js";
import * as downloads from "./downloads.js";
import { parseCallContent } from "./callfmt.js";
import { openLightbox } from "./lightbox.js";
import { showSuccess, showError, showInfo } from "./toast.js";
import { applyAvatar } from "./avatar.js";
import { isBlocked } from "./blocked.js";

// Сообщения заблокированных в группе, которые пользователь раскрыл кликом по
// «скрыто» (фаза 7b) — до перезапуска не прячем их снова при перерисовке.
var revealedBlocked = new Set();
var hiddenBlockedMsgs = new Map(); // id → msg для строк «скрыто» (раскрытие по клику)

// Раскрытие «скрыто» — один делегированный слушатель на ленте: строку после
// отрисовки оформляет и может заменить decorateRow, так что слушатели на
// самой строке или её содержимом теряются.
dom.messagesDiv.addEventListener("click", function (e) {
    var hid = e.target.closest && e.target.closest(".blocked-hidden-msg");
    if (!hid) return;
    var row = hid.closest(".message-row");
    var msg = row && hiddenBlockedMsgs.get(row.dataset.msgId);
    if (!msg) return;
    revealedBlocked.add(String(msg.id));
    hiddenBlockedMsgs.delete(String(msg.id));
    // appendRow не строит строку, если в ленте уже есть такой id — снимаем
    // его со строки «скрыто» на время отрисовки настоящей.
    row.removeAttribute("data-msg-id");
    appendRow(msg);
    var real = dom.messagesDiv.lastElementChild;
    if (real && real !== row && real.dataset.msgId === String(msg.id)) {
        decorateRow(real, msg);
        row.replaceWith(real);
        if (window.lucide) lucide.createIcons({ nodes: [real] });
    } else {
        row.dataset.msgId = String(msg.id);
    }
});

// --- Render full message history ---

// Реестр document-level слушателей vox:download-updated, навешенных на bubbles.
// renderMessages очищает чат через innerHTML="", отчего сами bubbles исчезают,
// но слушатели на document оставались навсегда и копились (замыкая detached
// DOM) — за день переписки с фото их набирались сотни, каждый vox:file-progress
// гонял querySelectorAll по мёртвым деревьям. Снимаем их перед ре-рендером.
var _downloadListeners = [];
function clearDownloadListeners() {
    for (var i = 0; i < _downloadListeners.length; i++) {
        document.removeEventListener("vox:download-updated", _downloadListeners[i]);
    }
    _downloadListeners = [];
}

// renderMessages(msgs, opts) — полная отрисовка ленты открытого чата.
//   msgs — сообщения от старых к новым (null — перерисовать currentMsgs);
//   opts.unread — сколько последних входящих ещё не прочитано: перед первым
//     из них встаёт разделитель «Новые сообщения», и лента открывается на нём;
//   opts.preserveScroll — сохранить расстояние от низа (подгрузка старых,
//     удаление) вместо прокрутки вниз.
// Серии одного автора, аватары и имена в группах, время inline — decorateRow
// и linkSeries ниже (фаза 3a, макет MessagesText).
// buildChatEmptyState — пустой чат (макет EmptyStates «Новый пустой чат» и
// «Группа без участников»). Звонок и диалог участника грузятся лениво —
// статический импорт calls.js/info-panel.js замкнул бы цикл модулей.
function buildChatEmptyState() {
    if (state.selectedGroupID) {
        var gid = state.selectedGroupID;
        var g = (state.groups || []).find(function (x) { return x.groupId === gid; });
        if (g && !g.left && g.members <= 1 && g.creatorId === state.myID) {
            return emptyState("users", t("empty.group_alone_title"), t("empty.group_alone_text"), {
                label: t("info.add_member"), icon: "user-plus",
                onClick: function () { import("./info-panel.js").then(function (m) { m.showAddMemberDialog(gid); }); },
            });
        }
        return emptyState("send", t("empty.chat_title"), t("empty.first_text"));
    }
    var p = state.selectedPeerInfo || {};
    var name = p.nickname || p.shortId || "";
    var pid = state.selectedPeerID;
    if (!name || !pid || String(pid).indexOf("__mesh:") === 0) {
        return emptyState("send", t("empty.chat_title"), t("empty.first_text"));
    }
    if (p.online) {
        return emptyState("send", t("empty.chat_title"), t("empty.chat_online").replace("{name}", name), {
            label: t("info.act_call"), icon: "phone",
            onClick: function () { import("./calls.js").then(function (m) { m.startCall(pid); }); },
        });
    }
    return emptyState("send", t("empty.chat_title"), t("empty.chat_offline").replace("{name}", name));
}

export function renderMessages(msgs, opts) {
    opts = opts || {};
    clearDownloadListeners();
    var key = chatKeyNow();
    if (key !== renderedKey) exitSelectMode();
    if (msgs) {
        currentMsgs = msgs.slice();
        hasMore = currentMsgs.length >= PAGE_SIZE;
        unreadAnchorId = null;
    }
    renderedKey = key;
    var el = dom.messagesDiv;
    var fromBottom = el.scrollHeight - el.scrollTop;
    el.innerHTML = "";
    if (currentMsgs.length === 0) {
        el.appendChild(buildChatEmptyState());
        return;
    }

    // Разделитель «Новые сообщения» — перед первым из последних unread входящих
    if (msgs && opts.unread > 0) {
        var cnt = 0, firstIdx = -1;
        for (var u = currentMsgs.length - 1; u >= 0; u--) {
            var um = currentMsgs[u];
            if (um.outbound || um.msgType === "call" || um.msgType === "missed_call") continue;
            cnt++;
            firstIdx = u;
            if (cnt >= opts.unread) break;
        }
        if (firstIdx >= 0) unreadAnchorId = String(currentMsgs[firstIdx].id);
    }

    var lastDate = "";
    var newDivider = null;
    for (var i = 0; i < currentMsgs.length; i++) {
        var msg = currentMsgs[i];
        var msgDate = new Date(msg.timestamp).toDateString();
        if (msgDate !== lastDate) {
            lastDate = msgDate;
            el.appendChild(buildDateDivider(msg.timestamp));
        }
        if (unreadAnchorId && String(msg.id) === unreadAnchorId) {
            newDivider = buildNewDivider();
            el.appendChild(newDivider);
        }
        appendRowDecorated(msg);

        if (!state.selectedGroupID) {
            var peerId = msg.outbound ? state.selectedPeerID : msg.senderId;
            if (peerId) {
                if (!state.lastMessages[peerId] || msg.timestamp >= state.lastMessages[peerId].timestamp) {
                    state.lastMessages[peerId] = msg;
                }
            }
        }
    }
    if (opts.preserveScroll) {
        el.scrollTop = el.scrollHeight - fromBottom;
    } else if (newDivider) {
        el.scrollTop = Math.max(0, newDivider.offsetTop - 80);
    } else {
        settleScrollToBottom();
    }
    document.dispatchEvent(new CustomEvent("vox:ui-messages-rendered"));
}

// settleScrollToBottom — держать ленту у низа, пока догружаются картинки и
// иконки после рендера. Один scrollTop=scrollHeight сразу после innerHTML не
// работает: превью фото/файлов меняют высоту позже, и чат открывался «не в
// самом низу» (Тим, 2026-09-08). Пин снимается, как только пользователь сам
// прокрутил вверх (isNearBottom=false) или через 2.5 с.
//
// Проверки isNearBottom недостаточно: плавный скролл WebView2 двигает ленту
// по несколько px за кадр, и каждый шаг колёсика попадал в 80-px зону и
// «примагничивался» обратно к низу (Тим, 2026-09-14). Поэтому пин снимается
// сразу при любом действии пользователя: колесо вверх, зажатие скроллбара,
// тач, клавиши навигации — см. initSettleCancelOnUserScroll.
var _settleUntil = 0;
function cancelSettle() { _settleUntil = 0; }
(function initSettleCancelOnUserScroll() {
    var el = dom.messagesDiv;
    if (!el) return;
    el.addEventListener("wheel", function (e) {
        if (e.deltaY < 0) cancelSettle();
    }, { passive: true });
    el.addEventListener("mousedown", cancelSettle, { passive: true });
    el.addEventListener("touchstart", cancelSettle, { passive: true });
    el.addEventListener("keydown", function (e) {
        if (e.key === "ArrowUp" || e.key === "PageUp" || e.key === "Home") cancelSettle();
    });
})();
function settleScrollToBottom() {
    _settleUntil = Date.now() + 2500;
    var el = dom.messagesDiv;
    el.scrollTop = el.scrollHeight;
    var tick = function () {
        if (Date.now() > _settleUntil) return;
        if (isNearBottom()) el.scrollTop = el.scrollHeight;
        requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
}
(function initSettleOnImageLoad() {
    if (!dom.messagesDiv) return;
    // load у <img> не всплывает — ловим на capture-фазе контейнера.
    dom.messagesDiv.addEventListener("load", function () {
        if (Date.now() <= _settleUntil && isNearBottom()) {
            dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
        }
    }, true);
})();

// --- Append single message bubble ---

// startEditMessage callback — set by chat.js to avoid circular dep
var _startEditMessage = null;
export function setStartEditMessage(fn) { _startEditMessage = fn; }
var _startReply = null;                // chat.js: полоса «Ответ»
export function setStartReply(fn) { _startReply = fn; }

// senderNameOf — имя автора для цитаты и «Переслано от».
function senderNameOf(m) {
    if (m.outbound) return state.myNickname || state.myShortID || "";
    if (m.senderName) return m.senderName;
    if (m.nickname) return m.nickname;
    var pid = String(m.senderId || state.selectedPeerID || "");
    var peers = state.peers || [];
    for (var i = 0; i < peers.length; i++) {
        if (peers[i].id === pid) return peers[i].nickname || peers[i].shortId || pid.substring(0, 8);
    }
    return pid.substring(0, 8);
}

// buildReplyRef — снимок сообщения для ответа (Go: services.ReplyRef).
export function buildReplyRef(m) {
    var kind = "text";
    var snippet = m.content || "";
    if (m.msgType === "voice") {
        kind = "voice";
        snippet = t("msg.voice");
    } else if (m.msgType === "attachment") {
        var atts = m.attachments || [];
        var allPhotos = atts.length > 0 && atts.every(function (a) { return a.kind === "photo"; });
        kind = allPhotos ? "photo" : "file";
        if (!snippet) snippet = allPhotos ? t("msg.photo") : t("msg.file").replace("{name}", (atts[0] && atts[0].name) || "");
    }
    snippet = snippet.replace(/\s+/g, " ").trim();
    if (snippet.length > 140) snippet = snippet.substring(0, 140);
    return {
        id: String(m.id),
        senderId: m.outbound ? String(state.myID || "") : String(m.senderId || state.selectedPeerID || ""),
        senderName: senderNameOf(m),
        snippet: snippet,
        kind: kind,
    };
}

// buildQuote — цитата в пузыре ответа; клик — прыжок к оригиналу.
function buildQuote(ref) {
    var q = document.createElement("div");
    q.className = "bubble-quote";
    var b = document.createElement("b");
    b.textContent = ref.senderId && ref.senderId === state.myID
        ? t("composer.you")
        : (ref.senderName || String(ref.senderId || "").substring(0, 8));
    var s = document.createElement("span");
    s.textContent = ref.snippet || "";
    q.appendChild(b);
    q.appendChild(s);
    q.addEventListener("click", function (e) {
        if (selectMode || e.ctrlKey || e.metaKey) return;
        e.stopPropagation();
        jumpToMessage(ref.id);
    });
    return q;
}

// buildForwardLine — «Переслано от {name}» первой строкой пузыря.
function buildForwardLine(name) {
    var el = document.createElement("div");
    el.className = "bubble-forward";
    el.innerHTML = FORWARD_SVG;
    var s = document.createElement("span");
    s.textContent = t("msg.forwarded_from").replace("{name}", name);
    el.appendChild(s);
    return el;
}

// jumpToMessage — к цитируемому сообщению: если не загружено, Go считает его
// смещение, догружаем историю с запасом (время в секундах, соседи могут
// совпасть) и прокручиваем; рамка гаснет.
// jumpToMessage — прокрутить к сообщению, догрузив историю (цитата,
// результат поиска в медиа).
export async function jumpToMessage(id) {
    var key = chatKeyNow();
    var row = rowById(id);
    if (!row) {
        var pid = state.selectedPeerID, gid = state.selectedGroupID;
        var off = -1;
        try { off = await svc("GetMessageOffset", gid ? "" : pid, gid || "", String(id)); } catch (err) { off = -1; }
        if (chatKeyNow() !== key) return;
        if (off < 0) { showInfo(t("msg.reply_deleted")); return; }
        await loadOlderUntil(off + 5);
        if (chatKeyNow() !== key) return;
        row = rowById(id);
        if (!row) { showInfo(t("msg.reply_deleted")); return; }
    }
    row.scrollIntoView({ block: "center" });
    row.classList.remove("jump-flash");
    void row.offsetWidth;
    row.classList.add("jump-flash");
    setTimeout(function () { row.classList.remove("jump-flash"); }, 1700);
}

// isNearBottom — пользователь «у низа» ленты (с запасом на последний бабл).
export function isNearBottom() {
    var el = dom.messagesDiv;
    return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
}

// scrollToBottom(onlyIndicator=false) — прокрутить ленту вниз. Если передан
// true — не прокручивать, а показать кнопку «новые сообщения ↓», чтобы
// входящее не сбивало чтение истории выше.
export function scrollToBottom(onlyIndicator) {
    var btn = document.getElementById("scroll-bottom-btn");
    if (onlyIndicator) {
        if (btn) { btn.classList.add("visible", "has-new"); }
        return;
    }
    dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
    if (btn) btn.classList.remove("visible", "has-new");
}

// Кнопка «вниз»: показываем, когда пользователь ушёл вверх от низа ленты.
(function initScrollBottomBtn() {
    var btn = document.getElementById("scroll-bottom-btn");
    if (!btn || !dom.messagesDiv) return;
    dom.messagesDiv.addEventListener("scroll", function () {
        if (isNearBottom()) btn.classList.remove("visible", "has-new");
        else btn.classList.add("visible");
    }, { passive: true });
    btn.addEventListener("click", function () { scrollToBottom(); });
})();

function appendRow(msg) {
    // Дедупликация: если сообщение уже в DOM — не добавляем повторно
    if (dom.messagesDiv.querySelector('.message-row[data-msg-id="' + cssEscape(msg.id) + '"]')) return;

    // Убираем placeholder "Сообщений пока нет" при добавлении первого сообщения
    var emptyState = dom.messagesDiv.querySelector(".empty-state");
    if (emptyState) emptyState.remove();

    // System message: звонок (type='call' в БД; content "answered:<sec>" |
    // "missed" | "no_answer" | "declined"). Пишется бэкендом на обеих сторонах.
    if (msg.msgType === "call") {
        var callRow = document.createElement("div");
        callRow.className = "message-row system-message";
        callRow.dataset.msgId = msg.id;
        var callBubble = document.createElement("div");
        callBubble.className = "system-bubble";
        var info = parseCallContent(msg.content, msg.outbound);
        var cd = new Date(msg.timestamp);
        var chh = cd.getHours().toString().padStart(2, "0");
        var cmm = cd.getMinutes().toString().padStart(2, "0");
        var callIcon = document.createElement("i");
        callIcon.setAttribute("data-lucide", info.icon);
        callIcon.className = "icon-14";
        callBubble.appendChild(callIcon);
        callBubble.appendChild(document.createTextNode(" " + info.label + " · " + chh + ":" + cmm));
        callRow.appendChild(callBubble);
        dom.messagesDiv.appendChild(callRow);
        if (window.lucide) lucide.createIcons({ nodes: [callRow] });
        trackLastMessage(msg);
        return;
    }

    // System message: missed call
    if (msg.msgType === "missed_call") {
        var sysRow = document.createElement("div");
        sysRow.className = "message-row system-message";
        sysRow.dataset.msgId = msg.id;
        var sysBubble = document.createElement("div");
        sysBubble.className = "system-bubble";
        var d = new Date(msg.timestamp);
        var hh = d.getHours().toString().padStart(2, "0");
        var mm = d.getMinutes().toString().padStart(2, "0");
        sysBubble.innerHTML = '<i data-lucide="phone-missed" class="icon-14"></i> ' +
            t("call.missed") + ' &middot; ' + hh + ':' + mm;
        sysRow.appendChild(sysBubble);
        dom.messagesDiv.appendChild(sysRow);
        if (window.lucide) lucide.createIcons({ nodes: [sysRow] });
        dom.messagesDiv.scrollTop = dom.messagesDiv.scrollHeight;
        return;
    }

    // Группа: сообщение от заблокированного (фаза 7b, решение Тима) — строка
    // «скрыто · показать»; клик строит настоящее сообщение на том же месте.
    if (state.selectedGroupID && !msg.outbound && msg.senderId && isBlocked(msg.senderId) && !revealedBlocked.has(String(msg.id))) {
        var hidRow = document.createElement("div");
        hidRow.className = "message-row inbound";
        hidRow.dataset.msgId = msg.id;
        var hid = document.createElement("div");
        hid.className = "blocked-hidden-msg";
        hid.appendChild(document.createTextNode(t("block.hidden_msg") + " · "));
        var show = document.createElement("b");
        show.textContent = t("block.show");
        hid.appendChild(show);
        hiddenBlockedMsgs.set(String(msg.id), msg); // раскрытие — делегированный слушатель выше
        hidRow.appendChild(hid);
        dom.messagesDiv.appendChild(hidRow);
        return;
    }

    var wrapper = document.createElement("div");
    wrapper.className = "message-row " + (msg.outbound ? "outbound" : "inbound");
    wrapper.dataset.msgId = msg.id;

    var bubble = document.createElement("div");
    bubble.className = "message-bubble";

    // Контекстное меню (макет ContextMenus): Редактировать / Копировать текст /
    // Выделить / Отменить отправку · Удалить у меня / Удалить у всех.
    wrapper.addEventListener("contextmenu", (function (m) {
        return function (e) {
            if (selectMode) { e.preventDefault(); return; }
            var items = buildMessageMenu(m, wrapper);
            if (items.length > 0) showContextMenu(e, items);
        };
    })(msg));

    // Не ушло (✕): клик по пузырю — отправить ещё раз
    if (msg.failed) {
        wrapper.classList.add("failed");
        wrapper.addEventListener("click", function () {
            if (selectMode) return;
            forgetMessage(msg.id);
            wrapper.remove();
            if (_retrySend) _retrySend(msg.content || "", msg.replyTo || null);
        });
    }
    // Отклонено сервером: строка в БД настоящая, повтор — тем же id.
    if (msg.sendFailed) {
        wrapper.classList.add("failed");
        bindRelayRetry(wrapper, msg.id);
    }

    if (msg.msgType === "voice") {
        renderVoiceBubble(bubble, wrapper, msg);
        return;
    }

    if (msg.msgType === "attachment") {
        renderAttachmentBubble(bubble, wrapper, msg);
        return;
    }

    // Text message
    if (state.selectedGroupID && !msg.outbound && msg.senderId) {
        var senderEl = document.createElement("div");
        senderEl.className = "bubble-sender";
        senderEl.textContent = msg.senderName || msg.senderId.substring(0, 16);
        senderEl.style.setProperty("--h", String(hueFromId(msg.senderId)));
        bubble.appendChild(senderEl);
    }

    if (msg.sendFailed) {
        bubble.appendChild(buildSendFailedLine(msg.sendFailed));
    }

    if (msg.failed) {
        var failedEl = document.createElement("div");
        failedEl.className = "bubble-failed";
        // «Не отправлено · нет связи с CHORT» (макет MessagesText); повтор — по клику
        failedEl.textContent = t("msg.failed") + " · " + (msg.failReason || t("msg.retry_hint"));
        if (msg.failReason) failedEl.title = t("msg.retry_hint");
        bubble.appendChild(failedEl);
    }

    if (msg.forwardedFrom && msg.forwardedFrom.name) bubble.appendChild(buildForwardLine(msg.forwardedFrom.name));
    if (msg.replyTo && msg.replyTo.id) bubble.appendChild(buildQuote(msg.replyTo));

    var content = document.createElement("span");
    content.className = "bubble-content";
    content.textContent = msg.content;
    bubble.appendChild(content);

    var meta = buildMeta(msg);
    bubble.appendChild(meta);
    wrapper.appendChild(bubble);
    dom.messagesDiv.appendChild(wrapper);

    trackLastMessage(msg);
}

// photosAllPortrait — true если у нас есть размеры фото И минимум одно
// чётко портретное. Используется в paths где нет ориентационных метаданных
// (старые сообщения без width/height) — там по дефолту landscape.
//
// Threshold ≥ 1.05 (height/width) чтобы квадраты считались landscape — иначе
// при mix square+portrait получается «портретный» layout который для квадратов
// плохо смотрится.
function photosAllPortrait(photos) {
    if (!photos || photos.length === 0) return false;
    var portraits = 0;
    var hasDimensions = false;
    for (var i = 0; i < photos.length; i++) {
        var p = photos[i];
        if (!p.width || !p.height) continue;
        hasDimensions = true;
        if (p.height >= p.width * 1.05) portraits++;
    }
    if (!hasDimensions) return false;
    // Считаем portrait-layout если ВСЕ известные фото портретные.
    return portraits === photos.filter(function (p) {
        return p.width && p.height;
    }).length;
}

// triggerFileDownload — клик на «Скачать» в bubble. Backend FilesService
// найдёт source-peer (тот кто прислал manifest) и запустит pull. Прогресс
// будет отрисован через vox:download-updated в overlay.
function triggerFileDownload(att) {
    if (!att.sha256) {
        console.warn("[FILE-DL] no sha for attachment id=" + att.id);
        return;
    }
    startFileDownload(att.fromPeer || "", att.sha256);
}

// startFileDownload — общий вход для «Скачать» (первый клик и после cancel).
// Оптимистично переводим bubble в состояние «качается» (× + шкала 0 %) ещё
// до ответа бэкенда: поднятие files PC через TURN занимает секунды, и без
// этого кнопка выглядит «не нажавшейся». Повторный клик за это время —
// no-op. Если бэкенд отказал — возвращаем «Скачать».
function startFileDownload(fromPeer, sha) {
    if (downloads.isActive(sha)) return;
    var resumeFrom = downloads.pausedFraction(sha);
    downloads.upsertProgress(sha, Math.round(resumeFrom * 1000), 1000, 0);
    svc("StartFileDownload", fromPeer, sha).catch(function (err) {
        console.error("[FILE-DL] start failed:", err);
        downloads.markCancelled(sha);
        showError(String(err && err.message || err), { title: t("attach.title") });
    });
}

// CANCEL_GRACE_MS — клик по «×» в первые мс после его появления игнорируем:
// пользователь, не увидев реакции, часто кликает по кнопке ещё раз-два и
// попадал уже в cancel.
var CANCEL_GRACE_MS = 700;
function armCancelButton(btn, sha) {
    btn.dataset.armedAt = String(Date.now());
    btn.addEventListener("click", function (e) {
        e.stopPropagation();
        if (Date.now() - Number(btn.dataset.armedAt || 0) < CANCEL_GRACE_MS) return;
        cancelFileDownload(sha);
    });
}

// attachReceiverProgressListeners — для каждого att с sha подписываемся на
// vox:download-updated. Пока download активен — overlay с %; когда complete —
// force-reload всех <img src=/vox-attach/sha>, чтобы свежий файл подгрузился
// (раньше src возвращал 404 пока chunks ещё качались).
function attachReceiverProgressListeners(attEl, atts) {
    var shas = [];
    for (var i = 0; i < atts.length; i++) {
        var sha = atts[i].sha256;
        if (!sha) continue;
        shas.push(sha);
        // Если уже идёт активная скачка — сразу нарисовать overlay; если
        // загрузка остановлена на середине — показать сколько уже есть.
        if (downloads.isActive(sha)) {
            applyPerFileDownloadUI(attEl, sha, downloads.fractionDone(sha));
        } else if (downloads.pausedFraction(sha) > 0) {
            applyPausedUI(attEl, sha, downloads.pausedFraction(sha));
        }
    }
    if (shas.length === 0) return;

    var listener = function (e) {
        var sha = e.detail && e.detail.fileSha;
        if (!sha || shas.indexOf(sha) < 0) return;
        var st = downloads.getState(sha);
        if (e.detail && e.detail.cancelled) {
            // Cancel: убираем прогресс UI И возвращаем «Скачать»-кнопку,
            // оставляя на карточке процент уже скачанного.
            removeDownloadOverlay(attEl);
            restoreDownloadButton(attEl, sha);
            if (downloads.pausedFraction(sha) > 0) {
                applyPausedUI(attEl, sha, downloads.pausedFraction(sha));
            }
            return;
        }
        if (!st) {
            removeDownloadOverlay(attEl);
            return;
        }
        if (st.complete) {
            removeDownloadOverlay(attEl);
            replacePhotoPlaceholders(attEl, sha);
            replaceFileDownloadIcons(attEl, sha);
            forceReloadAttImgs(attEl, sha);
        } else {
            applyPerFileDownloadUI(attEl, sha, downloads.fractionDone(sha));
        }
    };
    document.addEventListener("vox:download-updated", listener);
    _downloadListeners.push(listener);
}

// restoreDownloadButton — после cancel: возвращаем «×» обратно в «⬇» (Download)
// и привязываем click → triggerFileDownload. Обратная операция к
// applyFileDownloadProgress / replaceFileDownloadIcons.
function restoreDownloadButton(attEl, sha) {
    var cards = attEl.querySelectorAll('.attach-file[data-sha="' + cssEscape(sha) + '"]');
    for (var i = 0; i < cards.length; i++) {
        var card = cards[i];
        var action = card.querySelector(".attach-file-action");
        if (action) {
            action.classList.remove("is-cancel");
            action.title = t("attach.download_all");
            action.innerHTML = '<i data-lucide="download" class="icon-16"></i>';
            var fresh = action.cloneNode(true);
            action.parentNode.replaceChild(fresh, action);
            fresh.addEventListener("click", (function (s, from) {
                return function (e) {
                    e.stopPropagation();
                    startFileDownload(from, s);
                };
            })(sha, card.dataset.fromPeer || ""));
            if (window.lucide) lucide.createIcons({ nodes: [fresh] });
        }
    }
    // Photo cells: убираем оверлей, восстанавливаем placeholder с кнопкой.
    var cells = attEl.querySelectorAll('.attach-photo[data-sha="' + cssEscape(sha) + '"]');
    for (var j = 0; j < cells.length; j++) {
        var cell = cells[j];
        if (!cell.classList.contains("needs-download")) continue;
        var ring = cell.querySelector(".attach-photo-progress");
        if (ring) ring.remove();
        cell.classList.remove("is-downloading");
    }
}

// applyPerFileDownloadUI — для всех photo cells / file cards с этим sha
// рисует прогресс ВНУТРИ карточки + cancel-кнопку. Заменяет старый общий
// overlay на bubble. Юзер видит прогресс ровно на том файле что качается.
function applyPerFileDownloadUI(attEl, sha, fraction) {
    var pct = Math.round(fraction * 100);

    // Photo cells (download placeholder уже есть, добавляем поверх ring + cancel)
    var photoCells = attEl.querySelectorAll('.attach-photo[data-sha="' + cssEscape(sha) + '"]');
    for (var i = 0; i < photoCells.length; i++) {
        applyPhotoDownloadProgress(photoCells[i], sha, pct);
    }

    // File cards
    var fileCards = attEl.querySelectorAll('.attach-file[data-sha="' + cssEscape(sha) + '"]');
    for (var j = 0; j < fileCards.length; j++) {
        applyFileDownloadProgress(fileCards[j], sha, pct);
    }
}

function applyPhotoDownloadProgress(cell, sha, pct) {
    cell.classList.add("is-downloading");
    var ring = cell.querySelector(".attach-photo-progress");
    if (!ring) {
        ring = document.createElement("div");
        ring.className = "attach-photo-progress";
        ring.innerHTML = '<div class="attach-photo-progress-fill"></div>' +
                         '<button type="button" class="attach-photo-cancel" title="' + t("attach.cancel") + '">' +
                            '<i data-lucide="x" class="icon-18"></i>' +
                         '</button>' +
                         '<div class="attach-photo-progress-text">0%</div>';
        cell.appendChild(ring);
        armCancelButton(ring.querySelector(".attach-photo-cancel"), sha);
        if (window.lucide) lucide.createIcons({ nodes: [ring] });
    }
    var fill = ring.querySelector(".attach-photo-progress-fill");
    if (fill) fill.style.width = pct + "%";
    var text = ring.querySelector(".attach-photo-progress-text");
    if (text) text.textContent = pct + "%";
}

function applyFileDownloadProgress(card, sha, pct) {
    card.classList.add("is-downloading");
    card.classList.remove("is-paused");

    // 1. Action-кнопка справа: иконка × + клик cancel.
    var action = card.querySelector(".attach-file-action");
    if (action && !action.classList.contains("is-cancel")) {
        action.classList.add("is-cancel");
        action.title = t("attach.cancel");
        action.innerHTML = '<i data-lucide="x" class="icon-16"></i>';
        var fresh = action.cloneNode(true);
        action.parentNode.replaceChild(fresh, action);
        armCancelButton(fresh, sha);
        if (window.lucide) lucide.createIcons({ nodes: [fresh] });
    }

    // 2. Прогресс — тонкая линия снизу карточки + текст процента справа от размера.
    var bar = card.querySelector(".attach-file-progress");
    if (!bar) {
        bar = document.createElement("div");
        bar.className = "attach-file-progress";
        bar.innerHTML = '<div class="attach-file-progress-fill"></div>';
        card.appendChild(bar);
    }
    bar.classList.remove("is-paused");
    var fill = bar.querySelector(".attach-file-progress-fill");
    if (fill) fill.style.width = pct + "%";

    // 3. Размер заменяем на «X% · N MB» пока качается.
    var sizeEl = card.querySelector(".attach-file-size");
    if (sizeEl && !sizeEl.dataset.origText) {
        sizeEl.dataset.origText = sizeEl.textContent;
    }
    if (sizeEl) {
        // «38% · 41 из 108 МБ · 1,2 МБ/с» (макет MessagesFiles)
        var st = downloads.getState(sha);
        var parts = [pct + "%"];
        if (st && st.bytes > 0 && sizeEl.dataset.origText) {
            parts.push(t("attach.of_size").replace("{done}", formatBytes(st.bytes)).replace("{total}", sizeEl.dataset.origText));
        } else {
            parts.push(sizeEl.dataset.origText || "");
        }
        if (st && st.speed > 0) parts.push(formatBytes(st.speed) + t("attach.per_sec"));
        sizeEl.textContent = parts.join(" · ");
    }

    // 4. Click на саму карточку во время скачки — игнорируем (cancel — только через ×).
    card.style.pointerEvents = "auto";
}

function cancelFileDownload(sha) {
    svc("CancelFileDownload", sha).catch(function (err) {
        console.error("[FILE-CANCEL] failed:", err);
    });
}

// replacePhotoPlaceholders — после complete: cell с .needs-download для этого
// sha заменяем на готовый <img src=/vox-attach/sha>. Восстанавливаем
// click → openLightbox + contextmenu.
function replacePhotoPlaceholders(attEl, sha) {
    var cells = attEl.querySelectorAll('.attach-photo.needs-download[data-sha="' + cssEscape(sha) + '"]');
    for (var i = 0; i < cells.length; i++) {
        var cell = cells[i];
        cell.classList.remove("needs-download");
        cell.style.cursor = "zoom-in";
        cell.innerHTML = "";
        var img = document.createElement("img");
        img.alt = "";
        img.loading = "lazy";
        img.src = "/vox-attach/" + sha + "?t=" + Date.now();
        cell.appendChild(img);
        // Re-bind click → openLightbox. Без msg в closure не знаем полный
        // photos-array — но bubble-владелец имеет attEl.dataset.msgId. Простое
        // решение: клик открывает лайтбокс ровно с этим one-photo. Если фото
        // в album с другими — пока компромисс.
        cell.addEventListener("click", (function (s) {
            return function (e) {
                e.stopPropagation();
                openLightbox([{ src: "/vox-attach/" + s, sha256: s, name: "", mime: "image/jpeg" }]);
            };
        })(sha));
    }
}

// replaceFileDownloadIcons — после complete: action-кнопка "download" в
// .attach-file меняется на "open" + click handler становится Open native.
function replaceFileDownloadIcons(attEl, sha) {
    var cards = attEl.querySelectorAll('.attach-file[data-sha="' + cssEscape(sha) + '"]');
    for (var i = 0; i < cards.length; i++) {
        var card = cards[i];
        var action = card.querySelector(".attach-file-action");
        if (action) {
            action.title = t("ctx.open");
            action.innerHTML = '<i data-lucide="external-link" class="icon-16"></i>';
            // clone чтобы убрать старые listener'ы.
            var fresh = action.cloneNode(true);
            action.parentNode.replaceChild(fresh, action);
            fresh.addEventListener("click", (function (s) {
                return function (e) {
                    e.stopPropagation();
                    // Open через legacy: attachmentID = sha (CAS-fallback в copyAttachmentTo).
                    var name = card.querySelector(".attach-file-name");
                    svc("OpenAttachment", s, name ? name.textContent : "").catch(function (err) {
                        console.error("[FILE-OPEN] failed:", err);
                    });
                };
            })(sha));
        }
        // На самой карточке тоже сменить click — был triggerFileDownload, стал Open.
        var freshCard = card.cloneNode(true);
        card.parentNode.replaceChild(freshCard, card);
        freshCard.style.cursor = "pointer";
        freshCard.addEventListener("click", (function (s) {
            return function () {
                var name = freshCard.querySelector(".attach-file-name");
                svc("OpenAttachment", s, name ? name.textContent : "").catch(function () {});
            };
        })(sha));
        // Re-render lucide icons in this fresh card.
        if (window.lucide) lucide.createIcons({ nodes: [freshCard] });
    }
}

// removeDownloadOverlay — снимает все per-file UI (на случай complete/cancel/error).
// Вызывается из listener'а download-updated с пустым state.
function removeDownloadOverlay(attEl) {
    // Photo: убираем прогресс-ring, восстанавливаем оригинальный cell.
    var photoRings = attEl.querySelectorAll(".attach-photo-progress");
    for (var i = 0; i < photoRings.length; i++) photoRings[i].remove();
    var photoCells = attEl.querySelectorAll(".attach-photo.is-downloading");
    for (var k = 0; k < photoCells.length; k++) photoCells[k].classList.remove("is-downloading");

    // File: убираем линию, восстанавливаем размер.
    var bars = attEl.querySelectorAll(".attach-file-progress");
    for (var j = 0; j < bars.length; j++) bars[j].remove();
    var sizes = attEl.querySelectorAll(".attach-file-size");
    for (var s = 0; s < sizes.length; s++) {
        var el = sizes[s];
        if (el.dataset.origText) {
            el.textContent = el.dataset.origText;
            delete el.dataset.origText;
        }
    }
    var cards = attEl.querySelectorAll(".attach-file.is-downloading, .attach-file.is-paused");
    for (var c = 0; c < cards.length; c++) {
        cards[c].classList.remove("is-downloading");
        cards[c].classList.remove("is-paused");
    }
    var badges = attEl.querySelectorAll(".attach-photo-download-size");
    for (var b = 0; b < badges.length; b++) {
        if (badges[b].dataset.origText) {
            badges[b].textContent = badges[b].dataset.origText;
            delete badges[b].dataset.origText;
        }
    }
}

// applyPausedUI — остановленная на середине загрузка (cancel / peer offline /
// перезапуск): карточка в состоянии «Скачать», но с процентом и приглушённой
// полоской, кнопка подписана «Продолжить». Без этого после отмены не видно,
// сколько уже скачано, и кажется, что всё придётся качать заново.
function applyPausedUI(attEl, sha, fraction) {
    var pct = Math.round(fraction * 100);
    if (pct <= 0) return;
    var cards = attEl.querySelectorAll('.attach-file[data-sha="' + cssEscape(sha) + '"]');
    for (var i = 0; i < cards.length; i++) {
        var card = cards[i];
        if (card.classList.contains("is-downloading")) continue;
        card.classList.add("is-paused");
        var bar = card.querySelector(".attach-file-progress");
        if (!bar) {
            bar = document.createElement("div");
            bar.className = "attach-file-progress";
            bar.innerHTML = '<div class="attach-file-progress-fill"></div>';
            card.appendChild(bar);
        }
        bar.classList.add("is-paused");
        var fill = bar.querySelector(".attach-file-progress-fill");
        if (fill) fill.style.width = pct + "%";
        var sizeEl = card.querySelector(".attach-file-size");
        if (sizeEl) {
            if (!sizeEl.dataset.origText) sizeEl.dataset.origText = sizeEl.textContent;
            sizeEl.textContent = pct + "% · " + sizeEl.dataset.origText;
        }
        var action = card.querySelector(".attach-file-action");
        if (action && !action.classList.contains("is-cancel")) action.title = t("attach.resume");
    }
    var cells = attEl.querySelectorAll('.attach-photo.needs-download[data-sha="' + cssEscape(sha) + '"]');
    for (var j = 0; j < cells.length; j++) {
        var badge = cells[j].querySelector(".attach-photo-download-size");
        if (badge) {
            if (!badge.dataset.origText) badge.dataset.origText = badge.textContent;
            badge.textContent = pct + "% · " + badge.dataset.origText;
        }
        var btn = cells[j].querySelector(".attach-photo-download");
        if (btn) btn.title = t("attach.resume");
    }
}

// forceReloadAttImgs — после complete: кэш-buster в src чтобы webview не
// держал 404 в памяти. Триггерит свежий GET /vox-attach/sha.
function forceReloadAttImgs(attEl, sha) {
    var imgs = attEl.querySelectorAll('img[src*="/vox-attach/' + sha + '"]');
    for (var i = 0; i < imgs.length; i++) {
        var url = imgs[i].src.split("?")[0];
        imgs[i].src = url + "?t=" + Date.now();
    }
}

// buildBulkDownloadBtn — компактная icon-only кнопка, вставляется в bubble-meta.
// Отдельной строки не занимает; title работает как tooltip при наведении.
function buildBulkDownloadBtn(msg) {
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "attach-bulk-download";
    btn.title = t("attach.download_all");
    btn.setAttribute("aria-label", t("attach.download_all"));
    btn.innerHTML = '<i data-lucide="download" class="icon-14"></i>';
    btn.addEventListener("click", function (e) {
        e.stopPropagation();
        btn.disabled = true;
        var msgKind = state.selectedGroupID ? "group" : "dm";
        svc("SaveAllAttachmentsForMessage", msg.id, msgKind)
            .then(function (res) {
                if (!res || res.cancelled) return;
                showSuccess(
                    t("attach.saved_count")
                        .replace("{saved}", res.saved)
                        .replace("{total}", res.total),
                    { title: t("attach.title") }
                );
            })
            .catch(function (err) {
                console.error("[ATTACH-DOWNLOAD-ALL] error:", err);
                showError(String(err && err.message || err), { title: t("attach.save_failed") });
            })
            .finally(function () { btn.disabled = false; });
    });
    return btn;
}

// --- Shared helpers ---

// buildMeta — время и статус. В текстовом пузыре это последний элемент с
// float:right: короткое сообщение помещается в одну строку со временем,
// длинное обтекает его (макет MessagesText). «изм.» — перед временем.
// Статусы: часы — ждёт сеть, ✓ — доставлено, ✓✓ — прочитано, ✕ — не ушло.
function buildMeta(msg) {
    var meta = document.createElement("span");
    meta.className = "bubble-meta";

    if (msg.edited) {
        var ed = document.createElement("span");
        ed.className = "bubble-edited";
        ed.textContent = t("msg.edited");
        meta.appendChild(ed);
    }

    var time = document.createElement("span");
    time.className = "bubble-time";
    time.textContent = new Date(msg.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
    meta.appendChild(time);

    if (msg.outbound) {
        var status = document.createElement("span");
        status.className = "bubble-status";
        if (msg.failed || msg.sendFailed) {
            status.innerHTML = FAILED_SVG;
            status.classList.add("failed");
        } else if (msg.read) {
            status.innerHTML = DOUBLE_CHECK_SVG;
        } else if (msg.delivered) {
            status.innerHTML = SINGLE_CHECK_SVG;
        } else {
            status.innerHTML = CLOCK_SVG;
            status.classList.add("pending");
        }
        meta.appendChild(status);
    }

    return meta;
}

function renderVoiceBubble(bubble, wrapper, msg) {
    var voiceDiv = document.createElement("div");
    voiceDiv.className = "voice-bubble";
    voiceDiv.dataset.msgId = msg.id;
    voiceDiv.dataset.durMs = msg.duration || 0;

    var isLong = needsDownload(msg.duration || 0);
    var isPending = msg.outbound && !msg.delivered;

    var playBtn = document.createElement("button");
    playBtn.className = "voice-play-btn";
    playBtn.dataset.msgId = msg.id;

    if (isPending) {
        // Недоставленное — крестик для отмены
        playBtn.innerHTML = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>';
        playBtn.title = "Cancel";
        playBtn.addEventListener("click", function () {
            svc("CancelMessage", msg.id).then(function () {
                var row = wrapper.closest ? wrapper : wrapper.parentElement;
                if (row) row.remove();
                refreshPeers();
            }).catch(function (err) { console.error("cancel voice:", err); });
        });
    } else {
        playBtn.innerHTML = (isLong && !msg.outbound) ? downloadIconSvg : playIconSvg;
        playBtn.addEventListener("click", function () { togglePlayVoice(msg.id, playBtn); });
    }

    var waveDiv = document.createElement("div");
    waveDiv.className = "voice-waveform";
    waveDiv.id = "wave-" + msg.id;
    waveDiv.style.cursor = "pointer";
    var peaks = (msg.waveform || "").split(",").map(Number).filter(function (v) { return v > 0; });
    if (peaks.length === 0) {
        for (var fi = 0; fi < 32; fi++) peaks.push(20);
    }
    for (var bi = 0; bi < peaks.length; bi++) {
        var bar = document.createElement("div");
        bar.className = "wave-bar";
        bar.style.height = peaks[bi] + "%";
        waveDiv.appendChild(bar);
    }
    // Seek по клику на waveform
    waveDiv.addEventListener("click", function (e) {
        var rect = waveDiv.getBoundingClientRect();
        var progress = (e.clientX - rect.left) / rect.width;
        seekVoice(msg.id, Math.max(0, Math.min(1, progress)));
    });

    var voiceBottom = document.createElement("div");
    voiceBottom.className = "voice-bottom";

    var infoDiv = document.createElement("span");
    infoDiv.className = "voice-info";
    infoDiv.id = "vinfo-" + msg.id;
    var durMs = msg.duration || 0;
    if (isLong && !msg.outbound) {
        infoDiv.textContent = formatMmSs(durMs) + " · " + estimateSize(durMs);
    } else {
        infoDiv.textContent = formatMmSs(0) + " / " + formatMmSs(durMs);
    }
    voiceBottom.appendChild(infoDiv);

    var vmeta = buildMeta(msg);
    vmeta.className = "voice-meta";
    voiceBottom.appendChild(vmeta);

    voiceDiv.appendChild(playBtn);
    voiceDiv.appendChild(waveDiv);
    // Скорость 1× / 1,5× / 2× — видна во время воспроизведения (макет MessagesFiles)
    var speedBtn = document.createElement("button");
    speedBtn.type = "button";
    speedBtn.className = "voice-speed";
    speedBtn.textContent = voiceRateLabel();
    speedBtn.title = t("voice.speed");
    speedBtn.addEventListener("click", function (e) { e.stopPropagation(); cycleVoiceRate(); });
    voiceDiv.appendChild(speedBtn);
    // has-voice: время уже в потоке (.voice-bottom), нижний отступ пузыря
    // под absolute-время не нужен — см. voice.css.
    bubble.classList.add("has-voice");
    bubble.appendChild(voiceDiv);
    bubble.appendChild(voiceBottom);
    wrapper.appendChild(bubble);
    dom.messagesDiv.appendChild(wrapper);

    trackLastMessage(msg);
}

function trackLastMessage(msg) {
    if (!state.selectedGroupID) {
        var peerId = msg.outbound ? state.selectedPeerID : msg.senderId;
        if (peerId) { state.lastMessages[peerId] = msg; }
    }
}

// --- Attachment bubble ---

function renderAttachmentBubble(bubble, wrapper, msg) {
    // has-attachment: у .message-bubble нижний отступ под absolute-время не
    // нужен — meta у вложений идёт в потоке (см. attach-picker.css).
    bubble.classList.add("has-attachment");
    var att = document.createElement("div");
    att.className = "attachment-bubble";
    att.dataset.msgId = msg.id;

    // Sender ник в группе
    if (state.selectedGroupID && !msg.outbound && msg.senderId) {
        var senderEl = document.createElement("div");
        senderEl.className = "bubble-sender";
        senderEl.textContent = msg.senderName || msg.senderId.substring(0, 16);
        senderEl.style.setProperty("--h", String(hueFromId(msg.senderId)));
        att.appendChild(senderEl);
    }
    if (msg.forwardedFrom && msg.forwardedFrom.name) att.appendChild(buildForwardLine(msg.forwardedFrom.name));

    // Берём список attachments из msg (backend) или из uploads.state (uploading own).
    var atts = msg.attachments || [];
    var uploadState = uploads.getState(msg.id);
    if (uploadState && (!atts || atts.length === 0)) {
        // Свежеотправленное сообщение — backend ChatMessage ещё не пришёл, используем previews.
        atts = uploadState.attachments;
    }

    // Разделяем фото / файлы
    var photos = [], files = [];
    for (var i = 0; i < atts.length; i++) {
        if (atts[i].kind === "photo") photos.push(atts[i]);
        else files.push(atts[i]);
    }

    if (photos.length > 9) {
        var parts = Math.ceil(photos.length / 9);
        for (var pi = 0; pi < parts; pi++) {
            var chunk = photos.slice(pi * 9, pi * 9 + 9);
            att.appendChild(buildPhotoGrid(msg, chunk, photos, pi * 9));
            var partLbl = document.createElement("div");
            partLbl.className = "album-part-label";
            partLbl.textContent = t("msg.album_part").replace("{n}", pi + 1).replace("{m}", parts).replace("{k}", chunk.length);
            att.appendChild(partLbl);
        }
    } else if (photos.length > 0) {
        att.appendChild(buildPhotoGrid(msg, photos));
    }
    if (files.length > 0) {
        att.appendChild(buildFilesList(msg, files));
    }

    // Caption
    if (msg.content) {
        var cap = document.createElement("div");
        cap.className = "attach-caption";
        cap.textContent = msg.content;
        att.appendChild(cap);
    }

    // Meta (time + status). Для входящих сообщений с >1 вложением внедряем
    // кнопку «Скачать все» в ту же строку что и время — она не добавляет
    // отдельный row и не раздвигает bubble вниз.
    var showBulkDownload = atts.length > 1 && !msg.outbound && !uploadState;
    var meta = buildMeta(msg);
    if (showBulkDownload) {
        meta.classList.add("has-bulk-download");
        meta.insertBefore(buildBulkDownloadBtn(msg), meta.firstChild);
    }
    att.appendChild(meta);

    bubble.appendChild(att);
    wrapper.appendChild(bubble);
    dom.messagesDiv.appendChild(wrapper);

    // КРИТИЧНО: lucide рендерит иконки после того как элементы УЖЕ в DOM.
    // Внутри buildFilesList/buildPhotoGrid вызов на detached subtree иногда
    // не рендерит → остаются пустые <i data-lucide>. Делаем финальный проход.
    if (window.lucide) lucide.createIcons({ nodes: [att] });

    // ─── Receiver-side download progress ───
    // Для каждого att с sha если в downloads-state есть active — повесить
    // overlay "идёт скачка X%" + listener на vox:download-updated.
    // По complete — force-reload <img src=/vox-attach/sha> чтобы убрать 404.
    if (!msg.outbound) {
        attachReceiverProgressListeners(att, atts);
    }

    // Если есть active upload — подвешиваем overlay + listener на progress.
    if (uploadState) {
        applyUploadOverlay(att, msg, uploadState);
        var listener = function (e) {
            if (!e.detail || e.detail.msgId !== msg.id) return;
            var st = uploads.getState(msg.id);
            if (!st) {
                document.removeEventListener("vox:upload-updated", listener);
                clearUploadOverlay(att);
                return;
            }
            applyUploadOverlay(att, msg, st);
            if (uploads.aggregateStatus(st) === "complete") {
                document.removeEventListener("vox:upload-updated", listener);
                // КРИТИЧНО: до revokeObjectURL (внутри unregister) подменяем
                // img.src у outbound-превьюх на реальные attachment-URL.
                // Иначе revoke убивает blob, но <img src=blob:...> остаются
                // в DOM и показывают «broken image» — свои же фото.
                swapBubbleToRealAttachments(att, st);
                setTimeout(function () { uploads.unregister(msg.id); }, 200);
            }
        };
        document.addEventListener("vox:upload-updated", listener);
    }

    trackLastMessage(msg);
}

// buildPhotoGrid(msg, photos, allPhotos, offset) — сетка до 9 фото. allPhotos
// и offset — для части большого альбома: лайтбокс листает весь альбом.
function buildPhotoGrid(msg, photos, allPhotos, offset) {
    var all = allPhotos || photos;
    var base = offset || 0;
    var box = document.createElement("div");
    var n = photos.length;
    // count-N + ориентация (portrait | landscape). Telegram-style layout:
    //   landscape всех → большое сверху, мелкие снизу
    //   portrait всех → большое слева, мелкие справа стэкаются
    // Mixed считаем landscape (привычнее).
    var countCls;
    if (n === 1) countCls = "count-1";
    else if (n === 2) countCls = "count-2";
    else if (n === 3) countCls = "count-3";
    else if (n === 4) countCls = "count-4";
    else if (n === 5) countCls = "count-5";
    else if (n === 6) countCls = "count-6";
    else countCls = "count-many";

    var orientCls = photosAllPortrait(photos) ? "is-portrait" : "is-landscape";
    box.className = "attach-photos " + countCls + " " + orientCls;

    // Single-photo: aspect-ratio из stored width/height — оставляем место заранее,
    // нет layout shift при загрузке blob.
    if (n === 1 && photos[0].width && photos[0].height) {
        box.style.setProperty("--photo-ratio", photos[0].width + " / " + photos[0].height);
    }

    // 7-9 фото показываем все в сетке 3×3, 10+ — 9 видны + "+N" overlay.
    var maxShown = n >= 10 ? 9 : n;
    for (var i = 0; i < maxShown; i++) {
        var p = photos[i];
        var cell = document.createElement("div");
        cell.className = "attach-photo";
        if (i === maxShown - 1 && n > maxShown) {
            cell.classList.add("has-more");
            cell.setAttribute("data-more", n - maxShown);
        }

        // Manual-download для inbound photos с !available — placeholder + Download.
        // data-sha — селектор для replaceDownloadPlaceholder при complete.
        var needsDownload = !msg.outbound && p.available === false;
        if (needsDownload && p.sha256 && p.progress > 0) downloads.notePaused(p.sha256, p.progress);
        if (p.sha256) cell.setAttribute("data-sha", p.sha256);
        if (needsDownload) {
            cell.classList.add("needs-download");
            var dlBtn = document.createElement("button");
            dlBtn.type = "button";
            dlBtn.className = "attach-photo-download";
            dlBtn.title = t("attach.download_all");
            dlBtn.innerHTML = '<i data-lucide="download" class="icon-24"></i>';
            cell.appendChild(dlBtn);
            // Размер — отдельным span'ом поверх cell, видим всегда.
            var sizeBadge = document.createElement("div");
            sizeBadge.className = "attach-photo-download-size";
            sizeBadge.textContent = formatBytes(p.size || 0);
            cell.appendChild(sizeBadge);
            // Клик в любую точку cell — запуск скачки.
            cell.style.cursor = "pointer";
            cell.addEventListener("click", (function (att) {
                return function (e) {
                    e.stopPropagation();
                    triggerFileDownload(att);
                };
            })(p));
        } else {
            var img = document.createElement("img");
            img.alt = p.name || "";
            img.loading = "lazy";
            // Пустой URL (нет ни localUrl, ни sha, ни id) — не ставим src="":
            // браузер тут же кидает error и ячейка становилась «утеряна».
            var previewUrl = photoPreviewUrl(msg, p, img);
            if (previewUrl) img.src = previewUrl;
            // Blob утерян (очистка вложений / 404 от /vox-attach) — вместо
            // «сломанной картинки» браузера показываем заглушку с именем файла.
            img.onerror = (function (att, c) {
                return function () { markPhotoLost(c, att); };
            })(p, cell);
            cell.appendChild(img);
            cell.addEventListener("click", (function (idx) {
                return function (e) {
                    e.stopPropagation();
                    if (e.currentTarget.classList.contains("attach-photo-lost")) return;
                    openLightbox(buildLightboxItems(all, msg), { startIndex: base + idx });
                };
            })(i));
            cell.addEventListener("contextmenu", (function (idx) {
                return function (e) { e.stopPropagation(); showPhotoContextMenu(e, all, base + idx, msg); };
            })(i));
        }
        box.appendChild(cell);
    }
    return box;
}

// markPhotoLost — фото не загрузилось (blob удалён/утерян): заглушка с иконкой
// и именем файла вместо broken-image, клик по ячейке больше не открывает lightbox.
function markPhotoLost(cell, att) {
    if (!cell || cell.classList.contains("attach-photo-lost")) return;
    cell.classList.add("attach-photo-lost");
    cell.innerHTML = "";
    var inner = document.createElement("div");
    inner.className = "attach-photo-lost-inner";
    inner.innerHTML = '<i data-lucide="image-off" class="icon-24"></i>';
    var nameEl = document.createElement("span");
    nameEl.textContent = att.name || "";
    inner.appendChild(nameEl);
    cell.appendChild(inner);
    cell.style.cursor = "default";
    cell.title = t("attach.lost");
    if (window.lucide) lucide.createIcons({ nodes: [inner] });
}

// buildLightboxItems — элементы лайтбокса (макет Lightbox): автор, время и
// текст сообщения берутся из msg; подпись под фото — текст, не имя файла.
function buildLightboxItems(photos, msg) {
    var out = [];
    for (var i = 0; i < photos.length; i++) out.push(lightboxItem(photos[i], msg));
    return out;
}

function lightboxItem(p, msg) {
    // sha256 → /vox-attach/<sha> (CAS); fallback на id → legacy attachmentBlobURL.
    // localUrl применяется сразу (для свежих outbound bubble).
    return {
        src: p.localUrl || "",
        caption: (msg && msg.content) || "",
        sha256: p.sha256 || null,
        id: p.id || null,
        name: p.name || "",
        mime: p.mime || "image/jpeg",
        senderId: msg ? (msg.outbound ? state.myID : String(msg.senderId || state.selectedPeerID || "")) : "",
        senderName: msg ? senderNameOf(msg) : "",
        timestamp: msg ? msg.timestamp : 0,
        msgId: msg ? msg.id : "",
    };
}

function buildFilesList(msg, files) {
    var box = document.createElement("div");
    box.className = "attach-files";
    if (files.length > 1) box.classList.add("is-stacked");
    for (var i = 0; i < files.length; i++) {
        var f = files[i];
        var card = document.createElement("div");
        card.className = "attach-file";
        if (f.sha256) card.setAttribute("data-sha", f.sha256);

        // Для image-mime файлов (отправленных как kind=file) — реальный thumbnail
        // вместо generic "image" иконки. Telegram-style: видишь содержимое сразу.
        var isImageFile = f.mime && f.mime.indexOf("image/") === 0;
        var iconWrap;
        if (isImageFile) {
            iconWrap = document.createElement("div");
            iconWrap.className = "attach-file-icon attach-file-icon-thumb";
            var thumbImg = document.createElement("img");
            thumbImg.alt = f.name || "";
            thumbImg.loading = "lazy";
            thumbImg.src = photoPreviewUrl(msg, f, thumbImg);
            iconWrap.appendChild(thumbImg);
        } else {
            iconWrap = document.createElement("div");
            iconWrap.className = "attach-file-icon";
            var icon = document.createElement("i");
            icon.setAttribute("data-lucide", iconForFile(f.mime, f.name));
            icon.className = "icon-20";
            iconWrap.appendChild(icon);
        }

        var extInfo = fileExtInfo(f.name);
        var ext = extInfo.ext;
        if (!isImageFile && ext) {
            iconWrap.classList.add("has-ext");
            iconWrap.dataset.kind = extInfo.kind;
            iconWrap.textContent = ext.slice(0, 4);
        }

        var meta = document.createElement("div");
        meta.className = "attach-file-meta";
        var name = document.createElement("div");
        name.className = "attach-file-name";
        name.textContent = f.name || "—";
        name.title = f.name || "";
        var size = document.createElement("div");
        size.className = "attach-file-size";
        size.textContent = formatBytes(f.size) + (ext ? " · " + ext : "");
        meta.appendChild(name);
        meta.appendChild(size);

        // Download-mode: для inbound и !available — Click = StartFileDownload.
        // Иначе (своё или уже скачано) — Click = Open native.
        var needsDownload = !msg.outbound && f.available === false;
        if (needsDownload && f.sha256 && f.progress > 0) downloads.notePaused(f.sha256, f.progress);
        if (f.fromPeer) card.dataset.fromPeer = f.fromPeer;
        card.style.cursor = "pointer";
        card.addEventListener("click", (function (att, isImg, dl) {
            return function () {
                if (dl) {
                    triggerFileDownload(att);
                    return;
                }
                if (isImg && att.id) {
                    var lbIt = lightboxItem(att, msg);
                    lbIt.src = photoPreviewUrl(msg, att);
                    openLightbox([lbIt]);
                } else {
                    openAttachmentNative(att);
                }
            };
        })(f, isImageFile, needsDownload));

        // Action иконка справа: download-icon если файл ещё не скачан, иначе Open.
        var action = document.createElement("button");
        action.className = "attach-file-action";
        action.type = "button";
        if (needsDownload) {
            action.title = t("attach.download_all"); // переиспользуем строку
            action.innerHTML = '<i data-lucide="download" class="icon-16"></i>';
            action.addEventListener("click", (function (att) {
                return function (e) {
                    e.stopPropagation();
                    triggerFileDownload(att);
                };
            })(f));
        } else {
            action.title = "Open";
            action.innerHTML = '<i data-lucide="external-link" class="icon-16"></i>';
            action.addEventListener("click", (function (att) {
                return function (e) {
                    e.stopPropagation();
                    openAttachmentNative(att);
                };
            })(f));
        }

        // Right-click → меню (Open / Save As / Forward)
        card.addEventListener("contextmenu", (function (att) {
            return function (e) { e.stopPropagation(); showFileContextMenu(e, att, msg); };
        })(f));

        card.appendChild(iconWrap);
        card.appendChild(meta);
        card.appendChild(action);
        box.appendChild(card);
    }
    // lucide.createIcons НЕ вызываем здесь — box ещё detached. Внешний
    // renderAttachmentBubble сделает финальный проход после appendChild.
    return box;
}

export function openAttachmentNative(att) {
    if (!att.id) return;
    svc("OpenAttachment", att.id, att.name || "").catch(function (err) {
        var msg = String(err && err.message || err);
        if (msg.indexOf("файл утерян") >= 0) {
            showError(att.name || t("attach.file_generic"), { title: t("attach.lost") });
            return;
        }
        console.error("open attachment:", err);
        showError(msg, { title: t("attach.open_failed") });
    });
}

function saveAttachmentAs(att) {
    if (!att.id) return;
    svc("SaveAttachmentAs", att.id, att.name || "attachment").catch(function (err) {
        console.error("save attachment:", err);
    });
}

// buildAttachmentItems — пункты вложения по макету ContextMenus («Вложение ·
// фото или файл»): Открыть · Открыть в программе · Сохранить как… · Переслать ·
// Копировать изображение | Показать в папке. Общий для ленты и вкладки Медиа.
//   opts.isPhoto — фото (лайтбокс, копирование); opts.onOpen — «Открыть» для
//   фото (лайтбокс); opts.imgEl — уже загруженная картинка под курсором.
export function buildAttachmentItems(att, opts) {
    opts = opts || {};
    var items = [];
    if (opts.isPhoto) {
        if (opts.onOpen) items.push({ label: t("ctx.open"), icon: "maximize", action: opts.onOpen });
        if (att.id) items.push({ label: t("ctx.open_external"), icon: "external-link", action: function () { openAttachmentNative(att); } });
    } else {
        items.push({ label: t("ctx.open"), icon: "external-link", action: function () { openAttachmentNative(att); } });
    }
    if (!att.id) return items;
    items.push({ label: t("ctx.save_as"), icon: "download", action: function () { saveAttachmentAs(att); } });
    items.push({ label: t("info.media.forward"), icon: "forward", action: function () { showAttachForwardDialog(att); } });
    if (opts.isPhoto) {
        items.push({ label: t("ctx.copy_image"), icon: "clipboard-copy", action: function () { copyAttachmentImage(att, opts.imgEl); } });
    }
    // «Показать в папке» — только у CAS-файла, который уже лежит на диске
    // (id = sha256); legacy-blob и нескачанное показать негде.
    if (att.id.length === 64 && att.available !== false) {
        items.push({ separator: true });
        items.push({ label: t("ctx.reveal"), icon: "folder-open", action: function () { revealAttachment(att); } });
    }
    return items;
}

function showFileContextMenu(e, att, msg) {
    var items = buildAttachmentItems(att, {});
    appendMessageItems(items, e, msg);
    showContextMenu(e, items);
}

function showPhotoContextMenu(e, photos, idx, msg) {
    var p = photos[idx];
    var img = e.currentTarget && e.currentTarget.querySelector ? e.currentTarget.querySelector("img") : null;
    var items = buildAttachmentItems(p, {
        isPhoto: true,
        imgEl: img,
        onOpen: function () { openLightbox(buildLightboxItems(photos, msg), { startIndex: idx }); },
    });
    appendMessageItems(items, e, msg);
    showContextMenu(e, items);
}

// copyAttachmentImage — PNG в буфер обмена. Оригинал берём по /vox-attach/<sha>
// (CAS), иначе — уже показанную картинку (legacy blob URL).
async function copyAttachmentImage(att, imgEl) {
    try {
        var src;
        if (att.sha256 || (att.id && att.id.length === 64)) {
            var resp = await fetch("/vox-attach/" + (att.sha256 || att.id));
            if (!resp.ok) throw new Error("HTTP " + resp.status);
            src = await createImageBitmap(await resp.blob());
        } else if (imgEl && imgEl.complete && imgEl.naturalWidth) {
            src = imgEl;
        } else {
            throw new Error(t("attach.lost"));
        }
        var canvas = document.createElement("canvas");
        canvas.width = src.naturalWidth || src.width;
        canvas.height = src.naturalHeight || src.height;
        canvas.getContext("2d").drawImage(src, 0, 0);
        var png = await new Promise(function (resolve) { canvas.toBlob(resolve, "image/png"); });
        await navigator.clipboard.write([new ClipboardItem({ "image/png": png })]);
        showSuccess(t("ctx.image_copied"));
    } catch (err) {
        showError(String(err && err.message || err), { title: t("ctx.copy_image") });
    }
}

function revealAttachment(att) {
    svc("RevealAttachment", att.id).catch(function (err) {
        var msg = String(err && err.message || err);
        if (msg.indexOf("not_on_disk") >= 0) { showInfo(t("ctx.reveal_not_on_disk")); return; }
        showError(msg, { title: t("ctx.reveal") });
    });
}

function appendMessageItems(items, e, msg) {
    if (!msg || selectMode) return;
    var row = e.currentTarget && e.currentTarget.closest ? e.currentTarget.closest(".message-row") : null;
    var more = buildMessageMenu(msg, row);
    if (more.length > 0) {
        items.push({ separator: true });
        for (var i = 0; i < more.length; i++) items.push(more[i]);
    }
}

// showAttachForwardDialog — переслать одно вложение (меню вложения, медиа,
// лайтбокс) тем же диалогом, что и сообщения: мультивыбор и подпись.
export function showAttachForwardDialog(att) {
    if (!att || !att.id) return;
    forwardAttachments([att]);
}

// forwardAttachments — переслать вложения (фаза 7e: и выбранные в «Медиа»).
// Файлы нового формата (id = sha) — одним сообщением через ForwardFileShas:
// ForwardAttachment знает только старую таблицу attachments и на них
// отвечал «source not found». Старые — по одному ForwardAttachment.
// Подпись уходит с первым сообщением.
export function forwardAttachments(atts) {
    atts = (atts || []).filter(function (a) { return a && a.id; });
    if (!atts.length) return;
    var shas = atts.filter(function (a) { return a.id.length === 64; }).map(function (a) { return a.id; });
    var legacy = atts.filter(function (a) { return a.id.length !== 64; });
    pickForwardTargets(async function (targets, caption) {
        var ok = 0;
        for (var i = 0; i < targets.length; i++) {
            var tg = targets[i];
            var peer = tg.kind === "peer" ? tg.id : "";
            var group = tg.kind === "group" ? tg.id : "";
            var cap = caption;
            try {
                if (shas.length) {
                    await svc("ForwardFileShas", shas, peer, group, cap);
                    cap = "";
                }
                for (var j = 0; j < legacy.length; j++) {
                    await svc("ForwardAttachment", legacy[j].id, peer, group, cap);
                    cap = "";
                }
                ok++;
            } catch (err) {
                var msg = String(err && err.message || err);
                showError(msg.indexOf("not_downloaded") >= 0 ? t("info.media.not_downloaded") : msg, { title: t("info.media.forward") });
            }
        }
        if (ok > 0) showSuccess(t("info.media.forward_sent"));
        refreshPeers();
    });
}

function applyUploadOverlay(att, msg, st) {
    clearUploadOverlay(att);

    var status = uploads.aggregateStatus(st);
    var prog = uploads.aggregateProgress(st);

    if (status === "complete") {
        att.classList.remove("is-uploading", "is-failed");
        return;
    }

    att.classList.toggle("is-uploading", status === "uploading");
    att.classList.toggle("is-failed", status === "failed");

    // Полоска прогресса снизу баббла
    var bar = document.createElement("div");
    bar.className = "attach-upload-bar";
    var fill = document.createElement("div");
    fill.className = "attach-upload-bar-fill";
    fill.style.width = Math.round(prog.ratio * 100) + "%";
    bar.appendChild(fill);
    att.appendChild(bar);

    // Overlay с текстом + cancel/retry
    var overlay = document.createElement("div");
    overlay.className = "attach-upload-overlay";
    if (status === "failed") overlay.classList.add("is-failed");

    var text = document.createElement("div");
    text.className = "attach-upload-text";
    if (status === "failed") {
        var err = uploads.getFirstError(st);
        text.textContent = (t("attach.send_failed") || "Send failed") + (err ? ": " + err : "");
    } else {
        text.textContent = formatBytes(prog.sent) + " / " + formatBytes(prog.total);
    }
    overlay.appendChild(text);

    if (status === "uploading") {
        var cancel = document.createElement("button");
        cancel.className = "attach-upload-cancel";
        cancel.type = "button";
        cancel.title = t("attach.remove") || "Cancel";
        cancel.innerHTML = '<i data-lucide="x" class="icon-14"></i>';
        cancel.addEventListener("click", function () {
            svc("CancelAttachmentTransfer", msg.id).catch(function (err) {
                console.error("cancel attach:", err);
            });
        });
        overlay.appendChild(cancel);
    } else if (status === "failed") {
        var retry = document.createElement("button");
        retry.className = "attach-retry-btn";
        retry.type = "button";
        retry.textContent = "Retry";
        retry.addEventListener("click", function () {
            // Точечный retry: backend найдёт pending peers (DM или group) и
            // перезапустит sendAttachmentsToPeer. Текущий failed-state баббла
            // сбросим в uploading, чтобы overlay начал двигаться.
            var st0 = uploads.getState(msg.id);
            if (st0) {
                var keys = Object.keys(st0.peers);
                for (var i = 0; i < keys.length; i++) {
                    var p = st0.peers[keys[i]];
                    if (p.status === "failed") {
                        p.status = "uploading";
                        p.error = null;
                    }
                }
            }
            svc("RetryAttachmentSend", msg.id).catch(function (err) {
                console.error("retry attach:", err);
                showError(String(err && err.message || err), { title: t("attach.send_failed") });
            });
        });
        overlay.appendChild(retry);
    }

    att.appendChild(overlay);
    if (window.lucide) lucide.createIcons({ nodes: [overlay] });
}

function clearUploadOverlay(att) {
    var existingBar = att.querySelector(".attach-upload-bar");
    if (existingBar) existingBar.remove();
    var existingOverlay = att.querySelector(".attach-upload-overlay");
    if (existingOverlay) existingOverlay.remove();
    att.classList.remove("is-uploading", "is-failed");
}

// --- Helpers ---

function photoPreviewUrl(msg, p, imgEl) {
    // Outbound сообщение в процессе загрузки → используем localUrl из preview.
    if (p.localUrl) return p.localUrl;
    if (p.sha256) return "/vox-attach/" + p.sha256;
    if (p.id) return attachmentBlobURL(p, imgEl);
    return "";
}

// PLACEHOLDER — пустой 1×1 SVG. Возвращается пока blob едет с backend.
// Используется только legacy-путём (attachments без sha256 в meta).
var ATTACH_PLACEHOLDER = "data:image/svg+xml;utf8,%3Csvg xmlns='http://www.w3.org/2000/svg' width='1' height='1'/%3E";

// Legacy fetch через GetAttachmentBytes — оставлен для совместимости с
// возможными старыми сообщениями без sha256. Новый путь — /vox-attach/<sha>
// через AssetBridge, без копирования в JS heap.
var _attachUrlCache = new Map();
var _attachImgPending = new Map();

function attachmentBlobURL(att, imgEl) {
    var cached = _attachUrlCache.get(att.id);
    if (cached) return cached;

    if (!_attachImgPending.has(att.id)) {
        _attachImgPending.set(att.id, new Set());
        svc("GetAttachmentBytes", att.id).then(function (bytes) {
            var bin;
            if (typeof bytes === "string") {
                var raw = atob(bytes);
                bin = new Uint8Array(raw.length);
                for (var i = 0; i < raw.length; i++) bin[i] = raw.charCodeAt(i);
            } else if (bytes instanceof ArrayBuffer) {
                bin = new Uint8Array(bytes);
            } else {
                bin = new Uint8Array(bytes);
            }
            var blob = new Blob([bin], { type: att.mime || "image/jpeg" });
            var url = URL.createObjectURL(blob);
            _attachUrlCache.set(att.id, url);
            var waiters = _attachImgPending.get(att.id);
            _attachImgPending.delete(att.id);
            if (waiters) {
                waiters.forEach(function (img) {
                    if (img && img.isConnected) img.src = url;
                });
            }
        }).catch(function (err) {
            console.error("fetch attachment bytes:", err);
            _attachImgPending.delete(att.id);
        });
    }
    if (imgEl) _attachImgPending.get(att.id).add(imgEl);
    return ATTACH_PLACEHOLDER;
}

// Экспорт для lightbox: синхронный URL (sha-path, localUrl, или legacy).
// Для новых attachments (с sha256) возвращается сразу реальный /vox-attach/<sha>
// без заходов через svc.
export function resolveAttachmentURL(att, imgEl) {
    if (!att) return "";
    if (att.localUrl) return att.localUrl;
    if (att.sha256) return "/vox-attach/" + att.sha256;
    if (att.id) return attachmentBlobURL(att, imgEl);
    return att.src || "";
}

// swapBubbleToRealAttachments заменяет в bubble все <img src=blob:localUrl...>
// на URL от attachmentBlobURL(realAtt). Вызывается ровно перед unregister'ом
// (который revoke'нет localUrl). Порядок: state.attachments[i] ↔ state.realAttachments[i].
function swapBubbleToRealAttachments(bubbleEl, st) {
    if (!st || !st.realAttachments) return;
    var previews = st.attachments || [];
    var reals = st.realAttachments;
    for (var i = 0; i < previews.length && i < reals.length; i++) {
        var localUrl = previews[i] && previews[i].localUrl;
        if (!localUrl) continue;
        var imgs = bubbleEl.querySelectorAll('img[src="' + CSS.escape(localUrl) + '"]');
        // Fallback для старых WebView без CSS.escape (v2 ancient)
        if (!imgs || imgs.length === 0) {
            imgs = bubbleEl.querySelectorAll('img[src="' + localUrl + '"]');
        }
        var real = reals[i];
        for (var k = 0; k < imgs.length; k++) {
            if (real && real.sha256) {
                imgs[k].src = "/vox-attach/" + real.sha256;
            } else if (real) {
                imgs[k].src = attachmentBlobURL(real, imgs[k]);
            }
        }
    }
}

// fileExtInfo — «PDF» и вид для цвета значка (.has-ext[data-kind]) — карточка
// файла в ленте и строка во вкладке Медиа (info-media.js).
export function fileExtInfo(name) {
    var m = /\.([a-z0-9]{1,5})$/i.exec(name || "");
    var ext = m ? m[1].toUpperCase() : "";
    var kind = /^PDF$/.test(ext) ? "pdf"
        : /^(ZIP|RAR|7Z|TAR|GZ|XZ)$/.test(ext) ? "zip"
        : /^(DOC|DOCX|ODT|RTF|TXT|MD)$/.test(ext) ? "doc"
        : /^(XLS|XLSX|CSV|ODS)$/.test(ext) ? "sheet"
        : /^(MP3|WAV|OGG|FLAC|M4A|OPUS)$/.test(ext) ? "audio"
        : /^(MP4|MKV|MOV|AVI|WEBM)$/.test(ext) ? "video" : "";
    return { ext: ext, kind: kind };
}

function iconForFile(mime, name) {
    mime = mime || "";
    if (mime.indexOf("image/") === 0) return "image";
    if (mime.indexOf("video/") === 0) return "film";
    if (mime.indexOf("audio/") === 0) return "music";
    if (mime === "application/pdf") return "file-text";
    var ext = ((name || "").split(".").pop() || "").toLowerCase();
    if (["zip", "rar", "7z", "tar", "gz", "bz2"].indexOf(ext) !== -1) return "file-archive";
    if (["doc", "docx", "txt", "md", "rtf"].indexOf(ext) !== -1) return "file-text";
    if (["xls", "xlsx", "csv"].indexOf(ext) !== -1) return "table";
    return "file";
}

// --- Rename group dialog ---

export function showRenameGroupDialog(groupId, currentName) {
    var body = '<div class="settings-field">' +
        '<input type="text" class="settings-input" placeholder="' + t("group.rename_placeholder") + '" maxlength="32" autocomplete="off"></div>';

    var dlg = buildDialogOverlay(t("group.rename_title"), body, t("group.cancel"), t("group.rename"), function (overlay) {
        var input = overlay.querySelector(".settings-input");
        var newName = input.value.trim();
        if (!newName || newName === currentName) { overlay.remove(); return; }
        svc("RenameGroup", groupId, newName).then(function () {
            overlay.remove();
            refreshPeers();
        }).catch(function (err) {
            svc("Notify", "Error", err.message || String(err)).catch(function () {});
        });
    });

    var input = dlg.overlay.querySelector(".settings-input");
    input.value = currentName;
    input.focus();
    input.select();
    input.addEventListener("keydown", function (e) {
        if (e.key === "Enter" && dlg.confirmBtn) dlg.confirmBtn.click();
    });
}


// ============== Лента: модель, серии, подгрузка, выбор (фаза 3a) ==============

var PAGE_SIZE = 50;
var SERIES_GAP_MS = 60 * 1000;       // серия — один автор, интервал < 1 мин
var currentMsgs = [];                 // сообщения открытого чата, от старых к новым
var hasMore = false;                  // на сервере есть более старые
var loadingOlder = null;              // Promise текущей подгрузки
var renderedKey = "";                 // чат, для которого нарисована лента
var unreadAnchorId = null;            // id сообщения под разделителем «Новые»
var _retrySend = null;                // chat.js: повтор неотправленного текста
export function setRetrySend(fn) { _retrySend = fn; }

var FORWARD_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 17 20 12 15 7"/><path d="M4 18v-2a4 4 0 0 1 4-4h12"/></svg>';
var FAILED_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/></svg>';

function chatKeyNow() {
    if (state.selectedGroupID) return "g:" + state.selectedGroupID;
    if (state.selectedPeerID) return "p:" + state.selectedPeerID;
    return "";
}

function isLocalOnly(m) {
    var id = String(m.id || "");
    return !!m.failed || id.indexOf("sending-") === 0 || id.indexOf("failed-") === 0 || id.indexOf("err-") === 0;
}

// serverCount — сколько сообщений чата уже взято с сервера: offset для
// следующей страницы (локальные «отправка…» / «не ушло» не считаются).
function serverCount() {
    var n = 0;
    for (var i = 0; i < currentMsgs.length; i++) if (!isLocalOnly(currentMsgs[i])) n++;
    return n;
}

export function getCurrentMsgs() { return currentMsgs; }

function hueFromId(hexStr) {
    var h = 0;
    hexStr = String(hexStr || "");
    for (var i = 0; i < hexStr.length && i < 8; i++) h = hexStr.charCodeAt(i) + ((h << 5) - h);
    return ((h % 360) + 360) % 360;
}

function rowById(id) {
    return dom.messagesDiv.querySelector('.message-row[data-msg-id="' + cssEscape(String(id)) + '"]');
}

function buildDateDivider(ts) {
    var divider = document.createElement("div");
    divider.className = "date-divider";
    divider.dataset.date = new Date(ts).toDateString();
    var span = document.createElement("span");
    span.textContent = formatDateLabel(ts);
    divider.appendChild(span);
    return divider;
}

function buildNewDivider() {
    var divider = document.createElement("div");
    divider.className = "date-divider new";
    var span = document.createElement("span");
    span.textContent = t("msg.new_messages");
    divider.appendChild(span);
    return divider;
}

function lastMessageRow() {
    var el = dom.messagesDiv.lastElementChild;
    while (el && !el.classList.contains("message-row")) el = el.previousElementSibling;
    return el;
}

function appendRowDecorated(msg) {
    var before = dom.messagesDiv.lastElementChild;
    appendRow(msg);
    var row = dom.messagesDiv.lastElementChild;
    if (!row || row === before || row.dataset.msgId !== String(msg.id)) return null;
    decorateRow(row, msg);
    return row;
}

// decorateRow — данные серии, аватар автора (группы), кружок выбора.
function decorateRow(row, msg) {
    row.dataset.ts = String(msg.timestamp || 0);
    if (row.classList.contains("system-message")) return;
    row.dataset.sender = msg.outbound ? "me" : String(msg.senderId || "");
    if (state.selectedGroupID && !msg.outbound) {
        row.classList.add("in-group");
        var av = document.createElement("div");
        av.className = "msg-avatar";
        var nm = msg.senderName || String(msg.senderId || "?");
        applyAvatar(av, String(msg.senderId || ""), nm, { letters: 1 });
        av.title = nm;
        row.insertBefore(av, row.firstChild);
    }
    var chk = document.createElement("span");
    chk.className = "msg-check";
    row.insertBefore(chk, row.firstChild);
    if (selectMode && selected.has(String(msg.id))) row.classList.add("is-selected");
    linkSeries(row);
}

// linkSeries — продолжение серии: тот же автор, < 1 мин, без разделителя.
// Имя в группе — у первой строки серии, аватар и хвост-уголок — у последней,
// между сериями отступ 10px (класс gap), внутри серии 2px.
function linkSeries(row) {
    var prev = row.previousElementSibling;
    var prevIsMsg = !!prev && prev.classList.contains("message-row") && !prev.classList.contains("system-message");
    var cont = prevIsMsg && prev.dataset.sender === row.dataset.sender &&
        Math.abs(Number(row.dataset.ts) - Number(prev.dataset.ts)) < SERIES_GAP_MS;
    if (cont) {
        prev.classList.remove("series-last");
        row.classList.add("series-last");
    } else {
        row.classList.add("series-first", "series-last");
        if (prevIsMsg) row.classList.add("gap");
    }
}

// appendMessage — живое сообщение (отправка, входящее): в модель и в конец ленты,
// с разделителем даты при смене дня и продолжением серии.
export function appendMessage(msg) {
    if (rowById(msg.id)) return;
    currentMsgs.push(msg);
    var last = lastMessageRow();
    var lastDate = last ? new Date(Number(last.dataset.ts || 0)).toDateString() : "";
    if (lastDate !== new Date(msg.timestamp).toDateString()) {
        var empty = dom.messagesDiv.querySelector(".empty-state");
        if (empty) empty.remove();
        dom.messagesDiv.appendChild(buildDateDivider(msg.timestamp));
    }
    appendRowDecorated(msg);
}

// markEdited — сообщение изменено (своё или пришло vox:message-edited):
// новый текст в пузыре/подписи и «изм.» перед временем.
export function markEdited(id, content) {
    for (var i = 0; i < currentMsgs.length; i++) {
        if (String(currentMsgs[i].id) === String(id)) {
            if (content != null) currentMsgs[i].content = content;
            currentMsgs[i].edited = true;
        }
    }
    var row = rowById(id);
    if (!row) return;
    var c = row.querySelector(".bubble-content, .attach-caption");
    if (c && content != null) c.textContent = content;
    var meta = row.querySelector(".bubble-meta, .voice-meta");
    if (meta && !meta.querySelector(".bubble-edited")) {
        var ed = document.createElement("span");
        ed.className = "bubble-edited";
        ed.textContent = t("msg.edited");
        meta.insertBefore(ed, meta.firstChild);
    }
}

// Отказ сервера для своего сообщения в группе (код из GroupsService). Флаг
// sendFailed, а не failed: failed — локальный пузырь без строки в БД
// (isLocalOnly), а у отклонённого сервером строка настоящая — его можно
// удалить и повторить тем же id.
var RELAY_TOO_LARGE = "message_too_large";

function sendFailedReason(code) {
    if (code === RELAY_TOO_LARGE) return t("msg.too_long");
    if (code === "not_group_member") return t("msg.not_member");
    return t("msg.retry_hint");
}

function buildSendFailedLine(code) {
    var el = document.createElement("div");
    el.className = "bubble-failed";
    el.textContent = t("msg.failed") + " · " + sendFailedReason(code);
    if (code !== RELAY_TOO_LARGE) el.title = t("msg.retry_hint");
    return el;
}

function bindRelayRetry(wrapper, id) {
    if (wrapper.dataset.relayRetry) return;
    wrapper.dataset.relayRetry = "1";
    wrapper.addEventListener("click", function () {
        if (selectMode) return;
        var m = currentMsgs.find(function (x) { return String(x.id) === String(id); });
        // Слишком длинное повторять бессмысленно — причина уже на пузыре.
        if (!m || !m.sendFailed || m.sendFailed === RELAY_TOO_LARGE) return;
        svc("RetryGroupRelayMessage", String(id)).catch(function (err) {
            console.error("[SEND] повтор не удался:", err);
        });
    });
}

// markSendFailed — сервер отклонил своё сообщение (code) или отказ снят
// (code пуст: delivered — сообщение принято, иначе снова ждёт отправки).
export function markSendFailed(id, code, delivered) {
    for (var i = 0; i < currentMsgs.length; i++) {
        if (String(currentMsgs[i].id) === String(id)) {
            currentMsgs[i].sendFailed = code || "";
            if (delivered) currentMsgs[i].delivered = true;
        }
    }
    var row = rowById(id);
    if (!row) return;
    row.classList.toggle("failed", !!code);
    var old = row.querySelector(".bubble-failed");
    if (old) old.remove();
    if (code) {
        var bubble = row.querySelector(".message-bubble");
        if (bubble) bubble.insertBefore(buildSendFailedLine(code), bubble.firstChild);
        bindRelayRetry(row, id);
    }
    var status = row.querySelector(".bubble-status");
    if (!status) return;
    status.classList.remove("failed", "pending");
    if (code) {
        status.innerHTML = FAILED_SVG;
        status.classList.add("failed");
    } else if (delivered) {
        status.innerHTML = SINGLE_CHECK_SVG;
    } else {
        status.innerHTML = CLOCK_SVG;
        status.classList.add("pending");
    }
}

export function forgetMessage(id) {
    currentMsgs = currentMsgs.filter(function (m) { return String(m.id) !== String(id); });
}

export function scrollToMessage(id) {
    var row = rowById(id);
    if (row) row.scrollIntoView({ block: "center" });
    return row;
}

// --- подгрузка старых при прокрутке вверх ---

function loadOlder() {
    if (loadingOlder) return loadingOlder;
    if (!hasMore) return Promise.resolve(false);
    var key = chatKeyNow();
    var pid = state.selectedPeerID, gid = state.selectedGroupID;
    if (!key || (typeof pid === "string" && pid.indexOf("__mesh:") === 0)) return Promise.resolve(false);
    var offset = serverCount();
    loadingOlder = svc(gid ? "GetGroupMessagesPage" : "GetMessagesPage", gid || pid, PAGE_SIZE, offset).then(function (older) {
        if (chatKeyNow() !== key) return false;
        older = older || [];
        hasMore = older.length >= PAGE_SIZE;
        var have = {};
        for (var i = 0; i < currentMsgs.length; i++) have[String(currentMsgs[i].id)] = true;
        older = older.filter(function (m) { return !have[String(m.id)]; });
        if (older.length === 0) return false;
        currentMsgs = older.concat(currentMsgs);
        renderMessages(null, { preserveScroll: true });
        return true;
    }).catch(function (err) {
        console.error("load older messages:", err);
        hasMore = false;
        return false;
    }).finally(function () { loadingOlder = null; });
    return loadingOlder;
}

// loadOlderUntil(offset) — догрузить историю, пока сообщение с позицией offset
// (от новейшего, см. SearchMessages) не окажется в ленте.
export async function loadOlderUntil(offset) {
    var guard = 0;
    while (serverCount() <= offset && hasMore && guard++ < 400) {
        await loadOlder();
    }
}

dom.messagesDiv.addEventListener("scroll", function () {
    if (dom.messagesDiv.scrollTop < 200 && hasMore && !loadingOlder && renderedKey === chatKeyNow()) loadOlder();
}, { passive: true });

// --- меню сообщения ---

function buildMessageMenu(m, wrapper) {
    var items = [];
    var hasText = !!m.content && m.msgType !== "voice" && m.msgType !== "call" && m.msgType !== "missed_call";
    var editable = m.outbound && !isLocalOnly(m) && (m.msgType === "text" || !m.msgType || (m.msgType === "attachment" && !!m.content));
    var canRef = !isLocalOnly(m) && m.msgType !== "call" && m.msgType !== "missed_call";
    if (canRef && _startReply) {
        items.push({ label: t("ctx.reply"), icon: "reply", action: function () { _startReply(buildReplyRef(m)); } });
    }
    if (editable && wrapper) {
        items.push({ label: t("ctx.edit"), icon: "pencil", hint: "↑", action: function () {
            if (_startEditMessage) _startEditMessage(wrapper, m);
        }});
    }
    if (canRef && m.msgType !== "voice") {
        items.push({ label: t("ctx.forward"), icon: "forward", action: function () { showForwardDialog([m]); } });
    }
    if (hasText) {
        items.push({ label: t("ctx.copy_text"), icon: "copy", hint: m.outbound ? "" : "Ctrl+C", action: function () {
            try { navigator.clipboard.writeText(m.content); } catch (err) { /* окно без фокуса */ }
        }});
    }
    if (!isLocalOnly(m)) {
        items.push({ label: t("ctx.select"), icon: "square-check", action: function () { enterSelectMode(m.id); } });
    }
    if (m.outbound && !m.delivered && !m.read && !isLocalOnly(m)) {
        items.push({ label: t("ctx.cancel_send"), icon: "x-circle", action: function () {
            svc("CancelMessage", m.id).then(function () {
                forgetMessage(m.id);
                renderMessages(null, { preserveScroll: true });
                refreshPeers();
            }).catch(function (err) { console.error("cancel message:", err); });
        }});
    }
    items.push({ separator: true });
    items.push({ label: t("ctx.delete_me"), icon: "trash-2", action: function () { deleteMessages([m], false); } });
    if (m.outbound && !isLocalOnly(m)) {
        items.push({ label: t("ctx.delete_all"), icon: "trash-2", danger: true, action: function () { deleteMessages([m], true); } });
    }
    return items;
}

function deleteMessages(list, forAll) {
    for (var i = 0; i < list.length; i++) {
        var m = list[i];
        if (!isLocalOnly(m)) {
            var p = forAll
                ? svc("DeleteMessageForAll", m.id, state.selectedPeerID || "", state.selectedGroupID || "")
                : svc("DeleteMessage", m.id);
            p.catch(function (err) { console.error("delete message:", err); });
        }
        forgetMessage(m.id);
    }
    renderMessages(null, { preserveScroll: true });
    refreshPeers();
}

// --- выбор сообщений (макет ChatSelect) ---
// Вход: «Выделить» в меню или Ctrl+клик по пузырю. Кружки слева у входящих и
// справа у исходящих, выбранный пузырь — мятная рамка. Шапка → #select-bar,
// строка ввода → полоса «скрыта, пока идёт выбор».

var selectMode = false;
var selected = new Set();
// selectAnchor — последнее отмеченное: от него Shift+клик отмечает диапазон.
var selectAnchor = null;

export function isSelectMode() { return selectMode; }

function enterSelectMode(firstId) {
    if (!selectMode) {
        selectMode = true;
        selected.clear();
        dom.messagesDiv.classList.add("select-mode");
        // Выделение текста, начатое до входа (Ctrl+клик тянул мышью), снимаем:
        // дальше клики только отмечают сообщения.
        var ts = window.getSelection ? window.getSelection() : null;
        if (ts && ts.removeAllRanges) ts.removeAllRanges();
        var ca = document.getElementById("chat-area");
        if (ca) ca.classList.add("select-mode");
        if (dom.inputArea) dom.inputArea.classList.add("select-mode");
        var note = document.getElementById("select-composer-note");
        if (note) note.hidden = false;
    }
    if (firstId != null) toggleSelected(firstId, true);
    updateSelectBar();
}

export function exitSelectMode() {
    if (!selectMode) return;
    selectMode = false;
    selected.clear();
    selectAnchor = null;
    dom.messagesDiv.classList.remove("select-mode");
    var ca = document.getElementById("chat-area");
    if (ca) ca.classList.remove("select-mode");
    if (dom.inputArea) dom.inputArea.classList.remove("select-mode");
    var note = document.getElementById("select-composer-note");
    if (note) note.hidden = true;
    var rows = dom.messagesDiv.querySelectorAll(".message-row.is-selected");
    for (var i = 0; i < rows.length; i++) rows[i].classList.remove("is-selected");
    updateSelectBar();
}

function toggleSelected(id, force) {
    id = String(id);
    var on = force === undefined ? !selected.has(id) : !!force;
    if (on) { selected.add(id); selectAnchor = id; } else selected.delete(id);
    var row = rowById(id);
    if (row) row.classList.toggle("is-selected", on);
    if (selected.size === 0) { exitSelectMode(); return; }
    updateSelectBar();
}

function updateSelectBar() {
    var bar = document.getElementById("select-bar");
    if (!bar) return;
    bar.hidden = !selectMode;
    var c = document.getElementById("select-count");
    if (c) c.textContent = t("select.selected_n").replace("{n}", String(selected.size));
}

function selectedMsgs() {
    return currentMsgs.filter(function (m) { return selected.has(String(m.id)); });
}

function hhmm(ts) {
    var d = new Date(ts);
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
}

function copySelected() {
    var parts = [];
    var list = selectedMsgs();
    for (var i = 0; i < list.length; i++) {
        var m = list[i];
        if (!m.content || m.msgType === "voice") continue;
        if (state.selectedGroupID) {
            var who = m.outbound ? (state.myNickname || state.myShortID || "") : (m.senderName || String(m.senderId || "").substring(0, 8));
            parts.push(who + ", " + hhmm(m.timestamp) + ":\n" + m.content);
        } else {
            parts.push(m.content);
        }
    }
    try {
        navigator.clipboard.writeText(parts.join("\n\n")).then(function () { showSuccess(t("select.copied")); });
    } catch (err) { /* окно без фокуса */ }
    exitSelectMode();
}

// --- пересылка (макет Dialogs «Переслать кому») ---
// Поиск сверху, чаты и группы, мультивыбор, внизу подпись. Текст уходит новым
// сообщением с «Переслано от»; вложения нового формата — ForwardFiles (без
// повторной загрузки, только скачанные у нас); старого — ForwardAttachment.

async function showForwardDialog(list) {
    list = (list || []).filter(function (m) {
        return !isLocalOnly(m) && m.msgType !== "call" && m.msgType !== "missed_call";
    });
    if (list.length === 0) return;
    pickForwardTargets(async function (targets, caption) {
        var sent = 0;
        for (var i = 0; i < targets.length; i++) {
            var tg = targets[i];
            // Подпись — перед пересланным, как вводная к нему.
            if (caption) await sendForwardCaption(tg, caption);
            sent += await doForward(list, tg.kind, tg.id, tg.name, true);
        }
        if (sent > 0) {
            showSuccess(targets.length === 1
                ? t("forward.sent").replace("{name}", targets[0].name)
                : t("forward.sent_n").replace("{n}", String(targets.length)));
        }
    });
}

async function sendForwardCaption(tg, caption) {
    try {
        var r = tg.kind === "group"
            ? await svc("SendGroupMessageWithOptions", tg.id, caption, {})
            : await svc("SendMessageWithOptions", tg.id, caption, {});
        var sameChat = tg.kind === "group" ? state.selectedGroupID === tg.id : (!state.selectedGroupID && state.selectedPeerID === tg.id);
        if (sameChat && r && r.id) {
            appendMessage({ id: r.id, senderId: state.myID, content: caption, timestamp: Date.now(), outbound: true, delivered: r.delivered });
        }
    } catch (err) {
        console.error("forward caption:", err);
    }
}

function pluralMembers(n) {
    var cat = "other";
    try { cat = new Intl.PluralRules(getLocale()).select(n); } catch (e) { /* старый движок */ }
    var s = t("info.n_members." + cat);
    if (s === "info.n_members." + cat) s = t("info.n_members.other");
    return s.replace("{n}", n);
}

// pickForwardTargets — диалог выбора получателей; onDone(targets, caption),
// targets: [{kind: "peer"|"group", id, name}].
export async function pickForwardTargets(onDone) {
    var friends = [];
    try { friends = (await svc("GetFriends")) || []; } catch (err) { console.error("forward: friends", err); }
    var groups = (state.groups || []).filter(function (g) { return !g.left && (!g.type || g.type === 0); });
    if (friends.length === 0 && groups.length === 0) { showError(t("forward.empty")); return; }

    var selected = new Map();
    var dlg = buildDialogOverlay(t("forward.title"), "", t("dialog.cancel"), t("forward.send"), null);
    var confirmBtn = dlg.confirmBtn;
    function syncConfirm() {
        confirmBtn.disabled = selected.size === 0;
        confirmBtn.textContent = selected.size > 0 ? t("forward.send_n").replace("{n}", String(selected.size)) : t("forward.send");
    }
    syncConfirm();

    var wrap = document.createElement("div");
    wrap.className = "dialog-member-list-container";
    var sw = document.createElement("div");
    sw.className = "dialog-search-wrapper";
    sw.innerHTML = '<i data-lucide="search" class="icon-16"></i>';
    var search = document.createElement("input");
    search.type = "text";
    search.className = "dialog-search-input";
    search.placeholder = t("search.placeholder");
    search.autocomplete = "off";
    sw.appendChild(search);
    wrap.appendChild(sw);
    var listEl = document.createElement("div");
    listEl.className = "dialog-member-list";
    wrap.appendChild(listEl);

    var sections = [];
    function section(title, entries) {
        if (entries.length === 0) return;
        var h = document.createElement("div");
        h.className = "forward-section";
        h.textContent = title;
        listEl.appendChild(h);
        var rows = [];
        entries.forEach(function (en) {
            var key = en.kind + ":" + en.id;
            var row = document.createElement("div");
            row.className = "friend-checkbox-label" + (en.online === false ? " is-offline" : "");
            row.dataset.name = en.name.toLowerCase();
            var av = document.createElement("div");
            av.className = "friend-checkbox-avatar";
            if (en.kind === "group") {
                av.innerHTML = '<i data-lucide="users" class="icon-16"></i>';
                applyAvatar(av, "g:" + en.id, en.name, { keepContent: true });
            } else {
                applyAvatar(av, en.id, en.name);
            }
            var info = document.createElement("div");
            info.className = "friend-checkbox-info";
            var nm = document.createElement("span");
            nm.className = "friend-checkbox-name";
            nm.textContent = en.name;
            var sub = document.createElement("span");
            sub.className = "friend-checkbox-subtitle";
            sub.textContent = en.sub;
            info.appendChild(nm);
            info.appendChild(sub);
            var check = document.createElement("span");
            check.className = "friend-check";
            row.appendChild(av);
            row.appendChild(info);
            row.appendChild(check);
            row.addEventListener("click", function () {
                if (selected.has(key)) selected.delete(key); else selected.set(key, { kind: en.kind, id: en.id, name: en.name });
                row.classList.toggle("selected", selected.has(key));
                syncConfirm();
            });
            listEl.appendChild(row);
            rows.push(row);
        });
        sections.push({ header: h, rows: rows });
    }
    section(t("forward.chats"), friends.map(function (f) {
        return { kind: "peer", id: f.peerId, name: f.nickname || String(f.peerId || "").substring(0, 16), online: !!f.online,
            sub: f.online ? t("chat.online") : t("chat.offline") };
    }).sort(function (a, b) {
        if (a.online !== b.online) return a.online ? -1 : 1;
        return a.name.localeCompare(b.name);
    }));
    section(t("forward.groups"), groups.map(function (g) {
        return { kind: "group", id: g.groupId, name: g.name || String(g.groupId || "").substring(0, 8), sub: pluralMembers(g.members || 0) };
    }));

    search.addEventListener("input", function () {
        var q = search.value.trim().toLowerCase();
        sections.forEach(function (sec) {
            var any = false;
            sec.rows.forEach(function (r) {
                var show = !q || r.dataset.name.indexOf(q) >= 0;
                r.style.display = show ? "" : "none";
                if (show) any = true;
            });
            sec.header.hidden = !any;
        });
    });

    var caption = document.createElement("input");
    caption.type = "text";
    caption.className = "dialog-search-input fw-caption";
    caption.placeholder = t("forward.caption");
    caption.maxLength = 4000;
    wrap.appendChild(caption);

    function confirm() {
        if (selected.size === 0) return;
        var targets = Array.from(selected.values());
        var text = caption.value.trim();
        dlg.overlay.remove();
        onDone(targets, text);
    }
    confirmBtn.addEventListener("click", confirm);
    caption.addEventListener("keydown", function (e) { if (e.key === "Enter") { e.preventDefault(); confirm(); } });

    dlg.body.appendChild(wrap);
    if (window.lucide) lucide.createIcons({ nodes: [wrap] });
    setTimeout(function () { try { search.focus(); } catch (e) { /* закрыт */ } }, 0);
}

async function doForward(list, dstKind, dstId, dstName, quiet) {
    var dstPeer = dstKind === "peer" ? dstId : "";
    var dstGroup = dstKind === "group" ? dstId : "";
    var srcKind = state.selectedGroupID ? "group" : "dm";
    var ok = 0, voice = 0, notDownloaded = 0, failed = 0;
    for (var i = 0; i < list.length; i++) {
        var m = list[i];
        var fwdName = (m.forwardedFrom && m.forwardedFrom.name) || senderNameOf(m);
        var sameChat = dstGroup ? state.selectedGroupID === dstGroup : (!state.selectedGroupID && state.selectedPeerID === dstPeer);
        try {
            if (m.msgType === "voice") { voice++; continue; }
            if (m.msgType === "attachment") {
                var res = null;
                try {
                    res = await svc("ForwardFiles", String(m.id), srcKind, dstPeer, dstGroup, m.content || "", fwdName);
                } catch (err) {
                    var es = String(err && err.message || err);
                    if (es.indexOf("not_downloaded") >= 0) { notDownloaded++; continue; }
                    if (es.indexOf("legacy_format") < 0) throw err;
                    // Старый формат вложений — пересылаем по файлу, как раньше.
                    var atts = m.attachments || [];
                    for (var a = 0; a < atts.length; a++) {
                        await svc("ForwardAttachment", atts[a].id, dstPeer, dstGroup, a === 0 ? (m.content || "") : "");
                    }
                    ok++;
                    continue;
                }
                ok++;
                if (sameChat && res && res.id) {
                    appendMessage({
                        id: res.id, senderId: state.myID, content: m.content || "", msgType: "attachment",
                        attachments: m.attachments || [], timestamp: Date.now(), outbound: true, delivered: false,
                        forwardedFrom: { name: fwdName },
                    });
                }
                continue;
            }
            if (!m.content) continue;
            var opts = { forwardedFrom: { name: fwdName } };
            var r = dstGroup
                ? await svc("SendGroupMessageWithOptions", dstGroup, m.content, opts)
                : await svc("SendMessageWithOptions", dstPeer, m.content, opts);
            ok++;
            if (sameChat && r && r.id) {
                appendMessage({
                    id: r.id, senderId: state.myID, content: m.content, timestamp: Date.now(),
                    outbound: true, delivered: r.delivered, forwardedFrom: opts.forwardedFrom,
                });
            }
        } catch (err) {
            failed++;
            console.error("forward:", err);
        }
    }
    if (ok > 0 && !quiet) showSuccess(t("forward.sent").replace("{name}", dstName));
    if (notDownloaded > 0) showError(t("forward.not_downloaded"));
    if (voice > 0) showInfo(t("forward.voice_unsupported"));
    if (failed > 0) showError(t("forward.failed"));
    if (ok > 0 && (dstGroup ? state.selectedGroupID === dstGroup : state.selectedPeerID === dstPeer)) scrollToBottom();
    refreshPeers();
    return ok;
}

function forwardSelected() {
    var list = selectedMsgs();
    exitSelectMode();
    showForwardDialog(list);
}

function deleteSelected() {
    var list = selectedMsgs();
    if (list.length === 0) return;
    var allOwn = list.every(function (m) { return m.outbound; });
    var title = t("select.delete_title").replace("{n}", String(list.length));
    var dlg = buildDialogOverlay(
        title,
        "",
        t("dialog.cancel"),
        t("select.delete_me"),
        function (overlay) { overlay.remove(); deleteMessages(list, false); exitSelectMode(); },
        { noHeader: true, className: "dialog-confirm" }
    );
    // Вопрос — в теле, как в confirmDialog: с шапкой пустое тело давало полосу
    var h = document.createElement("div");
    h.className = "dialog-confirm-title";
    h.textContent = title;
    dlg.body.appendChild(h);
    if (!allOwn) {
        // Без пояснения непонятно, куда делась кнопка «Удалить у всех»
        var note = document.createElement("p");
        note.className = "dialog-confirm-text";
        note.textContent = t("select.delete_others_note");
        dlg.body.appendChild(note);
    }
    if (allOwn) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "backup-done-btn danger";
        b.textContent = t("select.delete_all");
        b.addEventListener("click", function () { dlg.overlay.remove(); deleteMessages(list, true); exitSelectMode(); });
        var actions = dlg.overlay.querySelector(".backup-actions");
        if (actions) actions.appendChild(b);
    }
}

// Клики в режиме выбора (и Ctrl+клик для входа) перехватываем до обработчиков
// пузыря: иначе клик по фото открыл бы лайтбокс, по файлу — скачивание.
dom.messagesDiv.addEventListener("click", function (e) {
    var row = e.target.closest ? e.target.closest(".message-row") : null;
    if (!row || row.classList.contains("system-message") || !row.dataset.msgId) return;
    if (String(row.dataset.msgId).indexOf("sending-") === 0) return;
    if (!selectMode && !(e.ctrlKey || e.metaKey)) return;
    e.preventDefault();
    e.stopPropagation();
    if (!selectMode) enterSelectMode(row.dataset.msgId);
    else if (e.shiftKey && selectAnchor) selectRange(selectAnchor, row.dataset.msgId);
    else toggleSelected(row.dataset.msgId);
}, true);

// selectRange — Shift+клик: отметить все сообщения ленты между anchor и id
// включительно. Якорь остаётся прежним — следующий Shift+клик меняет диапазон
// от той же точки.
function selectRange(anchorId, id) {
    var rows = dom.messagesDiv.querySelectorAll(".message-row:not(.system-message)[data-msg-id]");
    var from = -1, to = -1;
    for (var i = 0; i < rows.length; i++) {
        var rid = rows[i].dataset.msgId;
        if (rid === String(anchorId)) from = i;
        if (rid === String(id)) to = i;
    }
    if (from < 0 || to < 0) { toggleSelected(id); return; }
    if (from > to) { var x = from; from = to; to = x; }
    for (var j = from; j <= to; j++) {
        var mid = rows[j].dataset.msgId;
        if (String(mid).indexOf("sending-") === 0) continue;
        selected.add(mid);
        rows[j].classList.add("is-selected");
    }
    selectAnchor = String(anchorId);
    updateSelectBar();
}

// selectKeysBlocked — клавиши выбора не перехватывают ввод в полях (поиск по
// чату) и не действуют поверх открытого диалога (подтверждение удаления).
function selectKeysBlocked(e) {
    var el = e.target;
    if (el && (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable)) return true;
    return !!document.querySelector(".backup-overlay");
}

(function initSelectBar() {
    var close = document.getElementById("select-close");
    if (close) close.addEventListener("click", exitSelectMode);
    var fwdBtn = document.getElementById("select-forward");
    if (fwdBtn) fwdBtn.addEventListener("click", forwardSelected);
    var copyBtn = document.getElementById("select-copy");
    if (copyBtn) copyBtn.addEventListener("click", copySelected);
    var delBtn = document.getElementById("select-delete");
    if (delBtn) delBtn.addEventListener("click", deleteSelected);
    document.addEventListener("keydown", function (e) {
        if (!selectMode || selectKeysBlocked(e)) return;
        if (e.key === "Escape") {
            exitSelectMode();
        } else if (e.key === "Delete" && !e.ctrlKey && !e.altKey && !e.shiftKey && !e.metaKey) {
            e.preventDefault();
            deleteSelected();
        } else if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && e.code === "KeyC") {
            // По коду клавиши, а не символу: в русской раскладке e.key — «с».
            e.preventDefault();
            copySelected();
        }
    });
    document.addEventListener("vox:ui-chat-changed", exitSelectMode);
})();
