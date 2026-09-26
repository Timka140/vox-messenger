// Screen share module — отправка/приём демонстрации экрана.
//
// Sender side: кнопки из SHARE_BTN_IDS → диалог выбора экрана и режима
// (720p30/1080p30/1080p60) → StartScreenShare(displayIdx, mode) на бэке
// (Go ScreenShareService). Бэк шлёт нам же копию своего потока
// (peerId = state.myID) — превью «Вы».
//
// Receiver side: подписан на 'vox:screen-frame' — base64-кадр одного
// конкретного шарящего. Каждый пир → tile { canvas, decoder } (screen-decoder.js).
// Tile НЕ живёт в DOM сам по себе: это пул отвязанных canvas'ов. Кто и где
// их показывает, решает stage.js (сцена звонка) через getTile()/addSink():
// сцена монтирует tile.canvas крупно, лента участников подписывает маленький
// canvas-превью (sink), в который кадр копируется каждым N-м разом.
// Об изменениях пула модуль сообщает DOM-событиями vox:screen-tile-added /
// vox:screen-tile-removed {peerId}. Когда отправитель остановился — DC
// закрывается, кадры перестают идти, tile удаляется по таймауту.

import { svc, state, Events } from "./core.js";
import { t } from "./i18n.js";
import { showError } from "./toast.js";
import { buildDialogOverlay } from "./ui.js";
import { createScreenDecoder } from "./screen-decoder.js";

let isSharing = false;
const tiles = {}; // peerID → tile
const TILE_TIMEOUT_MS = 4000; // tile удаляется если кадров не было > 4s
const PLI_MIN_INTERVAL_MS = 1000; // не чаще одного PLI в секунду на пира
const SINK_EVERY_N_FRAMES = 6; // превью в ленте обновляем реже основного canvas
// Пресеты по смыслу (макет Dialogs); старые названия — синонимы (как в Go).
const MODES = ["text", "balanced", "motion"];
const DEFAULT_MODE = "balanced";
const LEGACY_MODES = { "720p30": "balanced", "1080p30": "balanced", "1080p60": "motion" };
const MODE_STORAGE_KEY = "vox.screen_mode";

// Кнопки start/stop: голосовой канал (user-bar), звонок (user-bar), сцена звонка.
const SHARE_BTN_IDS = ["user-bar-voice-screen", "user-bar-call-screen", "stage-share"];

export function isSelf(peerID) {
    return !!state.myID && peerID === state.myID;
}

export function peerLabel(peerID) {
    if (isSelf(peerID)) return t("screenshare.you");
    var known = (state.peers || []).find(function (p) { return p.id === peerID; });
    return (known && known.nickname) ? known.nickname : peerID.slice(0, 8) + "…";
}

function emitTileEvent(name, peerID) {
    document.dispatchEvent(new CustomEvent(name, { detail: { peerId: peerID } }));
}

// requestKeyframe — PLI отправителю (не чаще PLI_MIN_INTERVAL_MS). Для
// собственного превью бессмысленно: наш же энкодер и так шлёт нам всё.
function requestKeyframe(tile) {
    if (isSelf(tile.peerID)) return;
    const now = Date.now();
    if (now - tile.lastPliAt < PLI_MIN_INTERVAL_MS) return;
    tile.lastPliAt = now;
    svc("RequestScreenKeyframe", tile.peerID).catch(function (err) {
        console.warn("[SCREEN] PLI failed:", err && err.message || err);
    });
}

function copyToSinks(tile) {
    tile.frameCount++;
    if (tile.sinks.length === 0 || tile.frameCount % SINK_EVERY_N_FRAMES !== 0) return;
    for (const s of tile.sinks) {
        try {
            s.ctx.drawImage(tile.canvas, 0, 0, s.canvas.width, s.canvas.height);
        } catch (e) { /* canvas мог быть удалён */ }
    }
}

function ensureTile(peerID, width, height) {
    let tile = tiles[peerID];
    if (!tile) {
        const canvas = document.createElement("canvas");
        canvas.width = width;
        canvas.height = height;
        canvas.className = "screen-canvas";
        canvas.dataset.peer = peerID;
        tile = {
            peerID,
            canvas,
            lastFrameAt: Date.now(),
            lastFrameId: 0,         // последний обработанный frameId
            lastPliAt: 0,
            frameCount: 0,
            sinks: [],              // [{canvas, ctx}] — превью в ленте
            decoder: null,
        };
        tile.decoder = createScreenDecoder({
            canvas,
            onNeedKeyframe: () => requestKeyframe(tile),
            onFrame: () => copyToSinks(tile),
        });
        tiles[peerID] = tile;
        startTileGC();
        emitTileEvent("vox:screen-tile-added", peerID);
    } else if (tile.canvas.width !== width || tile.canvas.height !== height) {
        tile.canvas.width = width;
        tile.canvas.height = height;
    }
    return tile;
}

function removeAllTiles() {
    for (const pid in tiles) removeTile(pid);
}

function removeTile(peerID) {
    const tile = tiles[peerID];
    if (!tile) return;
    tile.decoder.close();
    tile.sinks = [];
    if (tile.canvas.parentNode) tile.canvas.parentNode.removeChild(tile.canvas);
    delete tiles[peerID];
    emitTileEvent("vox:screen-tile-removed", peerID);
}

// --- Публичный доступ к пулу для stage.js ---

export function getTile(peerID) {
    return tiles[peerID] || null;
}

// listSharers — peerID всех, чьи кадры сейчас приходят (включая себя).
export function listSharers() {
    return Object.keys(tiles);
}

// addSink — маленький canvas, в который копируется картинка пира (лента).
export function addSink(peerID, canvas) {
    const tile = tiles[peerID];
    if (!tile || !canvas) return;
    if (tile.sinks.some((s) => s.canvas === canvas)) return;
    const ctx = canvas.getContext("2d");
    tile.sinks.push({ canvas, ctx });
    try { ctx.drawImage(tile.canvas, 0, 0, canvas.width, canvas.height); } catch (e) { /* пусто */ }
}

export function removeSink(peerID, canvas) {
    const tile = tiles[peerID];
    if (!tile) return;
    tile.sinks = tile.sinks.filter((s) => s.canvas !== canvas);
}

// setDecodeSuspended — не декодировать поток пира (никто не смотрит). Кадры
// продолжают обновлять lastFrameAt, чтобы GC не убрал tile; при возобновлении
// декодер запросит keyframe.
export function setDecodeSuspended(peerID, flag) {
    const tile = tiles[peerID];
    if (tile) tile.decoder.suspend(flag);
}

export function requestKeyframeFor(peerID) {
    const tile = tiles[peerID];
    if (tile) requestKeyframe(tile);
}

// Сборщик мусора: раз в 2с проверяем timeout у tiles. Таймер живёт только
// пока есть хотя бы один tile (правило проекта — без вечного polling'а).
let gcTimer = null;
function startTileGC() {
    if (gcTimer) return;
    gcTimer = setInterval(() => {
        const now = Date.now();
        for (const pid in tiles) {
            if (now - tiles[pid].lastFrameAt > TILE_TIMEOUT_MS) {
                removeTile(pid);
            }
        }
        if (Object.keys(tiles).length === 0) {
            clearInterval(gcTimer);
            gcTimer = null;
        }
    }, 2000);
}

function onScreenFrame(payload) {
    if (!payload || !payload.peerId) return;
    const peerID = payload.peerId;
    const w = payload.width || 1280;
    const h = payload.height || 720;
    const fid = payload.frameId || 0;
    const tile = ensureTile(peerID, w, h);
    tile.lastFrameAt = Date.now(); // и при suspend: поток жив, tile не трогаем
    if (fid !== 0 && fid <= tile.lastFrameId) return; // out-of-order: устаревший
    if (tile.decoder.push(payload, fid, w, h)) tile.lastFrameId = fid;
}

// --- Старт: диалог выбора экрана и режима ---

function savedMode() {
    try {
        let m = localStorage.getItem(MODE_STORAGE_KEY);
        if (m && LEGACY_MODES[m]) m = LEGACY_MODES[m];
        if (m && MODES.indexOf(m) >= 0) return m;
    } catch (e) { /* storage недоступен */ }
    return DEFAULT_MODE;
}

function rememberMode(mode) {
    try { localStorage.setItem(MODE_STORAGE_KEY, mode); } catch (e) { /* ignore */ }
}

// --- Звук системы при демонстрации (фаза 5e) ---

const AUDIO_STORAGE_KEY = "vox.screen_audio";
const audioSharers = new Set();   // кто сейчас показывает со звуком (приём)
const shareVolumes = {};          // peerId → громкость демонстрации для меня, %

function savedAudio() {
    try { return localStorage.getItem(AUDIO_STORAGE_KEY) === "1"; } catch (e) { return false; }
}

function rememberAudio(on) {
    try { localStorage.setItem(AUDIO_STORAGE_KEY, on ? "1" : "0"); } catch (e) { /* ignore */ }
}

export function hasShareAudio(peerID) {
    return audioSharers.has(peerID);
}

export function getShareVolume(peerID) {
    return shareVolumes[peerID] === undefined ? 100 : shareVolumes[peerID];
}

export function setShareVolume(peerID, percent) {
    shareVolumes[peerID] = percent;
    svc("SetScreenShareVolume", peerID, percent / 100).catch(() => {});
}

// buildDisplayPicker — плитки экранов с живыми превью (Go шлёт
// vox:screen-thumbs, пока идёт StartScreenThumbs). Общая для «Показать экран»
// и «Снимок экрана» в скрепке (фаза 7e). Клик выбирает экран, двойной —
// onPick(index). → { el, selected(), stopThumbs() }; stopThumbs идемпотентна.
export function buildDisplayPicker(displays, onPick) {
    let selected = displays[0].index;
    const grid = document.createElement("div");
    grid.className = "sp-grid";
    const imgs = {};
    displays.forEach((d) => {
        const tile = document.createElement("button");
        tile.type = "button";
        tile.className = "sp-tile" + (d.index === selected ? " selected" : "");
        const thumb = document.createElement("div");
        thumb.className = "sp-thumb";
        thumb.innerHTML = '<i data-lucide="monitor" class="icon-20"></i>';
        const img = document.createElement("img");
        img.alt = "";
        img.hidden = true;
        thumb.appendChild(img);
        const name = document.createElement("span");
        name.className = "sp-name";
        const base = t("screenshare.display_n").replace("{n}", String(d.index + 1));
        name.textContent = d.width ? base + " · " + d.width + "×" + d.height : base;
        tile.appendChild(thumb);
        tile.appendChild(name);
        tile.addEventListener("click", () => {
            selected = d.index;
            grid.querySelectorAll(".sp-tile").forEach((x) => x.classList.toggle("selected", x === tile));
        });
        tile.addEventListener("dblclick", () => { selected = d.index; if (onPick) onPick(d.index); });
        imgs[d.index] = img;
        grid.appendChild(tile);
    });

    let stopped = false;
    const offThumbs = Events.On("vox:screen-thumbs", (ev) => {
        const items = (ev.data && ev.data.items) || [];
        items.forEach((it) => {
            const img = imgs[it.index];
            if (!img || !it.jpeg) return;
            img.src = "data:image/jpeg;base64," + it.jpeg;
            img.hidden = false;
            const ph = img.parentNode && img.parentNode.querySelector("svg, i[data-lucide]");
            if (ph) ph.remove();
        });
    });
    svc("StartScreenThumbs").catch((err) => console.warn("[SCREEN] thumbs:", err && err.message || err));

    return {
        el: grid,
        selected: () => selected,
        stopThumbs() {
            if (stopped) return Promise.resolve();
            stopped = true;
            if (typeof offThumbs === "function") offThumbs();
            return svc("StopScreenThumbs").catch(() => {});
        },
    };
}

// showStartDialog — «Показать экран» (макет Dialogs): плитки экранов с живым
// превью (buildDisplayPicker), качество сегментом с подписью «N к/с · M Мбит/с».
// Клик выбирает экран, двойной — сразу старт.
async function showStartDialog() {
    let displays = [];
    try {
        displays = (await svc("ListDisplays")) || [];
    } catch (err) {
        console.warn("[SCREEN] ListDisplays:", err && err.message || err);
    }
    if (displays.length === 0) displays = [{ index: 0, name: "Display 1", width: 0, height: 0 }];
    let mode = savedMode();

    const body = document.createElement("div");
    body.className = "sp";
    const picker = buildDisplayPicker(displays, () => start());
    body.appendChild(picker.el);

    const quality = document.createElement("div");
    quality.className = "sp-quality";
    const qLabel = document.createElement("span");
    qLabel.className = "sp-label";
    qLabel.textContent = t("screenshare.mode");
    const seg = document.createElement("div");
    seg.className = "sp-seg";
    const desc = document.createElement("span");
    desc.className = "sp-desc";
    MODES.forEach((m) => {
        const b = document.createElement("button");
        b.type = "button";
        b.textContent = t("screenshare.mode_" + m);
        b.classList.toggle("on", m === mode);
        b.addEventListener("click", () => {
            mode = m;
            seg.querySelectorAll("button").forEach((x) => x.classList.toggle("on", x === b));
            desc.textContent = t("screenshare.mode_desc_" + m);
        });
        seg.appendChild(b);
    });
    desc.textContent = t("screenshare.mode_desc_" + mode);
    quality.appendChild(qLabel);
    quality.appendChild(seg);
    quality.appendChild(desc);
    body.appendChild(quality);

    // «Со звуком системы» — выключен, если звук Vox нельзя исключить из захвата.
    let withAudio = savedAudio();
    const audioRow = document.createElement("div");
    audioRow.className = "sp-audio";
    const audioText = document.createElement("span");
    audioText.className = "sp-audio-text";
    audioText.textContent = t("screenshare.with_audio");
    const audioHint = document.createElement("span");
    audioHint.className = "sp-desc";
    const audioToggle = document.createElement("span");
    audioToggle.className = "ip-toggle";
    audioRow.appendChild(audioText);
    audioRow.appendChild(audioHint);
    audioRow.appendChild(audioToggle);
    body.appendChild(audioRow);
    const paintAudio = () => audioToggle.classList.toggle("on", withAudio);
    paintAudio();
    audioRow.addEventListener("click", () => {
        if (audioRow.classList.contains("disabled")) return;
        withAudio = !withAudio;
        paintAudio();
    });
    svc("ScreenAudioSupported").then((ok) => {
        if (ok) return;
        withAudio = false;
        audioRow.classList.add("disabled");
        audioHint.textContent = t("screenshare.audio_needs_win11");
        paintAudio();
    }).catch(() => {});

    const dlg = buildDialogOverlay(t("screenshare.dialog_title"), "", t("dialog.cancel"), t("screenshare.start"), null,
        { wide: true, onClose: picker.stopThumbs });
    dlg.body.appendChild(body);
    if (window.lucide) lucide.createIcons({ nodes: [body] });
    dlg.confirmBtn.addEventListener("click", start);

    async function start() {
        dlg.overlay.remove();
        // Сначала дождаться закрытия захвата превью — иначе DXGI того же
        // экрана откроется дважды.
        await picker.stopThumbs();
        rememberMode(mode);
        rememberAudio(withAudio);
        try {
            await svc("StartScreenShare", picker.selected(), mode, withAudio);
            isSharing = true;
            updateButtonUI();
        } catch (err) {
            console.error("start screen share:", err);
            showError(String(err.message || err), { title: t("screenshare.start_failed") });
        }
    }
}

export async function toggleScreenShare() {
    if (isSharing) {
        try {
            await svc("StopScreenShare");
            isSharing = false;
            updateButtonUI();
            if (state.myID) removeTile(state.myID);
        } catch (err) {
            console.error("stop screen share:", err);
        }
        return;
    }
    await showStartDialog();
}

function updateButtonUI() {
    const title = isSharing ? t("call.stop_share") : t("call.share_screen");
    const wantIcon = isSharing ? "monitor-off" : "monitor-up";
    SHARE_BTN_IDS.forEach(function (id) {
        const btn = document.getElementById(id);
        if (!btn) return;
        btn.classList.toggle("active", isSharing);
        btn.title = title;
        const label = btn.querySelector("span");
        if (label) label.textContent = title;
        // Lucide заменяет <i data-lucide> на <svg class="lucide lucide-<name> ...">;
        // для смены иконки возвращаем <i> и просим lucide перерисовать (как в calls.js).
        const cur = btn.querySelector("svg.lucide, i[data-lucide]");
        if (cur && !cur.classList.contains("lucide-" + wantIcon)) {
            const i = document.createElement("i");
            i.setAttribute("data-lucide", wantIcon);
            i.className = cur.classList.contains("icon-20") ? "icon-20" : "icon-16";
            cur.replaceWith(i);
            if (window.lucide) lucide.createIcons({ nodes: [btn] });
        }
    });
}

export function initScreenShare(events) {
    SHARE_BTN_IDS.forEach(function (id) {
        const btn = document.getElementById(id);
        if (btn) {
            btn.addEventListener("click", function (e) {
                e.stopPropagation(); // user-bar-call сам открывает панель по клику
                toggleScreenShare();
            });
        }
    });

    if (events && events.On) {
        events.On("vox:screen-frame", (e) => onScreenFrame(e && e.data));
        // Звонок закончился: бэкенд сам демонстрацию не останавливает (captureLoop
        // продолжает слать в группу звонка) — гасим здесь, тайлы убираем сразу.
        events.On("vox:call-ended", () => {
            removeAllTiles();
            stopScreenShareIfActive();
        });
        // vox:voice-leave — ПИР вышел из голосового канала (per-peer): его
        // тайл убираем сразу, не дожидаясь GC. Наш собственный выход из
        // канала обрабатывает stopScreenShareIfActive() из calls.js.
        events.On("vox:voice-leave", (e) => {
            const pid = e && e.data && e.data.peerId;
            if (pid && !isSelf(pid)) removeTile(pid);
        });
        // Отправитель нажал «Стоп» (control-фрейм по screen DC) — без этого
        // тайл жил до TILE_TIMEOUT_MS.
        events.On("vox:screen-share-ended", (e) => {
            const pid = e && e.data && e.data.peerId;
            if (pid) removeTile(pid);
        });
        // Звук чужой демонстрации начал/перестал приходить — значок на плитке
        // и ползунок громкости в меню участника (stage.js).
        events.On("vox:screen-audio", (e) => {
            const d = (e && e.data) || {};
            if (!d.peerId) return;
            if (d.active) audioSharers.add(d.peerId); else audioSharers.delete(d.peerId);
            document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
        });
        events.On("vox:screen-share-started", () => {
            isSharing = true;
            updateButtonUI();
        });
        events.On("vox:screen-share-stopped", () => {
            isSharing = false;
            updateButtonUI();
            if (state.myID) removeTile(state.myID);
        });
        events.On("vox:screen-share-error", (e) => {
            isSharing = false;
            updateButtonUI();
            const msg = (e && e.data && e.data.error) || "screen share error";
            console.error(msg);
            showError(String(msg), { title: t("screenshare.start_failed") });
        });
    }
}

export function isScreenSharing() {
    return isSharing;
}

// stopScreenShareIfActive — гасим свою демонстрацию, когда мы выходим из
// звонка/голосового канала: бэкенд сам её не останавливает (captureLoop
// продолжит слать в группу), а слать больше некому. Вызывается из calls.js
// при LeaveGroupVoice и из обработчика vox:call-ended.
export function stopScreenShareIfActive() {
    if (state.myID) removeTile(state.myID);
    if (!isSharing) return;
    svc("StopScreenShare").catch((err) => console.error("stop screen share:", err));
    isSharing = false;
    updateButtonUI();
}
