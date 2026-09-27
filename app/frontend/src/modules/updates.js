"use strict";

// updates.js — клиент автообновления.
// Показывает баннер "Доступно обновление" когда auth_ok приносит новую версию,
// скачивает через UpdateService, применяет и перезапускает.
// Force режим — модалка блокирует UI.

import { svc, flog, Events } from "./core.js";
import { t } from "./i18n.js";

export var updateState = {
    current: "dev",
    latest: null,         // {version, force, changelog}
    downloading: false,
    progress: 0,
    canRollback: false,
    lastError: null,
};

var bannerEl = null;
var forceModalEl = null;

function isNewer(latest) {
    if (!latest) return false;
    if (updateState.current === "dev") return true; // dev builds — любой release "новее"
    return compareVersions(latest, updateState.current) > 0;
}

// offered — предлагать ли обновление: версия новее или текущая отозвана
// (откат из админки ставит более старую latest, revoked приходит из Go).
function offered(info) {
    return !!info && (info.revoked === true || isNewer(info.version));
}

// Простое сравнение версий формата vMAJ.MIN.PAT.BLD
function compareVersions(a, b) {
    var re = /^v(\d+)\.(\d+)\.(\d+)\.(\d+)$/;
    var ma = a && a.match(re);
    var mb = b && b.match(re);
    if (!ma || !mb) return 0;
    for (var i = 1; i <= 4; i++) {
        var na = parseInt(ma[i], 10);
        var nb = parseInt(mb[i], 10);
        if (na !== nb) return na < nb ? -1 : 1;
    }
    return 0;
}

var AUTO_CHECK_KEY = "updates.autoCheck";

export function isAutoCheckEnabled() {
    var v = localStorage.getItem(AUTO_CHECK_KEY);
    return v === null ? true : v === "1";
}

export function setAutoCheckEnabled(enabled) {
    localStorage.setItem(AUTO_CHECK_KEY, enabled ? "1" : "0");
}

// Время последней успешной проверки — «Проверено N минут назад» в
// «О программе» (фаза 7d).
var LAST_CHECK_KEY = "updates.lastCheck";

export function lastUpdateCheck() {
    try { return parseInt(localStorage.getItem(LAST_CHECK_KEY), 10) || 0; } catch (e) { return 0; }
}

// pendingUpdateVersion — более новая известная версия (баннер), иначе "".
export function pendingUpdateVersion() {
    return offered(updateState.latest) ? updateState.latest.version : "";
}

export async function initUpdates() {
    // ВАЖНО: регистрируем обработчик событий ПЕРВЫМ — auth_ok от сигналки может
    // прилететь раньше чем resolveнутся await'ы ниже, и мы потеряем событие.
    Events.On("vox:update-available", function (event) {
        var info = event.data;
        if (!info) return;
        updateState.latest = info;
        // Force-апдейт всегда показываем, остальное — только при включённой авто-проверке
        if (!offered(info)) return;
        if (info.force || isAutoCheckEnabled()) {
            showBanner(info);
        }
    });

    try {
        updateState.current = await svc("GetCurrentVersion");
        updateState.canRollback = await svc("CanRollback");
    } catch (err) {
        flog("ERR", "updates", "init failed: " + err);
    }

    // При включённой авто-проверке делаем свежий HTTP-запрос за манифестом.
    // Это работает одинаково в dev и prod (в отличие от GetLatestRelease +
    // push через auth_ok, которые для dev-сборки бэкендом отключены).
    // ВАЖНО: CheckForUpdates требует активного соединения с сигналкой,
    // поэтому ждём первого vox:signal-updated с онлайн-сервером.
    if (isAutoCheckEnabled()) {
        var startupCheckDone = false;
        var tryStartupCheck = function () {
            if (startupCheckDone) return;
            svc("GetSignalServers").then(function (servers) {
                if (startupCheckDone) return;
                var online = Array.isArray(servers) && servers.some(function (s) { return s.online; });
                if (!online) return;
                startupCheckDone = true;
                checkForUpdates().catch(function (err) {
                    flog("ERR", "updates", "startup check failed: " + err);
                });
            }).catch(function () {});
        };
        Events.On("vox:signal-updated", tryStartupCheck);
        // На случай если сигналка уже успела подняться до initUpdates
        tryStartupCheck();
    }

    Events.On("vox:update-progress", function (event) {
        var p = event.data;
        if (!p) return;
        updateState.progress = p.percent || 0;
        // Обновляем оба прогресс-бара: банер в settings + force modal.
        // Видимый — тот блок что сейчас на экране, скрытые остаются скрытыми.
        var ids = ["update-progress-bar", "update-force-progress-bar"];
        for (var i = 0; i < ids.length; i++) {
            var el = document.getElementById(ids[i]);
            if (el) el.style.width = updateState.progress + "%";
        }
        var textIds = ["update-progress-text", "update-force-progress-text"];
        for (var j = 0; j < textIds.length; j++) {
            var txt = document.getElementById(textIds[j]);
            if (txt) txt.textContent = updateState.progress + "%";
        }
    });
}

function showBanner(info) {
    if (info.force) {
        showForceModal(info);
        return;
    }
    if (!bannerEl) {
        bannerEl = document.getElementById("update-banner");
    }
    if (!bannerEl) return;

    var versionEl = document.getElementById("update-banner-version");
    var changelogEl = document.getElementById("update-banner-changelog");
    if (versionEl) versionEl.textContent = info.version;
    if (changelogEl) changelogEl.textContent = info.changelog || "";
    bannerEl.style.display = "flex";
}

function hideBanner() {
    if (!bannerEl) bannerEl = document.getElementById("update-banner");
    if (bannerEl) bannerEl.style.display = "none";
}

function showForceModal(info) {
    if (!forceModalEl) {
        forceModalEl = document.getElementById("update-force-modal");
    }
    if (!forceModalEl) return;
    var versionEl = document.getElementById("update-force-version");
    var changelogEl = document.getElementById("update-force-changelog");
    if (versionEl) versionEl.textContent = info.version;
    if (changelogEl) changelogEl.textContent = info.changelog || "";
    forceModalEl.style.display = "flex";
}

export async function downloadAndApply(version) {
    // Guard против двойного клика / автотриггера + ручного клика —
    // на бэке тоже стоит TryLock, но здесь не показываем ошибку юзеру.
    if (updateState.downloading) {
        flog("INF", "updates", "downloadAndApply skipped: already downloading");
        return;
    }
    try {
        updateState.downloading = true;
        updateState.progress = 0;

        // Показываем оба прогресс-блока (банер + force modal). Тот что в скрытом
        // блоке всё равно невидим, but активный — отрабатывает.
        var bannerProgress = document.getElementById("update-progress-container");
        if (bannerProgress) bannerProgress.style.display = "block";
        var forceProgress = document.getElementById("update-force-progress-container");
        if (forceProgress) forceProgress.style.display = "block";

        // Disable кнопки чтобы юзер не тыкал повторно во время загрузки.
        var bannerBtn = document.getElementById("update-banner-install");
        if (bannerBtn) bannerBtn.disabled = true;
        var forceBtn = document.getElementById("update-force-install");
        if (forceBtn) forceBtn.disabled = true;

        flog("INF", "updates", "downloading " + version);
        var res = await svc("DownloadUpdate", version);
        if (!res || !res.ok) {
            throw new Error((res && res.message) || "download failed");
        }
        flog("INF", "updates", "applying update");
        await svc("ApplyUpdate");
        // После ApplyUpdate backend сам завершит приложение (UpdateService.SetQuit)
    } catch (err) {
        updateState.lastError = String(err);
        flog("ERR", "updates", "download/apply failed: " + err);
        alert(t("update.error") + ": " + err);
        updateState.downloading = false;
        var bannerProgress = document.getElementById("update-progress-container");
        if (bannerProgress) bannerProgress.style.display = "none";
        var forceProgress = document.getElementById("update-force-progress-container");
        if (forceProgress) forceProgress.style.display = "none";
        var bannerBtn = document.getElementById("update-banner-install");
        if (bannerBtn) bannerBtn.disabled = false;
        var forceBtn = document.getElementById("update-force-install");
        if (forceBtn) forceBtn.disabled = false;
    }
}

export async function rollback() {
    try {
        flog("INF", "updates", "rolling back");
        await svc("Rollback");
    } catch (err) {
        flog("ERR", "updates", "rollback failed: " + err);
        alert(t("update.error") + ": " + err);
    }
}

export async function checkForUpdates() {
    try {
        var res = await svc("CheckForUpdates");
        try { localStorage.setItem(LAST_CHECK_KEY, String(Date.now())); } catch (e) {}
        if (res && res.available && res.supported_for_platform) {
            // available решил Go по подписанному манифесту, revoked — текущая отозвана (откат).
            updateState.latest = { version: res.latest, force: res.force, changelog: res.changelog, revoked: !!res.revoked };
            showBanner(updateState.latest);
        }
        return res;
    } catch (err) {
        flog("ERR", "updates", "check failed: " + err);
        return { error: String(err) };
    }
}

// Bind DOM кнопок баннера и force modal после загрузки
export function bindUpdatesUI() {
    var installBtn = document.getElementById("update-banner-install");
    var laterBtn = document.getElementById("update-banner-later");
    if (installBtn) {
        installBtn.addEventListener("click", function () {
            if (updateState.latest) downloadAndApply(updateState.latest.version);
        });
    }
    if (laterBtn) {
        laterBtn.addEventListener("click", hideBanner);
    }
    var forceInstallBtn = document.getElementById("update-force-install");
    if (forceInstallBtn) {
        forceInstallBtn.addEventListener("click", function () {
            if (updateState.latest) downloadAndApply(updateState.latest.version);
        });
    }
}
