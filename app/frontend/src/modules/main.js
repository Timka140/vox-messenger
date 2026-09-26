"use strict";

// Global error handlers for crash diagnostics.
// ВАЖНО: НЕ трогаем document.body — прежняя версия делала
// `document.body.textContent = info`, из-за чего ЛЮБАЯ необработанная ошибка
// или promise-rejection (например clipboard.writeText, когда окно не в фокусе)
// стирала весь интерфейс. Пользователь воспринимал это как «вылет».
// Логируем в консоль (core.js дублирует в flog → на сервер) и, если UI уже
// готов, показываем ненавязчивый toast.
function showErrorToast(text) {
    try {
        var el = document.getElementById("global-error-toast");
        if (!el) {
            el = document.createElement("div");
            el.id = "global-error-toast";
            el.style.cssText = "position:fixed;bottom:16px;left:50%;transform:translateX(-50%);" +
                "max-width:80vw;padding:10px 16px;background:#c0392b;color:#fff;border-radius:8px;" +
                "font-size:13px;z-index:99999;box-shadow:0 2px 12px rgba(0,0,0,.4);opacity:0;" +
                "transition:opacity .2s;pointer-events:none;white-space:pre-wrap;";
            document.body.appendChild(el);
        }
        el.textContent = text;
        el.style.opacity = "1";
        clearTimeout(el._hideTimer);
        el._hideTimer = setTimeout(function () { el.style.opacity = "0"; }, 6000);
    } catch (_) { /* до готовности DOM — только console */ }
}

window.onerror = function(msg, src, line, col) {
    console.error("JS ERROR: " + msg + "\nSource: " + src + ":" + line + ":" + col);
    showErrorToast("Ошибка: " + msg);
};
window.addEventListener("unhandledrejection", function(e) {
    var reason = (e.reason && (e.reason.stack || e.reason.message)) || e.reason;
    console.error("Unhandled rejection: " + reason);
    showErrorToast("Ошибка: " + reason);
});

import { svc } from "./core.js";
import { Window } from "/wails/runtime.js";
import { initLocale } from "./i18n.js";
import { init } from "./setup.js";
import { registerEvents } from "./events.js";
import { initVoiceChat } from "./voice.js";
import { initCalls, updateInfoTabs } from "./calls.js";
import { initScreenShare } from "./screenshare.js";
import { initStage } from "./stage.js";
import { initCallBar } from "./call-bar.js";
import { initContacts } from "./contacts.js";
import { initProfile } from "./profile.js";
import { Events } from "./core.js";
import { setRenderMessages, setShowRenameGroupDialog, setUpdateInfoTabs } from "./contacts-list.js";
import { renderMessages, showRenameGroupDialog } from "./messages.js";
import { initUpdates, bindUpdatesUI } from "./updates.js";
import { initMeshtasticPanel } from "./meshtastic.js";
import { initComposerAttach } from "./composer-attach.js";
import { initChatSearch } from "./chat-search.js";
import { initTyping } from "./typing.js";

// Wire cross-module callbacks (avoids circular deps)
setRenderMessages(renderMessages);
setShowRenameGroupDialog(showRenameGroupDialog);
setUpdateInfoTabs(updateInfoTabs);

// These modules self-register their event listeners on import
import "./contextmenu.js";
import "./chat.js";
import "./offcanvas.js";
import "./info-panel.js";
import "./chat-menu.js";
import "./settings.js";
import "./contacts.js";

// Register backend event handlers
registerEvents();
initVoiceChat();
initCalls();
initScreenShare(Events);
initStage(Events);
initCallBar();
initContacts();
initProfile();
bindUpdatesUI();
initComposerAttach();
initChatSearch();
initTyping();

// DevTools (F12)
document.addEventListener("keydown", function (e) {
    if (e.key === "F12") {
        e.preventDefault();
        Window.OpenDevTools();
    }
});

// Start — load locale then init
initLocale().then(function () {
    init();
    // Updates init — после init (нужно чтобы сервисы были доступны)
    initUpdates().catch(function () {});
    initMeshtasticPanel().catch(function () {});
});
