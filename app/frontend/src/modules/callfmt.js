"use strict";

// callfmt.js — подписи системных сообщений о звонках и превью в списке
// чатов. Отдельный модуль без зависимостей на messages.js/contacts-list.js:
// оба импортируют его, а друг друга — нет (circular dependency).

import { t } from "./i18n.js";

// parseCallContent — разбор content системного сообщения о звонке в подпись
// и иконку. duration в секундах после "answered:".
export function parseCallContent(content, outbound) {
    var c = String(content || "");
    if (c.indexOf("answered:") === 0) {
        var sec = parseInt(c.slice(9), 10) || 0;
        var m = Math.floor(sec / 60), sPart = sec % 60;
        var dur = m > 0
            ? t("call.dur_ms").replace("{m}", m).replace("{s}", sPart)
            : t("call.dur_s").replace("{s}", sPart);
        return { icon: outbound ? "phone-outgoing" : "phone-incoming",
                 label: (outbound ? t("call.outgoing") : t("call.incoming")) + " · " + dur };
    }
    if (c === "no_answer") return { icon: "phone-missed", label: t("call.outgoing") + " · " + t("call.no_answer") };
    if (c === "declined") return { icon: "phone-missed", label: t("call.declined") };
    return { icon: "phone-missed", label: t("call.missed") };
}

// m:ss из миллисекунд — «0:42», как в макете.
function mss(ms) {
    var s = Math.round((ms || 0) / 1000);
    return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
}

// Вложения: у LastMessage с бэкенда — число + attachmentKind/Name, у живого
// vox:message — массив attachments.
function attachmentInfo(m) {
    var a = m.attachments;
    if (Array.isArray(a)) {
        return { count: a.length, kind: a[0] && a[0].kind, name: a[0] && a[0].name };
    }
    return { count: a || 0, kind: m.attachmentKind, name: m.attachmentName };
}

// previewBody — текст превью без автора (макеты Main / MessagesText):
// «🎤 Голосовое сообщение · 0:42», «📷 3 фото», «📎 Референсы.pdf».
function previewBody(m) {
    var type = m.msgType || m.type;
    if (type === "call") return parseCallContent(m.content, m.outbound).label;
    if (type === "voice") return "🎤 " + t("msg.voice") + (m.duration ? " · " + mss(m.duration) : "");
    var att = attachmentInfo(m);
    if (att.count > 0) {
        if (att.kind === "photo") {
            if (m.content) return "📷 " + m.content;
            return "📷 " + (att.count > 1 ? t("composer.n_photos").replace("{n}", att.count) : t("msg.photo"));
        }
        return "📎 " + (att.name || m.content || t("msg.file").replace("{name}", "").trim());
    }
    return m.content || "";
}

function isMissedCall(m) {
    return (m.msgType || m.type) === "call" && !m.outbound && String(m.content) === "missed";
}

// formatLastMessagePreview — превью строкой (автор + текст).
export function formatLastMessagePreview(lastMsg, isGroup) {
    if (!lastMsg) return "";
    var body = previewBody(lastMsg);
    if ((lastMsg.msgType || lastMsg.type) === "call") return body;
    var author = lastMsg.outbound ? t("composer.you") : (isGroup ? (lastMsg.senderName || "") : "");
    return author ? author + ": " + body : body;
}

// renderLastMessagePreview — превью в элемент строки: автор (в группах —
// мятным жирным), «Вы:» у своих, пропущенный звонок — красным.
export function renderLastMessagePreview(el, lastMsg, isGroup) {
    el.textContent = "";
    el.classList.remove("is-missed");
    if (!lastMsg) return;
    if (isMissedCall(lastMsg)) el.classList.add("is-missed");
    var isCall = (lastMsg.msgType || lastMsg.type) === "call";
    var author = isCall ? "" : (lastMsg.outbound ? t("composer.you") : (isGroup ? (lastMsg.senderName || "") : ""));
    if (author) {
        var a = document.createElement("span");
        a.className = "contact-preview-author" + (lastMsg.outbound ? " is-me" : "");
        a.textContent = author + ": ";
        el.appendChild(a);
    }
    el.appendChild(document.createTextNode(previewBody(lastMsg)));
}
