"use strict";

// downloads.js — Receiver-side прогресс CAS-файлов. Аналог uploads.js, но
// per-file (не per-peer): один file_sha может качаться у одного клиента в
// один момент времени, отдельные файлы независимы.
//
// Backend эмитит:
//   vox:file-progress  { fileSha, peer, received, total, bytes }
//   vox:file-received  { fileSha, msgId }
//
// События пробрасываются в DOM:
//   vox:download-updated { detail: { fileSha } }
//
// API:
//   getState(fileSha) → { received, total, complete } | null
//   isActive(fileSha) → bool (не complete)

var states = new Map(); // fileSha → state

// paused — последняя известная доля (0..1) для файлов, которые сейчас НЕ
// качаются: из бэкенда (att.progress при рендере) или запомненная при
// cancel. Нужна, чтобы остановленная карточка показывала «45 % · 61.9 MB»,
// а «Продолжить» стартовал overlay сразу с этого процента, а не с 0.
var paused = new Map(); // fileSha → fraction

export function notePaused(fileSha, fraction) {
    if (!fileSha || !(fraction > 0)) return;
    paused.set(fileSha, Math.min(1, fraction));
}

export function pausedFraction(fileSha) {
    return paused.get(fileSha) || 0;
}

function emit(fileSha) {
    document.dispatchEvent(new CustomEvent("vox:download-updated", { detail: { fileSha: fileSha } }));
}

// upsertProgress — при каждом vox:file-progress.
export function upsertProgress(fileSha, received, total, bytes) {
    if (!fileSha) return;
    var st = states.get(fileSha);
    if (!st) {
        st = { fileSha: fileSha, received: 0, total: total || 1, bytes: 0, complete: false };
        states.set(fileSha, st);
    }
    paused.delete(fileSha);
    st.received = received;
    st.total = total || st.total;
    st.bytes = bytes || (received * (st.bytes / Math.max(st.received, 1) || 0));
    // Скорость (фаза 6d, «1,2 МБ/с» на карточке): сглаженная по событиям,
    // пересчёт не чаще раза в 0,5 с — события приходят на каждый чанк.
    var now = Date.now();
    if (!st.speedAt) {
        st.speedAt = now;
        st.speedBytes = st.bytes;
    } else if (now - st.speedAt >= 500 && st.bytes >= st.speedBytes) {
        var inst = (st.bytes - st.speedBytes) * 1000 / (now - st.speedAt);
        st.speed = st.speed ? st.speed * 0.6 + inst * 0.4 : inst;
        st.speedAt = now;
        st.speedBytes = st.bytes;
    }
    st.complete = (st.total > 0 && st.received >= st.total);
    emit(fileSha);
}

// markCancelled — пользователь нажал «×». Удаляем state, эмитим updated event
// чтобы UI вернулся в initial («Скачать»).
export function markCancelled(fileSha) {
    if (!fileSha) return;
    var frac = fractionDone(fileSha);
    states.delete(fileSha);
    if (frac > 0 && frac < 1) paused.set(fileSha, frac);
    document.dispatchEvent(new CustomEvent("vox:download-updated", {
        detail: { fileSha: fileSha, cancelled: true }
    }));
}

// markComplete — vox:file-received финал.
export function markComplete(fileSha) {
    if (!fileSha) return;
    paused.delete(fileSha);
    var st = states.get(fileSha);
    if (!st) {
        states.set(fileSha, { fileSha: fileSha, received: 1, total: 1, bytes: 0, complete: true });
    } else {
        st.received = st.total;
        st.complete = true;
    }
    emit(fileSha);
    // Через 5 сек убираем из состояния — bubble получил force-reload, дальше
    // он показывает реальный /vox-attach/<sha>.
    setTimeout(function () {
        states.delete(fileSha);
    }, 5000);
}

export function getState(fileSha) {
    return states.get(fileSha) || null;
}

export function isActive(fileSha) {
    var st = states.get(fileSha);
    return !!(st && !st.complete);
}

// fractionDone — 0..1 для рендера progress bar'а.
export function fractionDone(fileSha) {
    var st = states.get(fileSha);
    if (!st || st.total <= 0) return 0;
    return Math.max(0, Math.min(1, st.received / st.total));
}
