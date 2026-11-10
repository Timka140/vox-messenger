"use strict";

// hotkeys.js — горячие клавиши окна (ревью интерфейса 2026-11):
//   Ctrl+K                     — поиск по списку чатов (в рейке — поповер поиска)
//   Enter в поиске             — открыть первый найденный чат
//   Esc в поиске               — очистить поиск, повторно — убрать фокус
//   Ctrl+PgUp / Ctrl+PgDn      — предыдущий / следующий чат открытого списка
//   Alt+↑ / Alt+↓              — то же
//   Esc                        — закрыть Настройки (если поверх нет диалога)
// Остальные живут по месту: Ctrl+F — поиск по чату (chat-search.js),
// Ctrl+, — настройки (user-menu.js), Ctrl+P — вложение (composer-attach.js),
// ↑ в пустом поле — править последнее сообщение (chat.js).

import { dom } from "./core.js";
import { closeSettings } from "./settings.js";

function dialogOpen() {
    return !!document.querySelector(".backup-overlay, .lightbox-overlay.is-open");
}

function settingsOpen() {
    return !!(dom.settingsScreen && dom.settingsScreen.style.display !== "none");
}

// visibleRows — строки списка слева, не скрытые фильтром поиска.
function visibleRows() {
    return Array.prototype.filter.call(dom.contactsList.querySelectorAll(".contact-item"), function (el) {
        return el.style.display !== "none" && el.offsetParent !== null;
    });
}

function focusSearch() {
    var input = document.getElementById("search-input");
    if (!input) return;
    var panel = document.getElementById("contacts-panel");
    if (document.body.classList.contains("rail") && panel && !panel.classList.contains("search-open")) {
        var btn = document.getElementById("rail-search-btn");
        if (btn) { btn.click(); return; } // narrow.js сам ставит фокус
    }
    input.focus();
    input.select();
}

// stepChat — соседний чат в открытом списке; без активного — первый/последний.
function stepChat(dir) {
    var rows = visibleRows();
    if (!rows.length) return;
    var idx = -1;
    for (var i = 0; i < rows.length; i++) {
        if (rows[i].classList.contains("active")) { idx = i; break; }
    }
    if (idx < 0) idx = dir > 0 ? -1 : rows.length;
    var next = rows[(idx + dir + rows.length) % rows.length];
    next.click();
    next.scrollIntoView({ block: "nearest" });
}

document.addEventListener("keydown", function (e) {
    var ctrl = e.ctrlKey || e.metaKey;
    // По коду клавиши, не символу: в русской раскладке e.key — «л».
    if (ctrl && !e.shiftKey && !e.altKey && e.code === "KeyK") {
        if (settingsOpen() || dialogOpen()) return;
        e.preventDefault();
        focusSearch();
        return;
    }
    if (settingsOpen()) {
        // defaultPrevented — Esc уже потратили на контекстное меню (contextmenu.js)
        if (e.key === "Escape" && !dialogOpen() && !e.defaultPrevented) {
            e.preventDefault();
            closeSettings();
        }
        return;
    }
    if (dialogOpen()) return;
    var pg = ctrl && !e.altKey && !e.shiftKey && (e.key === "PageUp" || e.key === "PageDown");
    var alt = e.altKey && !ctrl && !e.shiftKey && (e.key === "ArrowUp" || e.key === "ArrowDown");
    if (pg || alt) {
        e.preventDefault();
        stepChat(e.key === "PageUp" || e.key === "ArrowUp" ? -1 : 1);
    }
});

var search = document.getElementById("search-input");
if (search) {
    search.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
            // Пустой запрос и ссылка-приглашение (карточка «Вступить») списка
            // не фильтруют — «первый найденный» там просто верхний чат.
            if (e.isComposing || !search.value.trim() || dom.contactsList.querySelector(".join-card")) return;
            var first = visibleRows()[0];
            if (first) { e.preventDefault(); first.click(); }
        } else if (e.key === "Escape") {
            if (search.value) {
                e.preventDefault();
                search.value = "";
                search.dispatchEvent(new Event("input"));
            } else {
                search.blur();
            }
        }
    });
}
