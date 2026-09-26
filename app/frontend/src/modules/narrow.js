"use strict";

// narrow.js — раскладка узкого окна (макет Narrow, фаза 5c).
//   Рейка — список чатов складывается в 72px из аватаров (бейджи и активный
//     остаются, имя — подсказкой), поиск — лупой с поповером. Включается не
//     по ширине окна, а когда центр (сцена звонка, иначе колонка чата) при
//     полном списке был бы уже 600px: так учитывается и открытая панель
//     справа (320, в звонке 360). Окно 1400 вмещает всё полностью.
//   < 1000 — правая панель выдвигается поверх чата с затемнением,
//     закрывается кликом мимо или Esc.
// Рейка только в «Чатах» и «Группах»: в «Контактах» карточки заявок с
// кнопками в 72px не помещаются. Панель пользователя (#user-bar) в рейке
// переезжает в низ nav и встаёт вертикально: её кнопки голоса — единственное
// управление звонком, когда открыт другой чат (полоска call-bar — только
// «К звонку»). Модули ищут эти кнопки по id, перенос DOM-узла им не мешает.

import { getViewMode } from "./contacts-list.js";

const mqDrawer = window.matchMedia("(max-width: 999px)");
const CENTER_MIN = 600; // уже — список складывается в рейку
const LIST_W = 320;     // #contacts-panel без рейки (contacts.css)

const body = document.body;
const chatArea = document.getElementById("chat-area");
const stageBody = document.getElementById("stage-body");
const panel = document.getElementById("contacts-panel");
const nav = document.getElementById("nav-strip");
const userBar = document.getElementById("user-bar");
const list = document.getElementById("contacts-list");
const info = document.getElementById("info-panel");
const backdrop = document.getElementById("info-backdrop");
const searchBtn = document.getElementById("rail-search-btn");
const searchInput = document.getElementById("search-input");

// Ширина центра при полном списке. Считается по фактическим ширинам, а не
// по классу rail: во время смены ширины списка решение не перескакивает.
// null — центр не виден (настройки, экран входа), решение не менять.
function centerWidthFull() {
    const inStage = chatArea.classList.contains("stage-active") && stageBody.offsetWidth > 0;
    const w = (inStage ? stageBody : chatArea).getBoundingClientRect().width;
    if (w <= 0) return null;
    return w + panel.getBoundingClientRect().width - LIST_W;
}

function railWanted() {
    const mode = getViewMode();
    if (mode !== "messages" && mode !== "groups") return false;
    const w = centerWidthFull();
    if (w === null) return body.classList.contains("rail");
    return w < CENTER_MIN;
}

// Имя — подсказкой: в рейке видны только аватары.
function syncTitles() {
    if (!body.classList.contains("rail")) return;
    list.querySelectorAll(".contact-item").forEach((it) => {
        const n = it.querySelector(".contact-name");
        if (n && it.title !== n.textContent) it.title = n.textContent;
    });
}

function clearTitles() {
    list.querySelectorAll(".contact-item[title]").forEach((it) => it.removeAttribute("title"));
}

function closeSearch() {
    panel.classList.remove("search-open");
}

function apply() {
    const rail = railWanted();
    if (rail !== body.classList.contains("rail")) {
        body.classList.toggle("rail", rail);
        if (rail) {
            nav.appendChild(userBar);
            syncTitles();
        } else {
            panel.appendChild(userBar);
            closeSearch();
            clearTitles();
        }
    }
    body.classList.toggle("narrow-drawer", mqDrawer.matches);
    syncBackdrop();
}

function drawerOpen() {
    return body.classList.contains("narrow-drawer") && info.classList.contains("open");
}

function syncBackdrop() {
    backdrop.hidden = !drawerOpen();
}

function closeDrawer() {
    document.dispatchEvent(new CustomEvent("vox:ui-close-info-panel"));
}

mqDrawer.addEventListener("change", apply);
// Центр меняет ширину при ресайзе окна, открытии панели справа, начале и
// конце звонка. В rAF — переключение рейки само меняет ширину центра.
let rafPending = false;
new ResizeObserver(() => {
    if (rafPending) return;
    rafPending = true;
    requestAnimationFrame(() => { rafPending = false; apply(); });
}).observe(chatArea);
// Режим списка меняется кликом по nav — пересчитать после обработчиков.
nav.addEventListener("click", (e) => {
    if (e.target.closest(".nav-item")) setTimeout(apply, 0);
});
// Список перерисовывается часто — подсказки ставим заново.
new MutationObserver(syncTitles).observe(list, { childList: true });
new MutationObserver(syncBackdrop).observe(info, { attributes: true, attributeFilter: ["class"] });

backdrop.addEventListener("click", closeDrawer);
document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape" || !drawerOpen()) return;
    // Esc у открытого диалога или лайтбокса закрывает их, а не панель.
    if (document.querySelector(".backup-overlay, .lightbox-overlay.is-open")) return;
    closeDrawer();
});

searchBtn.addEventListener("click", (e) => {
    e.stopPropagation();
    const open = !panel.classList.contains("search-open");
    panel.classList.toggle("search-open", open);
    if (open) setTimeout(() => searchInput.focus(), 0);
});
searchInput.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && panel.classList.contains("search-open")) {
        e.stopPropagation();
        searchInput.value = "";
        searchInput.dispatchEvent(new Event("input"));
        closeSearch();
    }
});
document.addEventListener("click", (e) => {
    if (!panel.classList.contains("search-open")) return;
    if (e.target.closest(".search-wrap") || e.target.closest("#rail-search-btn")) return;
    if (!searchInput.value) closeSearch();
});

// Не сразу: модуль грузится по кругу contacts-list → events → narrow →
// contacts-list, и в момент выполнения этого файла viewMode из
// contacts-list ещё не присвоен — рейка при старте не включалась, пока не
// кликнешь по nav.
setTimeout(apply, 0);
