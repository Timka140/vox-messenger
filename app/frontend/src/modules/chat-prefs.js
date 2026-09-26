"use strict";

// chat-prefs.js — «Закрепить чат» и «Цвет чата» (фаза 7a, макеты HeaderMenus,
// ContextMenus, InfoDM). Хранение как у mute.js: настройка pinned_chats —
// JSON-массив ключей "p:<id>" / "g:<id>" в порядке закрепления; chat_colors —
// JSON {"p:<id>": "#hex"}.
//
// Цвет чата — цвет исходящих пузырей в этом личном чате (заметка макета к
// «Цвет исходящих»: «Можно задать отдельно для чата»). Ставится CSS-
// переменными --bubble-out / --bubble-out-text на #messages; у групп и без
// выбора переопределения нет — работает тема.

import { svc, state, dom, flog } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { peerKey } from "./mute.js";

// Палитра — из «Цвет исходящих» (макет SettingsAppearance).
export const CHAT_COLORS = [
    { hex: "#A3DAC5", name: "color.mint" },
    { hex: "#7fb3e6", name: "color.blue" },
    { hex: "#c99be0", name: "color.lilac" },
    { hex: "#e6a97f", name: "color.peach" },
    { hex: "#8fd39a", name: "color.green" },
];
const TEXT_ON_COLOR = "#1f2a2e"; // все цвета палитры пастельные — текст тёмный

let pinned = [];
let colors = {};
// Общий «Цвет исходящих» (Настройки → Внешний вид, фаза 7c); "" — цвет темы.
// Ставится на <html>, цвет чата — на #messages, поэтому цвет чата главнее.
let bubbleColor = "";

function changed() {
    applyBubbleColor();
    applyChatColor();
    document.dispatchEvent(new CustomEvent("vox:ui-chat-prefs-changed"));
}

// loadChatPrefs — после входа (setup.js showChat), рядом с loadMuted.
export function loadChatPrefs() {
    return Promise.all([
        svc("GetSetting", "pinned_chats").catch(() => ""),
        svc("GetSetting", "chat_colors").catch(() => ""),
        svc("GetSetting", "bubble_color").catch(() => ""),
    ]).then(([p, c, b]) => {
        try { const a = JSON.parse(p || "[]"); pinned = Array.isArray(a) ? a : []; } catch (e) { pinned = []; }
        try { const o = JSON.parse(c || "{}"); colors = o && typeof o === "object" && !Array.isArray(o) ? o : {}; } catch (e) { colors = {}; }
        bubbleColor = /^#[0-9a-f]{6}$/i.test(b || "") ? b : "";
        changed();
    });
}

export function isPinned(key) {
    return !!key && pinned.indexOf(key) >= 0;
}

// pinRank — место в закреплённых (−1 — не закреплён); для сортировки списка.
export function pinRank(key) {
    return key ? pinned.indexOf(key) : -1;
}

export function togglePinned(key) {
    if (!key) return;
    const i = pinned.indexOf(key);
    if (i >= 0) pinned.splice(i, 1); else pinned.push(key);
    svc("SaveSetting", "pinned_chats", JSON.stringify(pinned)).catch((err) => flog("ERR", "frontend.chat-prefs", "save pinned: " + err));
    changed();
}

export function getChatColor(key) {
    return (key && colors[key]) || "";
}

export function chatColorName(key) {
    const hex = getChatColor(key).toLowerCase();
    const c = CHAT_COLORS.find((x) => x.hex.toLowerCase() === hex);
    return c ? t(c.name) : t("color.default");
}

export function setChatColor(key, hex) {
    if (!key) return;
    if (hex) colors[key] = hex; else delete colors[key];
    svc("SaveSetting", "chat_colors", JSON.stringify(colors)).catch((err) => flog("ERR", "frontend.chat-prefs", "save colors: " + err));
    changed();
}

// applyChatColor — цвет исходящих открытого личного чата.
export function applyChatColor() {
    const el = dom.messagesDiv;
    if (!el) return;
    const hex = !state.selectedGroupID && state.selectedPeerID ? getChatColor(peerKey(state.selectedPeerID)) : "";
    if (hex) {
        el.style.setProperty("--bubble-out", hex);
        el.style.setProperty("--bubble-out-text", TEXT_ON_COLOR);
    } else {
        el.style.removeProperty("--bubble-out");
        el.style.removeProperty("--bubble-out-text");
    }
}

export function getBubbleColor() {
    return bubbleColor;
}

export function setBubbleColor(hex) {
    bubbleColor = hex || "";
    svc("SaveSetting", "bubble_color", bubbleColor).catch((err) => flog("ERR", "frontend.chat-prefs", "save bubble color: " + err));
    changed();
}

// applyBubbleColor — общий цвет исходящих на <html>: перекрывает тему,
// а цвет чата (applyChatColor на #messages) перекрывает его.
function applyBubbleColor() {
    const el = document.documentElement;
    if (bubbleColor) {
        el.style.setProperty("--bubble-out", bubbleColor);
        el.style.setProperty("--bubble-out-text", TEXT_ON_COLOR);
    } else {
        el.style.removeProperty("--bubble-out");
        el.style.removeProperty("--bubble-out-text");
    }
}

// showChatColorMenu — выбор цвета вторым меню. Без события — там же, где было
// предыдущее меню («Цвет чата» из «…»). На следующем тике: клик по пункту
// первого меню ещё всплывает до document, а тот закрывает любое меню.
export function showChatColorMenu(e, key) {
    const menu = document.getElementById("context-menu");
    const x = e && e.clientX != null ? e.clientX : (parseInt(menu && menu.style.left, 10) || 200);
    const y = e && e.clientY != null ? e.clientY : (parseInt(menu && menu.style.top, 10) || 200);
    const cur = getChatColor(key).toLowerCase();
    const items = [
        { label: t("color.default"), icon: "rotate-ccw", sub: cur ? "" : "✓", action: () => setChatColor(key, null) },
        { separator: true },
    ];
    CHAT_COLORS.forEach((c) => items.push({
        label: t(c.name),
        swatch: c.hex,
        sub: cur === c.hex.toLowerCase() ? "✓" : "",
        action: () => setChatColor(key, c.hex),
    }));
    setTimeout(() => showContextMenu({ clientX: x, clientY: y, preventDefault() {}, stopPropagation() {} }, items), 0);
}

// Выбор/смена чата — selectPeer/selectGroup рассылают vox:ui-refresh-info-panel.
document.addEventListener("vox:ui-refresh-info-panel", applyChatColor);
document.addEventListener("vox:ui-chat-changed", applyChatColor);
