"use strict";

// composer-attach.js — вложения в строке ввода (макет ComposerStates).
//
// Вместо модалки выбора: полоса над полем ввода с миниатюрами 64×64 (× на
// каждой, плитка «+»), справа «3 фото · 8,4 МБ» и сегмент «Как фото / Как
// файлы». Поле ввода становится подписью; Enter / «Отправить» уходят в
// chat.js sendPathAttachments (path-flow: SendFilesByPath / SendGroupFilesByPath).
//
// Источники файлов:
//   - скрепка → меню «Фото» (Ctrl+P) / «Файл» → PickAttachments (нативный диалог);
//   - скрепка → «Снимок экрана» → CaptureScreenshot (временный PNG) → путь;
//   - drag&drop → оверлей #drop-overlay с двумя зонами data-file-drop-target;
//     рантайм Wails отдаёт пути в Go (WindowFilesDropped), тот шлёт
//     vox:files-dropped {paths, target} (main.go);
//   - Ctrl+V картинки → SavePastedImage (временный файл) → путь.
// Полоса одна на всё (правило макета): начало редактирования очищает вложения
// (vox:ui-composer-clear-attach из chat.js), добавление вложений отменяет
// редактирование (vox:ui-edit-cancel).

import { svc, state, dom, Events, formatBytes } from "./core.js";
import { t } from "./i18n.js";
import { showContextMenu } from "./contextmenu.js";
import { showError } from "./toast.js";
import { sendPathAttachments } from "./chat.js";
import { buildDialogOverlay } from "./ui.js";
import { buildDisplayPicker } from "./screenshare.js";

var items = [];       // PreparedAttachment[]: {path, name, size, mime, isImage, previewUrl, width, height}
var asPhoto = true;   // «Как фото» — сжатие и альбом; «Как файлы» — оригиналы
var sending = false;
var dragDepth = 0;

function strip() { return document.getElementById("composer-strip"); }

function chatOpen() {
    var pid = state.selectedPeerID;
    if (typeof pid === "string" && pid.indexOf("__mesh:") === 0) return false;
    return !!(pid || state.selectedGroupID);
}

function composerVisible() {
    return !!dom.inputArea && dom.inputArea.style.display !== "none" &&
        dom.inputArea.offsetParent !== null && !dom.inputArea.classList.contains("select-mode");
}

export function hasAttachments() { return items.length > 0; }

export async function addPaths(paths, mode) {
    if (!paths || paths.length === 0 || !chatOpen()) return;
    document.dispatchEvent(new CustomEvent("vox:ui-edit-cancel"));
    var prepared;
    try {
        prepared = await svc("PrepareAttachments", paths);
    } catch (err) {
        showError(String(err && err.message || err), { title: t("attach.prepare_failed") });
        return;
    }
    if (!prepared || prepared.length === 0) return;
    if (items.length === 0) asPhoto = mode !== "file";
    items = items.concat(prepared);
    render();
    try { dom.messageInput.focus(); } catch (e) { /* поле скрыто */ }
}

export function clearAttachments() {
    var tokens = items.map(function (it) { return it.previewUrl; }).filter(Boolean);
    if (tokens.length) svc("RevokePreviewTokens", tokens).catch(function () {});
    items = [];
    render();
}

function removeAt(i) {
    var it = items.splice(i, 1)[0];
    if (it && it.previewUrl) svc("RevokePreviewTokens", [it.previewUrl]).catch(function () {});
    render();
}

function summary() {
    var photos = items.filter(function (i) { return i.isImage; }).length;
    var total = 0;
    for (var i = 0; i < items.length; i++) total += items[i].size || 0;
    var label = (asPhoto && photos === items.length)
        ? t("composer.n_photos").replace("{n}", String(photos))
        : t("composer.n_files").replace("{n}", String(items.length));
    return label + " · " + formatBytes(total);
}

function render() {
    var s = strip();
    if (!s) return;
    if (items.length === 0) {
        if (s.dataset.mode === "attach") {
            s.hidden = true;
            s.innerHTML = "";
            s.dataset.mode = "";
        }
        dom.inputArea.classList.remove("has-attach");
        dom.messageInput.placeholder = t("composer.placeholder");
        return;
    }
    s.dataset.mode = "attach";
    s.hidden = false;
    s.innerHTML = "";
    dom.inputArea.classList.add("has-attach");

    var thumbs = document.createElement("div");
    thumbs.className = "composer-thumbs";
    items.forEach(function (it, i) {
        var th = document.createElement("div");
        th.className = "composer-thumb";
        th.title = it.name + " · " + formatBytes(it.size || 0);
        if (it.isImage && it.previewUrl) {
            var img = document.createElement("img");
            img.src = it.previewUrl;
            img.alt = it.name || "";
            th.appendChild(img);
        } else {
            var m = /\.([a-z0-9]{1,5})$/i.exec(it.name || "");
            th.classList.add("is-file");
            th.textContent = m ? m[1].toUpperCase().slice(0, 4) : "FILE";
        }
        var x = document.createElement("button");
        x.type = "button";
        x.className = "composer-thumb-x";
        x.title = t("attach.remove");
        x.innerHTML = '<i data-lucide="x"></i>';
        x.addEventListener("click", function (e) { e.stopPropagation(); removeAt(i); });
        th.appendChild(x);
        thumbs.appendChild(th);
    });
    var add = document.createElement("button");
    add.type = "button";
    add.className = "composer-thumb composer-thumb-add";
    add.title = t("attach.modal_add");
    add.innerHTML = '<i data-lucide="plus" class="icon-18"></i>';
    add.addEventListener("click", function () { pick(asPhoto ? "photo" : "file"); });
    thumbs.appendChild(add);
    s.appendChild(thumbs);

    var side = document.createElement("div");
    side.className = "composer-attach-side";
    var sum = document.createElement("div");
    sum.className = "composer-attach-sum";
    sum.textContent = summary();
    side.appendChild(sum);
    if (items.some(function (i) { return i.isImage; })) {
        var seg = document.createElement("div");
        seg.className = "composer-seg";
        [["photo", t("composer.as_photo")], ["file", t("composer.as_files")]].forEach(function (p) {
            var b = document.createElement("button");
            b.type = "button";
            b.textContent = p[1];
            if ((p[0] === "photo") === asPhoto) b.className = "on";
            b.addEventListener("click", function () { asPhoto = p[0] === "photo"; render(); });
            seg.appendChild(b);
        });
        side.appendChild(seg);
    }
    s.appendChild(side);

    var anyImage = items.some(function (i) { return i.isImage; });
    dom.messageInput.placeholder = (asPhoto && anyImage) ? t("composer.caption_photo") : t("composer.caption");
    if (window.lucide) lucide.createIcons({ nodes: [s] });
}

async function send() {
    if (sending || items.length === 0) return;
    sending = true;
    var caption = dom.messageInput.value.trim();
    var out = items.map(function (it) {
        var photo = asPhoto && it.isImage;
        return {
            kind: photo ? "photo" : "file",
            name: it.name,
            size: it.size,
            mime: it.mime,
            path: it.path,
            compress: photo,
            previewUrl: it.previewUrl || null,
        };
    });
    // Токены превью не отзываем: свой пузырь показывает фото по previewUrl,
    // пока не придёт CAS-адрес; протухнут сами (previewTokenTTL в Go).
    items = [];
    render();
    dom.messageInput.value = "";
    dom.messageInput.style.height = "auto";
    try {
        await sendPathAttachments(out, caption);
    } catch (err) {
        showError(String(err && err.message || err), { title: t("attach.send_failed") });
    } finally {
        sending = false;
    }
}

async function pick(mode) {
    if (!chatOpen()) return;
    var paths;
    try {
        paths = await svc("PickAttachments");
    } catch (err) {
        showError(String(err && err.message || err), { title: t("attach.pick_failed") });
        return;
    }
    if (paths && paths.length) addPaths(paths, mode);
}

function showAttachMenu() {
    var btn = document.getElementById("attach-btn");
    if (!btn) return;
    var r = btn.getBoundingClientRect();
    // Меню над кнопкой: три пункта по ~36px + отступы
    showContextMenu({ clientX: r.left, clientY: Math.max(4, r.top - 124), preventDefault: function () {}, stopPropagation: function () {} }, [
        { label: t("attach.photo"), icon: "layout-grid", hint: "Ctrl+P", action: function () { pick("photo"); } },
        { label: t("attach.file"), icon: "file-text", action: function () { pick("file"); } },
        { label: t("attach.screenshot"), icon: "monitor", action: function () { takeScreenshot(); } },
    ]);
}

// takeScreenshot — «Снимок экрана» (фаза 7e, макет ComposerStates): один
// экран — снимок сразу, несколько — выбор плитками с живыми превью, как в
// «Показать экран». Снимок ложится в полосу вложений, как вставленная картинка.
async function takeScreenshot() {
    if (!chatOpen()) return;
    var displays = [];
    try { displays = (await svc("ListDisplays")) || []; } catch (err) { /* ниже — экран по умолчанию */ }
    if (displays.length <= 1) { snapScreen(displays.length ? displays[0].index : 0); return; }

    var fired = false;
    var picker = buildDisplayPicker(displays, function (idx) { done(idx); });
    var dlg = buildDialogOverlay(t("attach.screenshot_title"), "", t("dialog.cancel"), t("attach.screenshot_take"), null,
        { wide: true, onClose: picker.stopThumbs });
    dlg.body.appendChild(picker.el);
    if (window.lucide) lucide.createIcons({ nodes: [picker.el] });
    dlg.confirmBtn.addEventListener("click", function () { done(picker.selected()); });

    async function done(idx) {
        if (fired) return;
        fired = true;
        dlg.overlay.remove();
        await picker.stopThumbs();
        snapScreen(idx);
    }
}

async function snapScreen(idx) {
    // Меню и диалог уже закрыты — даём окну перерисоваться, чтобы их не было на снимке.
    await new Promise(function (resolve) { setTimeout(resolve, 250); });
    try {
        var path = await svc("CaptureScreenshot", idx);
        if (path) addPaths([path], "photo");
    } catch (err) {
        showError(String(err && err.message || err), { title: t("attach.screenshot_failed") });
    }
}

// --- drag&drop: оверлей с двумя зонами поверх области чата ---

function isFileDrag(e) {
    var types = e.dataTransfer && e.dataTransfer.types;
    if (!types) return false;
    for (var i = 0; i < types.length; i++) if (types[i] === "Files") return true;
    return false;
}

function showOverlay(on) {
    var ov = document.getElementById("drop-overlay");
    if (ov) ov.hidden = !on;
}

export function initComposerAttach() {
    var attachBtn = document.getElementById("attach-btn");
    if (attachBtn) {
        attachBtn.addEventListener("click", function (e) {
            e.stopPropagation();
            showAttachMenu();
        });
    }

    // Enter / Esc / «Отправить», пока есть вложения: перехватываем до chat.js
    // (слушатель в фазе перехвата на самом поле срабатывает раньше обычного).
    dom.messageInput.addEventListener("keydown", function (e) {
        if (items.length === 0) return;
        if (e.key === "Enter" && !e.shiftKey) {
            e.preventDefault();
            e.stopImmediatePropagation();
            send();
        } else if (e.key === "Escape") {
            e.preventDefault();
            e.stopImmediatePropagation();
            clearAttachments();
        }
    }, true);
    var sendBtn = document.getElementById("send-btn");
    if (sendBtn) {
        sendBtn.addEventListener("click", function (e) {
            if (items.length === 0) return;
            e.stopImmediatePropagation();
            send();
        }, true);
    }

    // Ctrl+P — «Фото»
    document.addEventListener("keydown", function (e) {
        if (!(e.ctrlKey || e.metaKey) || e.shiftKey || e.altKey) return;
        if (e.code !== "KeyP" && e.key !== "p" && e.key !== "P") return;
        if (!chatOpen() || !composerVisible()) return;
        e.preventDefault();
        pick("photo");
    });

    // Ctrl+V картинки из буфера → временный файл → полоса
    dom.messageInput.addEventListener("paste", function (e) {
        var cd = e.clipboardData;
        if (!cd || !cd.items) return;
        var files = [];
        for (var i = 0; i < cd.items.length; i++) {
            var it = cd.items[i];
            if (it.kind === "file" && it.type && it.type.indexOf("image/") === 0) {
                var f = it.getAsFile();
                if (f) files.push(f);
            }
        }
        if (files.length === 0) return;
        e.preventDefault();
        files.forEach(function (f) {
            var reader = new FileReader();
            reader.onload = function () {
                svc("SavePastedImage", String(reader.result), f.type).then(function (p) {
                    addPaths([p], "photo");
                }).catch(function (err) {
                    showError(String(err && err.message || err), { title: t("attach.prepare_failed") });
                });
            };
            reader.readAsDataURL(f);
        });
    });

    // Drag&drop: показываем оверлей, пока файл над окном; сам drop ловит рантайм Wails
    document.addEventListener("dragenter", function (e) {
        if (!isFileDrag(e) || !chatOpen() || !composerVisible()) return;
        dragDepth++;
        showOverlay(true);
    });
    document.addEventListener("dragleave", function (e) {
        if (!isFileDrag(e)) return;
        dragDepth = Math.max(0, dragDepth - 1);
        if (dragDepth === 0 || !e.relatedTarget) { dragDepth = 0; showOverlay(false); }
    });
    ["drop", "dragend"].forEach(function (ev) {
        document.addEventListener(ev, function () { dragDepth = 0; showOverlay(false); });
    });
    if (Events && Events.On) {
        Events.On("vox:files-dropped", function (ev) {
            var d = (ev && ev.data) || {};
            dragDepth = 0;
            showOverlay(false);
            if (!d.paths || d.paths.length === 0) return;
            addPaths(d.paths, d.target === "drop-as-file" ? "file" : "photo");
        });
    }

    document.addEventListener("vox:ui-chat-changed", clearAttachments);
    document.addEventListener("vox:ui-composer-clear-attach", clearAttachments);
    render();
}
