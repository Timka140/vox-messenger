"use strict";

// lightbox.js — просмотр фото во весь экран (макет Lightbox, фаза 5b).
//
// openLightbox(srcOrItems, opts)
//   items: [{ src?, id?, sha256?, name?, mime?, caption?, senderId?,
//             senderName?, timestamp?, msgId? }]
//   opts: { caption, startIndex }
//
// Шапка: автор, «вчера, 16:41 · 3 из 9», действия Масштаб · Сохранить ·
// Переслать · Открыть в программе · Ещё · Закрыть. Круглые стрелки по бокам,
// лента миниатюр снизу, подпись — текст сообщения. Esc и клик по фону
// закрывают, ←/→ листают; колесо и двойной клик — масштаб, при увеличении
// фото перетаскивается.

import { svc, formatDateLabel } from "./core.js";
import { t, getLocale } from "./i18n.js";
import { resolveAttachmentURL, buildAttachmentItems, showAttachForwardDialog, openAttachmentNative, jumpToMessage } from "./messages.js";
import { showContextMenu } from "./contextmenu.js";
import { applyAvatar } from "./avatar.js";

const MAX_ZOOM = 4;

let el = null;
let items = [];
let index = 0;
let keyHandler = null;
const zoom = { s: 1, x: 0, y: 0, drag: null };

function q(sel) { return el.overlay.querySelector(sel); }

function actionBtn(icon, title, onClick) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "lb-btn";
    b.title = title;
    b.innerHTML = '<i data-lucide="' + icon + '" class="icon-20"></i>';
    b.addEventListener("click", (e) => { e.stopPropagation(); onClick(e); });
    return b;
}

function ensureDOM() {
    if (el) return;
    const overlay = document.createElement("div");
    overlay.className = "lightbox-overlay";
    overlay.innerHTML =
        '<div class="lb-stage"><img class="lightbox-image" alt="" draggable="false"></div>' +
        '<div class="lb-head">' +
            '<div class="lb-av"></div>' +
            '<div class="lb-who"><span class="lb-name"></span><span class="lb-meta"></span></div>' +
            '<div class="lb-spacer"></div>' +
            '<div class="lb-actions"></div>' +
        '</div>' +
        '<button class="lightbox-nav lightbox-prev" type="button"><i data-lucide="arrow-left" class="icon-20"></i></button>' +
        '<button class="lightbox-nav lightbox-next" type="button"><i data-lucide="arrow-right" class="icon-20"></i></button>' +
        '<div class="lightbox-caption" hidden></div>' +
        '<div class="lb-strip" hidden></div>';
    document.body.appendChild(overlay);
    el = { overlay };
    el.stage = q(".lb-stage");
    el.img = q(".lightbox-image");
    el.av = q(".lb-av");
    el.name = q(".lb-name");
    el.meta = q(".lb-meta");
    el.prev = q(".lightbox-prev");
    el.next = q(".lightbox-next");
    el.caption = q(".lightbox-caption");
    el.strip = q(".lb-strip");

    const actions = q(".lb-actions");
    el.zoomBtn = actionBtn("zoom-in", t("lb.zoom"), () => setZoom(zoom.s > 1 ? 1 : 2));
    el.saveBtn = actionBtn("download", t("ctx.save_as"), downloadCurrent);
    el.fwdBtn = actionBtn("forward", t("info.media.forward"), forwardCurrent);
    el.extBtn = actionBtn("external-link", t("ctx.open_external"), () => { const it = items[index]; if (it && it.id) openAttachmentNative(it); });
    el.moreBtn = actionBtn("ellipsis", t("lb.more"), showMore);
    [el.zoomBtn, el.saveBtn, el.fwdBtn, el.extBtn, el.moreBtn, actionBtn("x", t("dialog.close"), closeLightbox)].forEach((b) => actions.appendChild(b));

    overlay.addEventListener("click", (e) => {
        if (e.target === overlay || e.target === el.stage) closeLightbox();
    });
    el.img.addEventListener("click", (e) => e.stopPropagation());
    el.img.addEventListener("dblclick", (e) => { e.stopPropagation(); setZoom(zoom.s > 1 ? 1 : 2); });
    el.stage.addEventListener("wheel", (e) => {
        e.preventDefault();
        setZoom(zoom.s * Math.exp(-e.deltaY * 0.0015));
    }, { passive: false });
    el.img.addEventListener("pointerdown", (e) => {
        if (zoom.s <= 1) return;
        zoom.drag = { x: e.clientX, y: e.clientY, sx: zoom.x, sy: zoom.y };
        el.img.setPointerCapture(e.pointerId);
    });
    el.img.addEventListener("pointermove", (e) => {
        if (!zoom.drag) return;
        zoom.x = zoom.drag.sx + (e.clientX - zoom.drag.x);
        zoom.y = zoom.drag.sy + (e.clientY - zoom.drag.y);
        applyZoom();
    });
    const endDrag = () => { zoom.drag = null; };
    el.img.addEventListener("pointerup", endDrag);
    el.img.addEventListener("pointercancel", endDrag);
    el.prev.addEventListener("click", (e) => { e.stopPropagation(); navigate(-1); });
    el.next.addEventListener("click", (e) => { e.stopPropagation(); navigate(1); });

    if (window.lucide) lucide.createIcons({ nodes: [overlay] });
}

function applyZoom() {
    el.img.style.transform = "translate(" + zoom.x + "px, " + zoom.y + "px) scale(" + zoom.s + ")";
    el.img.classList.toggle("zoomed", zoom.s > 1);
    el.zoomBtn.classList.toggle("on", zoom.s > 1);
}

function setZoom(s) {
    zoom.s = Math.min(MAX_ZOOM, Math.max(1, s));
    if (zoom.s === 1) { zoom.x = 0; zoom.y = 0; }
    applyZoom();
}

export function openLightbox(srcOrItems, opts) {
    ensureDOM();
    opts = opts || {};
    if (typeof srcOrItems === "string") {
        items = [{ src: srcOrItems, caption: opts.caption || "", name: "" }];
        index = 0;
    } else {
        items = srcOrItems || [];
        index = Math.max(0, Math.min(items.length - 1, opts.startIndex || 0));
    }
    if (items.length === 0) return;

    renderStrip();
    showCurrent();
    el.overlay.classList.add("is-open");

    if (!keyHandler) {
        keyHandler = function (e) {
            if (!el.overlay.classList.contains("is-open")) return;
            if (e.key === "Escape") { e.stopPropagation(); closeLightbox(); }
            else if (e.key === "ArrowLeft") { e.stopPropagation(); navigate(-1); }
            else if (e.key === "ArrowRight") { e.stopPropagation(); navigate(1); }
        };
        document.addEventListener("keydown", keyHandler);
    }
}

export function closeLightbox() {
    if (!el) return;
    el.overlay.classList.remove("is-open");
    el.img.src = "";
    el.strip.innerHTML = "";
    items = [];
    index = 0;
    setZoom(1);
    if (keyHandler) {
        document.removeEventListener("keydown", keyHandler);
        keyHandler = null;
    }
}

export function isOpen() {
    return !!el && el.overlay.classList.contains("is-open");
}

function imageURL(it, imgEl) {
    if (it.src) return it.src;
    if (it.id || it.sha256) return resolveAttachmentURL(it, imgEl);
    return "";
}

// whenText — «вчера, 16:41».
function whenText(ms) {
    const time = new Date(ms).toLocaleTimeString(getLocale() === "ru" ? "ru-RU" : "en-US", { hour: "2-digit", minute: "2-digit" });
    return formatDateLabel(ms).toLowerCase() + ", " + time;
}

function renderStrip() {
    el.strip.innerHTML = "";
    el.strip.hidden = items.length < 2;
    if (items.length < 2) return;
    items.forEach((it, i) => {
        const b = document.createElement("button");
        b.type = "button";
        b.className = "lb-thumb";
        const im = document.createElement("img");
        im.alt = "";
        im.src = imageURL(it, im);
        b.appendChild(im);
        b.addEventListener("click", (e) => { e.stopPropagation(); index = i; showCurrent(); });
        el.strip.appendChild(b);
    });
}

function showCurrent() {
    const it = items[index];
    if (!it) return;
    setZoom(1);
    el.img.alt = it.caption || it.name || "";
    el.img.src = imageURL(it, el.img);

    const hasAuthor = !!(it.senderId || it.senderName);
    el.av.hidden = !hasAuthor;
    if (hasAuthor) applyAvatar(el.av, it.senderId || "", it.senderName || "?");
    el.name.textContent = it.senderName || "";
    const meta = [];
    if (it.timestamp) meta.push(whenText(it.timestamp));
    if (items.length > 1) meta.push(t("lb.of").replace("{i}", String(index + 1)).replace("{n}", String(items.length)));
    el.meta.textContent = meta.join(" · ");

    el.caption.hidden = !it.caption;
    el.caption.textContent = it.caption || "";

    const many = items.length > 1;
    el.prev.hidden = !many;
    el.next.hidden = !many;
    el.prev.disabled = index === 0;
    el.next.disabled = index === items.length - 1;

    // Сохранить, переслать и прочее — только у вложений с id (не у локальных превью).
    [el.saveBtn, el.fwdBtn, el.extBtn, el.moreBtn].forEach((b) => { b.hidden = !it.id; });

    el.strip.querySelectorAll(".lb-thumb").forEach((b, i) => b.classList.toggle("current", i === index));
    const cur = el.strip.children[index];
    if (cur && cur.scrollIntoView) cur.scrollIntoView({ inline: "center", block: "nearest" });
}

function navigate(delta) {
    const i = index + delta;
    if (i < 0 || i >= items.length) return;
    index = i;
    showCurrent();
}

function downloadCurrent() {
    const it = items[index];
    if (!it || !it.id) return;
    svc("SaveAttachmentAs", it.id, it.name || "image.jpg").catch((err) => console.error("lightbox download:", err));
}

// Диалоги живут ниже лайтбокса — сначала закрыть его.
function forwardCurrent() {
    const it = items[index];
    if (!it || !it.id) return;
    closeLightbox();
    showAttachForwardDialog(it);
}

// «Ещё» — то, чего нет в шапке: копировать изображение, показать в папке,
// показать в чате.
function showMore(e) {
    const it = items[index];
    if (!it || !it.id) return;
    const inHeader = ["maximize", "external-link", "download", "forward"];
    const list = buildAttachmentItems(it, { isPhoto: true, imgEl: el.img })
        .filter((x) => !x.separator && inHeader.indexOf(x.icon) < 0);
    if (it.msgId) {
        list.push({ label: t("ctx.show_in_chat"), icon: "message-square", action: () => { const id = it.msgId; closeLightbox(); jumpToMessage(id); } });
    }
    if (!list.length) return;
    const r = e.currentTarget.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.bottom + 4, preventDefault() {}, stopPropagation() {} }, list);
    const menu = document.getElementById("context-menu");
    if (menu) menu.style.left = Math.max(4, r.right - menu.offsetWidth) + "px";
}
