"use strict";

// attach_approve.js — модалка подтверждения приёма больших вложений.
// Показывается на receiver-стороне когда суммарный размер offer'а > 10 MB
// (см. attachApproveThreshold в backend). Слушает event vox:attach-needs-approval.
//
// Кнопки: «Принять» → svc("ApproveAttachment") → backend шлёт ack, transfer идёт.
//         «Отклонить» → svc("DeclineAttachment") → backend шлёт sender'у decline,
//                       cleanup'ает assembly, sender видит attach-failed.

import { svc, formatBytes, state } from "./core.js";
import { t } from "./i18n.js";
import { buildDialogOverlay } from "./ui.js";
import { showError } from "./toast.js";

// Глобальный реестр открытых модалок: msgId → overlay. Иначе при повторном
// событии (после reconnect) открывается дубликат.
var openModals = new Map();

export function initAttachApprove() {
    // События приходят через WailsRuntime → Events.On — централизованно регистрируется
    // в events.js (см. там handleAttachNeedsApproval). Здесь просто экспортируем openModal.
}

// openApprovalModal — публичный API, зовётся из events.js по vox:attach-needs-approval.
// payload: { msgId, peerId, groupId, totalBytes, caption, files: [{name, size, kind, mime, dedup}] }
export function openApprovalModal(payload) {
    if (!payload || !payload.msgId) return;
    if (openModals.has(payload.msgId)) return; // уже открыта

    var senderName = resolvePeerName(payload.peerId, payload.groupId);
    var totalSize = formatBytes(payload.totalBytes || 0);
    var fileCount = (payload.files || []).length;

    var subtitleKey = fileCount === 1 ? "attach.approve_subtitle_one" : "attach.approve_subtitle";
    var subtitle = t(subtitleKey)
        .replace("{name}", escapeHTML(senderName))
        .replace("{count}", fileCount)
        .replace("{size}", totalSize);

    var bodyHTML =
        '<div class="attach-approve-body">' +
            '<div class="attach-approve-subtitle">' + subtitle + '</div>' +
            renderFileList(payload.files || []) +
            (payload.caption
                ? '<div class="attach-approve-caption">' + escapeHTML(payload.caption) + '</div>'
                : '') +
        '</div>';

    var dlg = buildDialogOverlay(
        t("attach.approve_title"),
        bodyHTML,
        t("attach.approve_decline"),
        t("attach.approve_accept"),
        function () {
            // Accept
            dlg.confirmBtn.disabled = true;
            dlg.cancelBtn.disabled = true;
            // Пробуем сначала CAS pipeline (FilesService); если msgID был
            // legacy attachment — backend вернёт ошибку, fallback на старый API.
            svc("ApproveFileDownload", payload.peerId, payload.msgId)
                .catch(function () {
                    return svc("ApproveAttachment", payload.peerId, payload.msgId);
                })
                .catch(function (err) {
                    console.error("[ATTACH-APPROVE] approve failed:", err);
                    showError(String(err && err.message || err));
                })
                .finally(function () {
                    openModals.delete(payload.msgId);
                    dlg.overlay.remove();
                });
        }
    );

    // Cancel-кнопка по умолчанию просто закрывает модалку. Перехватываем —
    // нам нужно ещё отправить decline-сигнал sender'у.
    dlg.cancelBtn.addEventListener("click", function (e) {
        e.stopImmediatePropagation();
        if (dlg.cancelBtn.disabled) return;
        dlg.confirmBtn.disabled = true;
        dlg.cancelBtn.disabled = true;
        svc("DeclineFileDownload", payload.peerId, payload.msgId)
            .catch(function () {
                return svc("DeclineAttachment", payload.peerId, payload.msgId);
            })
            .catch(function (err) {
                console.error("[ATTACH-APPROVE] decline failed:", err);
            })
            .finally(function () {
                openModals.delete(payload.msgId);
                dlg.overlay.remove();
            });
    }, true);

    // Закрытие через Esc/клик-вне модалки тоже = decline (на стороне UX:
    // лучше явный отказ чем "висящий" pending — sender ждёт ack 30 сек).
    var obs = new MutationObserver(function (muts) {
        for (var m = 0; m < muts.length; m++) {
            var removed = muts[m].removedNodes;
            for (var r = 0; r < removed.length; r++) {
                if (removed[r] === dlg.overlay) {
                    obs.disconnect();
                    if (openModals.has(payload.msgId)) {
                        openModals.delete(payload.msgId);
                        svc("DeclineFileDownload", payload.peerId, payload.msgId).catch(function () {
                            return svc("DeclineAttachment", payload.peerId, payload.msgId);
                        }).catch(function () {});
                    }
                    return;
                }
            }
        }
    });
    obs.observe(document.body, { childList: true });

    openModals.set(payload.msgId, dlg.overlay);
}

function renderFileList(files) {
    if (!files.length) return "";
    var html = '<div class="attach-approve-files">';
    for (var i = 0; i < files.length; i++) {
        var f = files[i];
        var icon = iconForFile(f.mime, f.name);
        html += '<div class="attach-approve-file">' +
                '<i data-lucide="' + icon + '" class="icon-18"></i>' +
                '<div class="attach-approve-file-meta">' +
                    '<div class="attach-approve-file-name" title="' + escapeAttr(f.name) + '">' + escapeHTML(f.name) + '</div>' +
                    '<div class="attach-approve-file-size">' + formatBytes(f.size) +
                        (f.dedup ? ' · <span class="attach-approve-dedup">' + t("attach.already_have") + '</span>' : '') +
                    '</div>' +
                '</div>' +
                '</div>';
    }
    html += '</div>';
    return html;
}

function resolvePeerName(peerId, groupId) {
    if (!peerId) return "";
    var peer = state.peers && state.peers.find(function (p) { return p.id === peerId; });
    if (peer && peer.nickname) return peer.nickname;
    if (groupId && state.groups) {
        var grp = state.groups.find(function (g) { return g.groupId === groupId; });
        if (grp && grp.members) {
            var m = grp.members.find(function (m) { return m.peerId === peerId; });
            if (m && m.nickname) return m.nickname;
        }
    }
    return peerId.substring(0, 8);
}

function iconForFile(mime, name) {
    mime = mime || "";
    if (mime.indexOf("image/") === 0) return "image";
    if (mime.indexOf("video/") === 0) return "film";
    if (mime.indexOf("audio/") === 0) return "music";
    if (mime === "application/pdf") return "file-text";
    var ext = ((name || "").split(".").pop() || "").toLowerCase();
    if (["zip", "rar", "7z", "tar", "gz", "bz2"].indexOf(ext) !== -1) return "file-archive";
    return "file";
}

function escapeHTML(s) {
    return String(s == null ? "" : s)
        .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function escapeAttr(s) {
    return escapeHTML(s).replace(/"/g, "&quot;");
}
