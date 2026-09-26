"use strict";

// info-media.js — вкладка «Медиа» правой панели (макеты InfoMedia,
// InfoMediaSearch): Фото · Файлы · Ссылки со счётчиками, группы по месяцам.
// Поиск спрятан за иконкой: строка вкладок превращается в поле, × и Esc
// возвращают вкладки. Результаты — по всем видам сразу, клик ведёт к
// сообщению в чате.

import { svc, state, formatBytes, Events } from "./core.js";
import { t, getLocale } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { openLightbox } from "./lightbox.js";
import { resolveAttachmentURL, buildAttachmentItems, jumpToMessage, openAttachmentNative, fileExtInfo, forwardAttachments } from "./messages.js";
import { buildDialogOverlay, emptyState } from "./ui.js";
import { showSuccess, showError } from "./toast.js";
import { Browser } from "/wails/runtime.js";

var cache = { key: "", media: [], links: [], ready: false, promise: null };
var kind = "photo";      // photo | file | link
var searchOpen = false;
var query = "";
var shownKey = "";       // чат, для которого выставлены вкладка и поиск
var barEl = null, bodyEl = null;
// «Выбрать» (фаза 7e): мультивыбор фото/файлов для «Сохранить» и «Переслать».
var selectMode = false;
var selected = new Map(); // id вложения → элемент медиа

function chatKey() {
    if (state.selectedGroupID) return "g:" + state.selectedGroupID;
    var pid = String(state.selectedPeerID || "");
    if (!pid || pid.indexOf("__mesh:") === 0) return "";
    return "p:" + pid;
}

// load — медиа и ссылки чата одним заходом; force — не брать кеш.
function load(force) {
    var key = chatKey();
    if (!key) return Promise.resolve({ key: "", media: [], links: [], ready: true });
    if (!force && cache.key === key && cache.promise) return cache.promise;
    var peer = key.charAt(0) === "p" ? key.slice(2) : "";
    var group = key.charAt(0) === "g" ? key.slice(2) : "";
    var entry = { key: key, media: [], links: [], ready: false, promise: null };
    entry.promise = Promise.all([
        svc("GetChatMedia", peer, group, "all", 500, 0).catch(function (err) { console.error("GetChatMedia:", err); return []; }),
        svc("GetChatLinks", peer, group, 500).catch(function (err) { console.error("GetChatLinks:", err); return []; }),
    ]).then(function (res) {
        entry.media = res[0] || [];
        entry.links = res[1] || [];
        entry.ready = true;
        return entry;
    });
    cache = entry;
    return entry.promise;
}

// getMediaSummary — счётчики для строки «Медиа» во вкладке «Инфо».
export function getMediaSummary() {
    return load(false).then(function (c) {
        var p = 0, f = 0;
        c.media.forEach(function (m) { if (m.kind === "photo") p++; else f++; });
        return { photos: p, files: f, links: c.links.length };
    });
}

function isVisible() {
    var root = document.getElementById("info-tab-media");
    var panel = document.getElementById("info-panel");
    return !!root && !!panel && root.classList.contains("active") && panel.classList.contains("open");
}

// resetMediaIfChatChanged — при смене чата: вкладка «Фото», поиск закрыт.
export function resetMediaIfChatChanged() {
    var key = chatKey();
    if (key === shownKey) return;
    shownKey = key;
    kind = "photo";
    searchOpen = false;
    query = "";
    selectMode = false;
    selected.clear();
    if (cache.key !== key) cache = { key: "", media: [], links: [], ready: false, promise: null };
    if (isVisible()) populateMediaTab();
}

export function populateMediaTab() {
    var root = document.getElementById("info-tab-media");
    if (!root) return;
    if (shownKey !== chatKey()) resetMediaIfChatChanged();
    root.innerHTML = "";
    barEl = el("div", "im-bar");
    bodyEl = el("div", "im-body");
    root.appendChild(barEl);
    root.appendChild(bodyEl);
    renderBar();
    renderBody();
    refreshData();
}

function refreshData() {
    load(true).then(function (c) {
        if (c.key !== chatKey() || !isVisible()) return;
        if (!searchOpen) renderBar();
        renderBody();
    });
}

// Новое сообщение с вложением или ссылкой в открытом чате, докачанный файл —
// данные устарели (сам запрос дешёвый, перезапрашиваем целиком).
var staleTimer = null;
function markStale(key) {
    if (key && key !== chatKey()) return;
    if (cache.key === chatKey()) cache.promise = null;
    if (!isVisible()) return;
    clearTimeout(staleTimer);
    staleTimer = setTimeout(refreshData, 600);
}
Events.On("vox:message", function (ev) {
    var m = ev.data || {};
    var pid = m.outbound ? m.peerId : m.senderId;
    if (pid) markStale("p:" + pid);
});
Events.On("vox:group-message", function (ev) {
    var d = ev.data || {};
    if (d.groupId) markStale("g:" + d.groupId);
});
document.addEventListener("vox:download-updated", function () { markStale(""); });

// --- построители ---

function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
}

function icon(name, size) {
    var i = document.createElement("i");
    i.setAttribute("data-lucide", name);
    i.className = "icon-" + (size || 16);
    return i;
}

function icons(node) {
    if (window.lucide) lucide.createIcons({ nodes: [node] });
}

function iconBtn(name, title, onClick) {
    var b = el("button", "ip-icon-btn");
    b.type = "button";
    b.title = title;
    b.appendChild(icon(name, 16));
    b.addEventListener("click", onClick);
    return b;
}

function locale() { return getLocale() === "ru" ? "ru-RU" : "en-US"; }

function shortDate(ms) {
    var d = new Date(ms);
    var opts = { day: "numeric", month: "short" };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
    return d.toLocaleDateString(locale(), opts).replace(".", "");
}

function monthLabel(ms) {
    var d = new Date(ms);
    var s = new Intl.DateTimeFormat(locale(), { month: "long" }).format(d);
    s = s.charAt(0).toUpperCase() + s.slice(1);
    if (d.getFullYear() !== new Date().getFullYear()) s += " " + d.getFullYear();
    return s;
}

// groupByMonth — элементы уже отсортированы по времени (новые сверху).
function groupByMonth(items) {
    var out = [];
    items.forEach(function (it) {
        var label = monthLabel(it.timestamp);
        if (!out.length || out[out.length - 1].label !== label) out.push({ label: label, items: [] });
        out[out.length - 1].items.push(it);
    });
    return out;
}

// hl — текст с подсветкой совпадений (q в нижнем регистре; null — без подсветки).
function hl(text, q) {
    var frag = document.createDocumentFragment();
    text = text || "";
    if (!q) { frag.appendChild(document.createTextNode(text)); return frag; }
    var lower = text.toLowerCase();
    var i = 0;
    for (;;) {
        var j = lower.indexOf(q, i);
        if (j < 0) break;
        frag.appendChild(document.createTextNode(text.slice(i, j)));
        frag.appendChild(el("b", "im-hl", text.slice(j, j + q.length)));
        i = j + q.length;
    }
    frag.appendChild(document.createTextNode(text.slice(i)));
    return frag;
}

// sectionHead — «Сентябрь · 14». items — месяц во вкладках «Фото»/«Файлы»:
// справа «Выбрать» (макет InfoMedia), в режиме выбора — «Выбрать все» за месяц.
function sectionHead(label, count, items) {
    var s = el("div", "ip-sec im-sec");
    s.appendChild(el("span", null, label + " · " + count));
    if (items && items.length) {
        var a = el("button", "ip-sec-link", t(selectMode ? "info.media.select_all" : "info.media.select"));
        a.type = "button";
        a.addEventListener("click", function () {
            if (!selectMode) { enterSelect(); return; }
            items.forEach(function (it) { selected.set(it.id, it); });
            renderBar();
            syncMarks();
        });
        s.appendChild(a);
    }
    return s;
}

// --- режим выбора ---

function enterSelect() {
    selectMode = true;
    selected.clear();
    renderBar();
    renderBody();
}

function exitSelect() {
    if (!selectMode) return;
    selectMode = false;
    selected.clear();
    renderBar();
    renderBody();
}

function toggleSelected(item) {
    if (selected.has(item.id)) selected.delete(item.id); else selected.set(item.id, item);
    if (!selected.size) { exitSelect(); return; }
    renderBar();
    syncMarks();
}

// syncMarks — отметки без перерисовки (картинки не перезагружаются).
function syncMarks() {
    if (!bodyEl) return;
    bodyEl.classList.toggle("im-selecting", selectMode);
    bodyEl.querySelectorAll("[data-att-id]").forEach(function (n) {
        n.classList.toggle("is-selected", selected.has(n.dataset.attId));
    });
}

function checkMark() {
    var c = el("span", "im-check");
    c.appendChild(icon("check", 12));
    return c;
}

function saveSelected() {
    var list = Array.from(selected.values());
    svc("SaveAttachmentsTo", list.map(function (x) { return x.id; }), list.map(function (x) { return x.name || ""; })).then(function (res) {
        if (!res || res.cancelled) return;
        showSuccess(t("info.media.saved_n").replace("{n}", String(res.saved)).replace("{m}", String(res.total)));
        exitSelect();
    }).catch(function (err) {
        showError(String(err && err.message || err), { title: t("info.media.save") });
    });
}

function forwardSelected() {
    var list = Array.from(selected.values());
    exitSelect();
    forwardAttachments(list);
}

// selectAction — действие шапки выбора. Только иконка с подсказкой: в панели
// 320 px «Выбрано 12 · Сохранить · Переслать» по-русски не помещается.
function selectAction(name, label, onClick) {
    var b = el("button", "select-action im-select-action");
    b.type = "button";
    b.title = label;
    b.setAttribute("aria-label", label);
    b.appendChild(icon(name, 16));
    b.addEventListener("click", onClick);
    return b;
}

document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && selectMode && isVisible()) { e.stopPropagation(); exitSelect(); }
});

function counts() {
    var c = { ready: cache.ready && cache.key === chatKey(), photo: 0, file: 0, link: 0 };
    if (!c.ready) return c;
    cache.media.forEach(function (m) { if (m.kind === "photo") c.photo++; else c.file++; });
    c.link = cache.links.length;
    return c;
}

// --- строка вкладок / поле поиска ---

function renderBar() {
    if (!barEl) return;
    barEl.innerHTML = "";
    barEl.classList.toggle("im-select-bar", selectMode);
    if (selectMode) {
        // шапка выбора как у сообщений (макет «Выбор сообщений»): ✕ Выбрано N
        barEl.appendChild(iconBtn("x", t("dialog.cancel"), exitSelect));
        barEl.appendChild(el("span", "select-count", t("select.selected_n").replace("{n}", String(selected.size))));
        barEl.appendChild(el("span", "im-spacer"));
        barEl.appendChild(selectAction("download", t("info.media.save"), saveSelected));
        barEl.appendChild(selectAction("forward", t("select.forward"), forwardSelected));
        icons(barEl);
        return;
    }
    if (searchOpen) {
        var field = el("label", "im-search");
        field.appendChild(icon("search", 14));
        var input = document.createElement("input");
        input.type = "text";
        input.className = "im-search-input";
        input.placeholder = t("info.media.search");
        input.value = query;
        input.addEventListener("input", function () { query = input.value.trim(); renderBody(); });
        input.addEventListener("keydown", function (e) {
            if (e.key === "Escape") { e.stopPropagation(); closeSearch(); }
        });
        field.appendChild(input);
        barEl.appendChild(field);
        barEl.appendChild(iconBtn("x", t("info.media.search_close"), closeSearch));
        icons(barEl);
        setTimeout(function () { input.focus(); }, 0);
        return;
    }
    var c = counts();
    [["photo", "info.media.photos", c.photo], ["file", "info.media.files", c.file], ["link", "info.media.links", c.link]].forEach(function (d) {
        var b = el("button", "im-tab" + (kind === d[0] ? " active" : ""));
        b.type = "button";
        b.appendChild(document.createTextNode(t(d[1])));
        if (c.ready) b.appendChild(el("span", "im-tab-count", String(d[2])));
        b.addEventListener("click", function () { kind = d[0]; renderBar(); renderBody(); });
        barEl.appendChild(b);
    });
    barEl.appendChild(el("span", "im-spacer"));
    barEl.appendChild(iconBtn("search", t("info.media.search_open"), function () {
        searchOpen = true;
        renderBar();
        renderBody();
    }));
    icons(barEl);
}

function closeSearch() {
    searchOpen = false;
    query = "";
    renderBar();
    renderBody();
}

// --- содержимое ---

function renderBody() {
    if (!bodyEl) return;
    bodyEl.innerHTML = "";
    if (!chatKey()) { bodyEl.appendChild(el("div", "im-empty", t("info.media.empty_all"))); return; }
    if (!cache.ready || cache.key !== chatKey()) { bodyEl.appendChild(el("div", "im-empty", "…")); return; }
    if (searchOpen) { renderSearch(); return; }

    var items = kind === "link" ? cache.links
        : cache.media.filter(function (m) { return kind === "photo" ? m.kind === "photo" : m.kind !== "photo"; });
    if (!items.length) {
        bodyEl.appendChild(emptyState(kind === "photo" ? "image" : kind === "file" ? "file" : "link",
            t(kind === "photo" ? "info.media.empty_photos" : kind === "file" ? "info.media.empty_files" : "info.media.empty_links"),
            t("info.media.empty_hint")));
        return;
    }
    groupByMonth(items).forEach(function (grp) {
        bodyEl.appendChild(sectionHead(grp.label, grp.items.length, kind === "link" ? null : grp.items));
        if (kind === "photo") bodyEl.appendChild(photoGrid(grp.items, items, false));
        else if (kind === "file") grp.items.forEach(function (f) { bodyEl.appendChild(fileRow(f, null)); });
        else grp.items.forEach(function (l) { bodyEl.appendChild(linkRow(l, null)); });
    });
    if (kind !== "link" && !selectMode) appendClearMediaButton(bodyEl);
    icons(bodyEl);
    syncMarks();
}

function renderSearch() {
    var q = query.toLowerCase();
    if (!q) { bodyEl.appendChild(el("div", "im-hint", t("info.media.search_empty"))); return; }
    var match = function (s) { return (s || "").toLowerCase().indexOf(q) !== -1; };
    var photos = cache.media.filter(function (m) { return m.kind === "photo" && (match(m.name) || match(m.caption)); });
    var files = cache.media.filter(function (m) { return m.kind !== "photo" && (match(m.name) || match(m.caption)); });
    var links = cache.links.filter(function (l) { return match(l.url) || match(l.text); });
    var total = photos.length + files.length + links.length;
    bodyEl.appendChild(el("div", "im-hint", total
        ? t("info.media.found").replace("{n}", total) + " · " + t("info.media.search_hint")
        : t("info.media.nothing")));
    if (photos.length) {
        bodyEl.appendChild(sectionHead(t("info.media.photos"), photos.length));
        bodyEl.appendChild(photoGrid(photos, photos, true));
    }
    if (files.length) {
        bodyEl.appendChild(sectionHead(t("info.media.files"), files.length));
        files.forEach(function (f) { bodyEl.appendChild(fileRow(f, q)); });
    }
    if (links.length) {
        bodyEl.appendChild(sectionHead(t("info.media.links"), links.length));
        links.forEach(function (l) { bodyEl.appendChild(linkRow(l, q)); });
    }
    if (total) bodyEl.appendChild(el("div", "im-hint center", t("info.media.open_hint")));
    icons(bodyEl);
}

// showInChat — к сообщению в ленте; во время сцены лента живёт во вкладке «Чат».
function showInChat(msgId) {
    var chatArea = document.getElementById("chat-area");
    if (chatArea && chatArea.classList.contains("stage-active")) {
        document.dispatchEvent(new CustomEvent("vox:ui-open-info-panel", { detail: { tab: "chat" } }));
    }
    jumpToMessage(msgId);
}

function withShowInChat(items, msgId) {
    if (!msgId) return items;
    items.push({ separator: true });
    items.push({ label: t("ctx.show_in_chat"), icon: "message-square", action: function () { showInChat(msgId); } });
    return items;
}

function lightboxItems(list) {
    return list.map(function (p) {
        return { id: p.id, caption: p.caption || "", name: p.name, mime: p.mime, sha256: p.sha256, timestamp: p.timestamp, msgId: p.msgId };
    });
}

// photoGrid — сетка 3 колонки; в поиске (withDates) — плашка с датой и клик к сообщению.
function photoGrid(photos, lbList, withDates) {
    var grid = el("div", "im-grid");
    photos.forEach(function (p) {
        var cell = el("div", "im-cell");
        var img = document.createElement("img");
        img.alt = p.name || "";
        img.loading = "lazy";
        img.src = resolveAttachmentURL(p, img);
        cell.appendChild(img);
        if (withDates) cell.appendChild(el("span", "im-cell-date", shortDate(p.timestamp)));
        cell.dataset.attId = p.id;
        cell.appendChild(checkMark());
        var openLb = function () { openLightbox(lightboxItems(lbList), { startIndex: lbList.indexOf(p) }); };
        cell.addEventListener("click", function () {
            if (selectMode) { toggleSelected(p); return; }
            if (withDates) showInChat(p.msgId); else openLb();
        });
        cell.addEventListener("contextmenu", function (e) {
            e.preventDefault();
            e.stopPropagation();
            showContextMenu(e, withShowInChat(buildAttachmentItems(p, { isPhoto: true, imgEl: img, onOpen: openLb }), p.msgId));
        });
        grid.appendChild(cell);
    });
    return grid;
}

function fileRow(f, q) {
    var r = el("div", "im-row");
    var info = fileExtInfo(f.name);
    var badge = el("div", "im-ext");
    if (info.ext) {
        badge.textContent = info.ext.slice(0, 4);
        if (info.kind) badge.dataset.kind = info.kind;
    } else {
        badge.appendChild(icon("file", 16));
    }
    r.appendChild(badge);
    var meta = el("div", "im-meta");
    var nm = el("div", "im-name");
    nm.appendChild(hl(f.name || "—", q));
    nm.title = f.name || "";
    meta.appendChild(nm);
    meta.appendChild(el("div", "im-info", formatBytes(f.size || 0) + " · " + shortDate(f.timestamp)));
    r.appendChild(meta);
    r.appendChild(iconBtn("download", t("ctx.save_as"), function (e) {
        e.stopPropagation();
        svc("SaveAttachmentAs", f.id, f.name || "attachment").catch(function () {});
    }));
    r.dataset.attId = f.id;
    r.appendChild(checkMark());
    r.addEventListener("click", function () {
        if (selectMode) { toggleSelected(f); return; }
        if (q !== null) showInChat(f.msgId); else openAttachmentNative(f);
    });
    r.addEventListener("contextmenu", function (e) {
        e.preventDefault();
        e.stopPropagation();
        showContextMenu(e, withShowInChat(buildAttachmentItems(f, {}), f.msgId));
    });
    return r;
}

function linkRow(l, q) {
    var r = el("div", "im-row");
    var ic = el("div", "im-link-icon");
    ic.appendChild(icon("link", 14));
    r.appendChild(ic);
    var meta = el("div", "im-meta");
    var title = el("div", "im-name");
    title.appendChild(hl(l.text || l.url.replace(/^https?:\/\//i, ""), q));
    title.title = l.url;
    meta.appendChild(title);
    var sub = el("div", "im-info");
    sub.appendChild(hl(l.host, q));
    sub.appendChild(document.createTextNode(" · " + shortDate(l.timestamp)));
    meta.appendChild(sub);
    r.appendChild(meta);
    r.addEventListener("click", function () { if (q !== null) showInChat(l.msgId); else openLink(l.url); });
    r.addEventListener("contextmenu", function (e) {
        e.preventDefault();
        e.stopPropagation();
        showContextMenu(e, withShowInChat([
            { label: t("ctx.open_link"), icon: "external-link", action: function () { openLink(l.url); } },
            { label: t("ctx.copy_link"), icon: "copy", action: function () {
                try { navigator.clipboard.writeText(l.url); showSuccess(t("info.copied")); } catch (err) { /* нет фокуса */ }
            } },
        ], l.msgId));
    });
    return r;
}

// openLink — только http(s): ссылка пришла из чужого сообщения.
export function openLink(url) {
    if (!/^https?:\/\//i.test(url || "")) return;
    Promise.resolve(Browser.OpenURL(url)).catch(function (err) {
        showError(String(err && err.message || err));
    });
}

// --- очистка медиа чата (существующая функция: освобождает место на диске) ---

function appendClearMediaButton(container) {
    var totalBytes = 0;
    cache.media.forEach(function (m) { totalBytes += m.size || 0; });
    var btnRow = el("div", "media-clear-row");
    var btn = el("button", "media-clear-btn");
    btn.type = "button";
    btn.appendChild(icon("trash-2", 16));
    btn.appendChild(el("span", null, t("media.clear_btn").replace("{count}", cache.media.length).replace("{size}", formatBytes(totalBytes))));
    btn.addEventListener("click", onClearMediaClick);
    btnRow.appendChild(btn);
    container.appendChild(btnRow);
}

function onClearMediaClick() {
    var key = chatKey();
    if (!key) return;
    var totalBytes = 0;
    cache.media.forEach(function (m) { totalBytes += m.size || 0; });
    var body =
        '<div class="dialog-text">' +
            '<p>' + t("media.clear_body").replace("{count}", cache.media.length).replace("{size}", formatBytes(totalBytes)) + '</p>' +
            '<p style="color: var(--text-secondary); font-size: 13px;">' + t("media.clear_note") + '</p>' +
        '</div>';
    buildDialogOverlay(t("media.clear_title"), body, t("dialog.cancel"), t("media.clear_confirm"), function (overlay) {
        overlay.remove();
        var peer = key.charAt(0) === "p" ? key.slice(2) : "";
        var group = key.charAt(0) === "g" ? key.slice(2) : "";
        svc("ClearChatMedia", peer, group).then(function (res) {
            if (res && res.messages > 0) console.log("[CLEAR-MEDIA] removed " + res.messages + " msgs, " + res.bytesFreed + " bytes");
            refreshData();
            document.dispatchEvent(new CustomEvent("vox:chat-cleared"));
        }).catch(function (err) {
            console.error("[CLEAR-MEDIA] failed:", err);
        });
    });
}
