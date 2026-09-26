"use strict";

// uploads.js — In-flight upload state tracking.
//
// Backend эмитит:
//   vox:attach-progress { msgId, peerId, sent, total }
//   vox:attach-complete { msgId, peerId }
//   vox:attach-failed   { msgId, peerId, error }
//
// Для DM это один peer, для group — несколько (per-member fanout).
// Этот модуль агрегирует прогресс и нотифицирует UI через DOM event:
//   "vox:upload-updated" { detail: { msgId } }
//
// API:
//   register(msgId, info)       — info: { totalBytes, peerIds[], caption, attachments[] }
//   updateProgress(msgId, peer, sent, total)
//   markPeerComplete(msgId, peer)
//   markPeerFailed(msgId, peer, error)
//   getState(msgId) → state | null
//   isActive(msgId) → bool
//   unregister(msgId)
//
// Агрегация: status = MIN всех peer-статусов;
//   все complete → "complete"; есть failed → "failed"; иначе → "uploading".
// Прогресс = MIN(sentBytes/totalBytes) по всем peer'ам — отражает "ещё не у всех".

var states = new Map(); // msgId → state

function emit(msgId) {
    document.dispatchEvent(new CustomEvent("vox:upload-updated", { detail: { msgId: msgId } }));
}

// register инициализирует state. peerIds — список ожидаемых receivers
// (для DM — один; для group — все участники).
export function register(msgId, info) {
    var peers = {};
    var ids = info.peerIds || [];
    for (var i = 0; i < ids.length; i++) {
        peers[ids[i]] = {
            sent: 0,
            total: info.totalBytes || 0,
            status: "uploading", // "uploading" | "complete" | "failed"
            error: null,
        };
    }
    states.set(msgId, {
        msgId: msgId,
        totalBytes: info.totalBytes || 0,
        caption: info.caption || "",
        attachments: info.attachments || [],
        peers: peers,
        startedAt: Date.now(),
    });
    emit(msgId);
}

export function unregister(msgId) {
    var st = states.get(msgId);
    if (!st) return;
    // Освобождаем object URLs локальных preview
    for (var i = 0; i < st.attachments.length; i++) {
        var a = st.attachments[i];
        if (a.localUrl) { try { URL.revokeObjectURL(a.localUrl); } catch (e) {} }
    }
    states.delete(msgId);
}

export function getState(msgId) {
    return states.get(msgId) || null;
}

export function isActive(msgId) {
    var st = states.get(msgId);
    if (!st) return false;
    return aggregateStatus(st) !== "complete";
}

export function updateProgress(msgId, peerId, sent, total) {
    var st = states.get(msgId);
    if (!st) return;
    var p = st.peers[peerId];
    if (!p) {
        // Получили progress для пира которого мы не ожидали (group race) — добавляем.
        p = { sent: 0, total: total || st.totalBytes, status: "uploading", error: null };
        st.peers[peerId] = p;
    }
    p.sent = sent;
    if (total > 0) p.total = total;
    emit(msgId);
}

export function markPeerComplete(msgId, peerId, realAttachments) {
    var st = states.get(msgId);
    if (!st) return;
    // Backend прислал реальные attachments (id/sha/mime) — сохраняем, чтобы
    // bubble мог переключить img.src с localUrl на attachmentBlobURL до revoke.
    if (realAttachments && realAttachments.length > 0) {
        st.realAttachments = realAttachments;
    }
    var p = st.peers[peerId];
    if (!p) {
        p = { sent: 0, total: st.totalBytes, status: "uploading", error: null };
        st.peers[peerId] = p;
    }

    // Anti-fake: если transfer прошёл мгновенно (<400ms) — прокручиваем
    // progress bar плавно 0→100 за 400ms чтобы юзер увидел движение.
    // Иначе мгновенный bubble выглядит как подмена, а не как P2P-пересылка.
    var elapsed = Date.now() - st.startedAt;
    if (elapsed < 400 && p.sent < p.total) {
        animateToComplete(msgId, peerId, 400 - elapsed);
        return;
    }

    p.status = "complete";
    p.sent = p.total;
    emit(msgId);
}

function animateToComplete(msgId, peerId, durationMs) {
    var st = states.get(msgId);
    if (!st) return;
    var p = st.peers[peerId];
    if (!p) return;
    var startSent = p.sent;
    var delta = p.total - startSent;
    var startTime = Date.now();
    function step() {
        var st2 = states.get(msgId);
        if (!st2) return;
        var p2 = st2.peers[peerId];
        if (!p2 || p2.status === "failed") return;
        var t = (Date.now() - startTime) / durationMs;
        if (t >= 1) {
            p2.status = "complete";
            p2.sent = p2.total;
            emit(msgId);
            return;
        }
        // Easing: cubic ease-out
        var eased = 1 - Math.pow(1 - t, 3);
        p2.sent = startSent + Math.round(delta * eased);
        emit(msgId);
        requestAnimationFrame(step);
    }
    requestAnimationFrame(step);
}

export function markPeerFailed(msgId, peerId, error) {
    var st = states.get(msgId);
    if (!st) return;
    var p = st.peers[peerId];
    if (!p) {
        p = { sent: 0, total: st.totalBytes, status: "failed", error: error || "" };
        st.peers[peerId] = p;
    } else {
        p.status = "failed";
        p.error = error || "";
    }
    emit(msgId);
}

// aggregateStatus возвращает общий статус по worst-case логике:
//   - если есть хоть один failed без других uploading → "failed"
//   - если все complete → "complete"
//   - иначе → "uploading"
export function aggregateStatus(state) {
    var anyFailed = false;
    var anyUploading = false;
    var allComplete = true;
    var keys = Object.keys(state.peers);
    if (keys.length === 0) return "uploading";
    for (var i = 0; i < keys.length; i++) {
        var p = state.peers[keys[i]];
        if (p.status === "failed") anyFailed = true;
        if (p.status === "uploading") anyUploading = true;
        if (p.status !== "complete") allComplete = false;
    }
    if (allComplete) return "complete";
    if (anyFailed && !anyUploading) return "failed";
    return "uploading";
}

// aggregateProgress возвращает MIN bytes/total по всем peer'ам (отражает worst-case).
export function aggregateProgress(state) {
    var keys = Object.keys(state.peers);
    if (keys.length === 0) return { sent: 0, total: state.totalBytes || 1, ratio: 0 };
    var minRatio = 1;
    var totalSent = 0;
    var totalAll = 0;
    for (var i = 0; i < keys.length; i++) {
        var p = state.peers[keys[i]];
        var t = p.total || state.totalBytes || 1;
        var r = t > 0 ? p.sent / t : 0;
        if (r < minRatio) minRatio = r;
        totalSent += p.sent;
        totalAll += t;
    }
    return {
        sent: Math.round((state.totalBytes || 0) * minRatio),
        total: state.totalBytes || totalAll,
        ratio: minRatio,
    };
}

export function getFirstError(state) {
    var keys = Object.keys(state.peers);
    for (var i = 0; i < keys.length; i++) {
        var p = state.peers[keys[i]];
        if (p.status === "failed" && p.error) return p.error;
    }
    return "";
}
