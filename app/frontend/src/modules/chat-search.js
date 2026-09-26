"use strict";

// chat-search.js — поиск по открытому чату (макет ChatSearch).
//
// Поле встаёт в шапку чата вместо кнопок: «2 из 3», стрелки (раньше / позже),
// фильтр «от: все / имя», ×. Ctrl+F открывает, Esc закрывает, Enter —
// следующее совпадение (старше), Shift+Enter — предыдущее.
// Поиск идёт в БД (MessagingService.SearchMessages, кириллица без учёта
// регистра); у каждого совпадения есть offset от новейшего сообщения — лента
// догружает историю до него (messages.js loadOlderUntil) и прокручивается.
// Совпавшие слова — <mark class="search-hit">, текущий пузырь — рамка.

import { svc, state, dom } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { loadOlderUntil, scrollToMessage } from "./messages.js";

var el = {};
var isOpen = false;
var hits = [];        // [{id, senderId, timestamp, offset}] — новые первыми
var cur = -1;
var query = "";
var fromId = "";
var fromLabel = "";
var timer = null;
var runGen = 0;

function chatOpen() {
    var pid = state.selectedPeerID;
    if (typeof pid === "string" && pid.indexOf("__mesh:") === 0) return false;
    return !!(pid || state.selectedGroupID);
}

function headerUsable() {
    var ca = document.getElementById("chat-area");
    return !!dom.chatHeader && dom.chatHeader.style.display !== "none" &&
        !(ca && (ca.classList.contains("stage-active") || ca.classList.contains("select-mode")));
}

export function openSearch() {
    if (!chatOpen() || !headerUsable()) return;
    if (isOpen) { el.input.focus(); el.input.select(); return; }
    isOpen = true;
    fromId = "";
    fromLabel = "";
    updateFrom();
    el.input.value = "";
    hits = [];
    cur = -1;
    updateCounter();
    dom.chatHeader.classList.add("searching");
    el.bar.hidden = false;
    el.input.focus();
}

export function closeSearch() {
    if (!isOpen) return;
    isOpen = false;
    hits = [];
    cur = -1;
    query = "";
    runGen++;
    clearTimeout(timer);
    dom.chatHeader.classList.remove("searching");
    el.bar.hidden = true;
    clearHighlights();
}

function updateCounter() {
    if (!query) { el.count.textContent = ""; return; }
    el.count.textContent = hits.length === 0
        ? t("search.none")
        : t("search.count").replace("{i}", String(cur + 1)).replace("{n}", String(hits.length));
}

function updateFrom() {
    el.fromLbl.textContent = t("search.from") + ": " + (fromId ? fromLabel : t("search.from_all_short"));
}

function schedule() {
    clearTimeout(timer);
    timer = setTimeout(run, 250);
}

async function run() {
    var q = el.input.value.trim();
    query = q;
    var gen = ++runGen;
    clearHighlights();
    if (!q) { hits = []; cur = -1; updateCounter(); return; }
    var res = [];
    try {
        res = await svc("SearchMessages",
            state.selectedGroupID ? "" : (state.selectedPeerID || ""),
            state.selectedGroupID || "", q, fromId);
    } catch (e) {
        res = [];
    }
    if (gen !== runGen) return;
    hits = res || [];
    cur = hits.length ? 0 : -1;
    updateCounter();
    if (cur >= 0) go(0);
}

async function go(i) {
    if (hits.length === 0) return;
    cur = (i + hits.length) % hits.length;
    updateCounter();
    var gen = runGen;
    var hit = hits[cur];
    await loadOlderUntil(hit.offset);
    if (gen !== runGen || !isOpen) return;
    highlightAll();
    var row = scrollToMessage(hit.id);
    if (row) row.classList.add("search-current");
}

// --- подсветка ---

function markText(node, q) {
    var ql = q.toLowerCase();
    var walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT, null);
    var texts = [];
    while (walker.nextNode()) texts.push(walker.currentNode);
    texts.forEach(function (tn) {
        if (!tn.parentNode || tn.parentNode.nodeName === "MARK") return;
        var text = tn.nodeValue;
        var low = text.toLowerCase();
        var idx = low.indexOf(ql);
        if (idx < 0) return;
        var frag = document.createDocumentFragment();
        var pos = 0;
        while (idx >= 0) {
            frag.appendChild(document.createTextNode(text.slice(pos, idx)));
            var m = document.createElement("mark");
            m.className = "search-hit";
            m.textContent = text.slice(idx, idx + q.length);
            frag.appendChild(m);
            pos = idx + q.length;
            idx = low.indexOf(ql, pos);
        }
        frag.appendChild(document.createTextNode(text.slice(pos)));
        tn.parentNode.replaceChild(frag, tn);
    });
}

function highlightAll() {
    clearHighlights();
    if (!query) return;
    var ids = {};
    for (var i = 0; i < hits.length; i++) ids[String(hits[i].id)] = true;
    var rows = dom.messagesDiv.querySelectorAll(".message-row[data-msg-id]");
    for (var r = 0; r < rows.length; r++) {
        if (!ids[rows[r].dataset.msgId]) continue;
        var nodes = rows[r].querySelectorAll(".bubble-content, .attach-caption");
        for (var n = 0; n < nodes.length; n++) markText(nodes[n], query);
    }
    if (cur >= 0 && hits[cur]) {
        var row = dom.messagesDiv.querySelector('.message-row[data-msg-id="' + CSS.escape(String(hits[cur].id)) + '"]');
        if (row) row.classList.add("search-current");
    }
}

function clearHighlights() {
    var marks = dom.messagesDiv.querySelectorAll("mark.search-hit");
    for (var i = 0; i < marks.length; i++) {
        var p = marks[i].parentNode;
        if (!p) continue;
        p.replaceChild(document.createTextNode(marks[i].textContent), marks[i]);
        p.normalize();
    }
    var c = dom.messagesDiv.querySelectorAll(".message-row.search-current");
    for (var j = 0; j < c.length; j++) c[j].classList.remove("search-current");
}

// --- фильтр «от» ---

async function pickFrom(e) {
    e.stopPropagation(); // иначе document click сразу закроет меню
    var opts = [{ id: "", label: t("search.from_all") }];
    if (state.selectedGroupID) {
        var mem = [];
        try { mem = (await svc("GetGroupMembers", state.selectedGroupID)) || []; } catch (err) { mem = []; }
        mem.forEach(function (m) {
            opts.push({ id: m.peerId, label: m.peerId === state.myID ? t("search.from_me") : (m.nickname || m.peerId.slice(0, 8)) });
        });
    } else {
        opts.push({ id: state.myID, label: t("search.from_me") });
        var nick = state.selectedPeerInfo && (state.selectedPeerInfo.nickname || state.selectedPeerInfo.shortId);
        opts.push({ id: state.selectedPeerID, label: nick || t("search.from_peer") });
    }
    var r = el.fromBtn.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.bottom + 2, preventDefault: function () {}, stopPropagation: function () {} },
        opts.map(function (o) {
            return {
                label: (o.id === fromId ? "✓ " : "") + o.label,
                action: function () { fromId = o.id; fromLabel = o.label; updateFrom(); run(); },
            };
        }));
}

export function initChatSearch() {
    el.bar = document.getElementById("chat-search");
    el.input = document.getElementById("chat-search-input");
    el.count = document.getElementById("chat-search-count");
    el.fromBtn = document.getElementById("chat-search-from");
    el.fromLbl = document.getElementById("chat-search-from-lbl");
    if (!el.bar || !el.input) return;

    var btn = document.getElementById("search-btn");
    if (btn) btn.addEventListener("click", openSearch);
    document.getElementById("chat-search-close").addEventListener("click", closeSearch);
    document.getElementById("chat-search-up").addEventListener("click", function () { go(cur + 1); });
    document.getElementById("chat-search-down").addEventListener("click", function () { go(cur - 1); });
    el.fromBtn.addEventListener("click", pickFrom);

    el.input.addEventListener("input", schedule);
    el.input.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
            e.preventDefault();
            if (query !== el.input.value.trim()) { clearTimeout(timer); run(); return; }
            go(e.shiftKey ? cur - 1 : cur + 1);
        } else if (e.key === "Escape") {
            e.preventDefault();
            e.stopPropagation();
            closeSearch();
        }
    });

    document.addEventListener("keydown", function (e) {
        if ((e.ctrlKey || e.metaKey) && !e.shiftKey && !e.altKey && (e.code === "KeyF" || e.key === "f" || e.key === "F")) {
            if (!chatOpen() || !headerUsable()) return;
            e.preventDefault();
            openSearch();
        }
    });

    // Лента перерисована (подгрузка старых, удаление) — вернуть подсветку
    document.addEventListener("vox:ui-messages-rendered", function () {
        if (isOpen && query) highlightAll();
    });
    document.addEventListener("vox:ui-chat-changed", closeSearch);
}
