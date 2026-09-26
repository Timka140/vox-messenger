"use strict";

// conn-stats.js — окно «Статистика соединения» (меню звонка, макет HeaderMenus).
// Только то, что уже меряется: режим (P2P каждый с каждым / SFU), участники,
// битрейт отправки голоса, джиттер приёма, длительность. Новых замеров
// RTT/потерь нет. Обновляется по push-событию vox:voice-bitrate (раз в 2 с),
// без своего таймера.

import { svc, Events, formatMmSs } from "./core.js";
import { t } from "./i18n.js";
import { buildDialogOverlay, escapeHtml } from "./ui.js";
import { getVoiceQuality } from "./voice.js";

let open = null;        // {body, count} — окно открыто
let lastBitrate = null; // последний снимок GetVoiceBitrate / vox:voice-bitrate
let sfu = null;         // GetSFUStatus

// connModeLabel — коротко для пункта меню («P2P» / «SFU»).
export function connModeLabel() {
    return sfu && sfu.active ? "SFU" : "P2P";
}

export function refreshConnMode() {
    return svc("GetSFUStatus").then((s) => { sfu = s || null; return connModeLabel(); })
        .catch(() => connModeLabel());
}

function row(label, value) {
    return '<div class="cs-row"><span>' + escapeHtml(label) + '</span><b>' + escapeHtml(value) + '</b></div>';
}

function paint() {
    if (!open) return;
    if (!open.body.isConnected) { open = null; return; }
    const rows = [
        row(t("stats.mode"), sfu && sfu.active ? t("stats.mode_sfu") : t("stats.mode_p2p")),
        row(t("stats.participants"), String(open.count())),
    ];
    const b = lastBitrate;
    if (b && b.active) {
        rows.push(row(t("stats.bitrate"), Math.round(b.instantKbps || 0) + " / " + Math.round(b.avgKbps || 0) + " " + t("stats.kbps")));
        rows.push(row(t("stats.duration"), formatMmSs((b.sessionSec || 0) * 1000)));
    } else {
        rows.push(row(t("stats.voice"), t("stats.voice_idle")));
    }
    const q = getVoiceQuality();
    rows.push(row(t("stats.jitter"), q && q.jitterMs ? Math.round(q.jitterMs) + " " + t("stats.ms") : "—"));
    open.body.innerHTML = '<div class="conn-stats">' + rows.join("") + '</div>' +
        '<p class="conn-stats-hint">' + escapeHtml(t("stats.hint")) + '</p>';
}

// showConnStats — countFn отдаёт текущее число участников сессии.
export function showConnStats(countFn) {
    const d = buildDialogOverlay(t("stats.title"), "", t("dialog.close"), null, null, { onClose: () => { open = null; } });
    open = { body: d.body, count: countFn || (() => 0) };
    paint();
    svc("GetVoiceBitrate").then((b) => { lastBitrate = b; paint(); }).catch(() => {});
    refreshConnMode().then(paint);
}

Events.On("vox:voice-bitrate", (e) => {
    if (e && e.data) lastBitrate = e.data;
    paint();
});
