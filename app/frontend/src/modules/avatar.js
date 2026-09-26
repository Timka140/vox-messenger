"use strict";

// avatar.js — фото профиля и группы поверх буквенных аватаров (фаза 5a).
//
// Все места, где рисуется аватар, по-прежнему ставят буквы и цвет, но через
// applyAvatar: если у id есть фото (хеш из GetAvatarHashes), поверх букв
// ложится <img class="av-img" src="/vox-avatar/<id>?v=<hash>">. Не
// загрузилось — картинка убирается, буквы остаются. Элемент помечается
// data-avatar-id, и по vox:avatar-updated все такие элементы обновляются на
// месте, без перерисовки списков.
//
// id: peerId пира или "g:<groupId>" для группы.

import { svc, flog, colorFromId, Events } from "./core.js";

var hashes = Object.create(null);

function bareId(id) {
    return String(id || "").indexOf("g:") === 0 ? String(id).slice(2) : String(id || "");
}

// initials — «TE» из «Test3b»; letters — сколько букв (сцена и мини-аватары — 1).
export function initials(name, letters) {
    var s = String(name || "?").trim() || "?";
    return s.substring(0, letters || 2).toUpperCase();
}

export function avatarHash(id) {
    return hashes[id] || "";
}

// avatarURL — адрес фото или "" (для notify.html, где своего DOM-хелпера нет).
export function avatarURL(id) {
    var h = hashes[id];
    return h ? "/vox-avatar/" + encodeURIComponent(id) + "?v=" + h : "";
}

function paint(el) {
    var id = el.dataset.avatarId;
    var url = avatarURL(id);
    var img = el.querySelector(":scope > img.av-img");
    if (!url) {
        if (img) img.remove();
        el.classList.remove("has-photo");
        return;
    }
    if (img && img.getAttribute("src") === url) return;
    if (!img) {
        img = document.createElement("img");
        img.className = "av-img";
        img.alt = "";
        img.draggable = false;
        img.onerror = function () {
            img.remove();
            el.classList.remove("has-photo");
        };
        el.appendChild(img);
    }
    img.src = url;
    el.classList.add("has-photo");
}

// applyAvatar — буквы + цвет + фото, если есть.
//   opts.letters — число букв; opts.keepContent — не трогать содержимое
//   (иконка группы остаётся под фото); opts.noColor — не красить фон.
export function applyAvatar(el, id, name, opts) {
    opts = opts || {};
    if (!el) return el;
    el.dataset.avatarId = id;
    if (!opts.keepContent) el.textContent = initials(name, opts.letters);
    if (!opts.noColor) el.style.backgroundColor = colorFromId(bareId(id));
    paint(el);
    return el;
}

// avatarHTML — то же для разметки, собираемой строкой; после вставки в DOM
// вызвать paintAvatars(контейнер). tag — "div" по умолчанию, "span" для
// строчных мини-аватаров (сцена звонка).
export function avatarHTML(id, name, cls, letters, tag) {
    var esc = function (s) {
        return String(s).replace(/[&<>"']/g, function (c) {
            return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
        });
    };
    tag = tag === "span" ? "span" : "div";
    return "<" + tag + ' class="' + esc(cls) + '" data-avatar-id="' + esc(id) + '" style="background:' +
        colorFromId(bareId(id)) + '">' + esc(initials(name, letters)) + "</" + tag + ">";
}

// paintAvatars — дорисовать фото всем [data-avatar-id] внутри root.
export function paintAvatars(root) {
    (root || document).querySelectorAll("[data-avatar-id]").forEach(paint);
}

function repaintId(id) {
    var sel = '[data-avatar-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]';
    document.querySelectorAll(sel).forEach(paint);
}

// loadAvatarHashes — после входа (setup.js showChat) и при смене identity.
export function loadAvatarHashes() {
    return svc("GetAvatarHashes").then(function (m) {
        hashes = Object.create(null);
        Object.keys(m || {}).forEach(function (k) { hashes[k] = m[k]; });
        paintAvatars(document);
        document.dispatchEvent(new CustomEvent("vox:ui-avatars-loaded"));
    }).catch(function (err) {
        flog("WRN", "frontend.avatar", "GetAvatarHashes: " + err);
    });
}

// setLocalAvatarHash — своё фото сменили в этом окне (ответ SetMyAvatar),
// не дожидаясь события из Go.
export function setLocalAvatarHash(id, hash) {
    if (hash) hashes[id] = hash; else delete hashes[id];
    repaintId(id);
    document.dispatchEvent(new CustomEvent("vox:ui-avatar-changed", { detail: { id: id, hash: hash || "" } }));
}

Events.On("vox:avatar-updated", function (ev) {
    var d = ev.data || {};
    if (!d.id) return;
    setLocalAvatarHash(d.id, d.hash);
});
