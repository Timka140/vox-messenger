"use strict";

// user-menu.js — меню своего аватара в нижней панели (макет ContextMenus
// «Аватар в нижней панели»): шапка с ником и тегом, «Не беспокоить»,
// «Копировать ссылку», «Копировать тег», «Настройки» (Ctrl+,).
//
// «Не беспокоить» = режим уведомлений «выключены» (notify_mode off, тот же,
// что в настройках): карточек нет, звонки, бейджи и звук остаются. Прежний
// режим запоминается в notify_mode_before_dnd и возвращается при выключении.
// #user-avatar переезжает в рейку (narrow.js) — слушатель по узлу живёт.

import { svc, state } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { showSettings } from "./settings.js";
import { applyAvatar } from "./avatar.js";
import { showSuccess } from "./toast.js";

const DND_PREV_KEY = "notify_mode_before_dnd";

function copy(text) {
    try { navigator.clipboard.writeText(text); showSuccess(t("info.copied")); } catch (e) { /* окно без фокуса */ }
}

async function getMode() {
    try { return (await svc("GetSetting", "notify_mode")) || "custom"; } catch (e) { return "custom"; }
}

async function setDnd(on) {
    if (on) {
        const cur = await getMode();
        if (cur !== "off") await svc("SaveSetting", DND_PREV_KEY, cur).catch(() => {});
        await svc("SaveSetting", "notify_mode", "off").catch(() => {});
    } else {
        let prev = "";
        try { prev = await svc("GetSetting", DND_PREV_KEY); } catch (e) { prev = ""; }
        await svc("SaveSetting", "notify_mode", prev && prev !== "off" ? prev : "custom").catch(() => {});
    }
}

function header(nick, tag) {
    const head = document.createElement("div");
    head.className = "context-menu-header";
    const av = document.createElement("div");
    av.className = "context-menu-header-av";
    applyAvatar(av, state.myID, nick, { letters: 1 });
    const txt = document.createElement("div");
    const n = document.createElement("div");
    n.className = "context-menu-header-name";
    n.textContent = nick;
    const tg = document.createElement("div");
    tg.className = "context-menu-header-tag";
    tg.textContent = tag;
    txt.append(n, tg);
    head.append(av, txt);
    return head;
}

async function showUserMenu(anchor) {
    const dnd = (await getMode()) === "off";
    const nick = state.myNickname || "user";
    const tag = nick + "#" + (state.myShortID || "");
    const r = anchor.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.top, preventDefault() {}, stopPropagation() {} }, [
        { header: { node: header(nick, tag) } },
        { separator: true },
        { label: t("user_menu.dnd"), icon: "bell-off", toggle: dnd, action: () => setDnd(!dnd) },
        { separator: true },
        { label: t("user_menu.copy_link"), icon: "share-2", action: () => svc("GetMyContactURI").then(copy).catch(() => {}) },
        { label: t("ctx.copy_tag"), icon: "copy", action: () => copy(tag) },
        { separator: true },
        { label: t("user_menu.settings"), icon: "settings", hint: "Ctrl+,", action: () => showSettings() },
    ]);
    // Меню над аватаром: панель внизу окна.
    const menu = document.getElementById("context-menu");
    if (menu) menu.style.top = Math.max(4, r.top - menu.offsetHeight - 4) + "px";
}

const avatarEl = document.getElementById("user-avatar");
if (avatarEl) {
    avatarEl.style.cursor = "pointer";
    avatarEl.addEventListener("click", (e) => {
        e.stopPropagation(); // иначе document-click в contextmenu.js сразу закроет меню
        showUserMenu(avatarEl);
    });
}

// Ctrl+, — настройки (подсказка в меню).
document.addEventListener("keydown", (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === ",") {
        e.preventDefault();
        showSettings();
    }
});
