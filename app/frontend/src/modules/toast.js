"use strict";

// toast.js — In-app snackbar/toast system.
//
// Замена для native OS Notify в случаях когда сообщение должно увидеть
// окно приложения (ошибки send, validation, success-confirmations).
// Native Notify оставляем для фоновых событий (новое сообщение пока окно
// не сфокусировано, входящий звонок при минимизации и т.п.).
//
// Базовое использование:
//   import { showToast, showError, showSuccess, showInfo, dismiss } from "./toast.js";
//   showError("Send failed", { title: "Vox", duration: 6000 });
//   showToast({ type: "info", title: "Updated", message: "...", actions: [...] });
//
// Action:
//   { label: "Retry", primary: true, onClick: function() { ... } }

var container = null;
var nextId = 1;
var toasts = new Map(); // id → { el, timer }

function ensureContainer() {
    if (container && document.body.contains(container)) return container;
    container = document.createElement("div");
    container.className = "vox-toast-container";
    document.body.appendChild(container);
    return container;
}

function iconFor(type) {
    switch (type) {
        case "error":   return "alert-triangle";
        case "success": return "check-circle";
        case "info":
        default:        return "info";
    }
}

// showToast — основная функция. Возвращает id для ручного dismiss.
// opts: { type: "info"|"error"|"success", title?, message?, duration?, actions? }
//   duration: ms; 0 = sticky (только ручной close)
export function showToast(opts) {
    opts = opts || {};
    var type = opts.type || "info";
    var duration = opts.duration === undefined ? (type === "error" ? 6000 : 4000) : opts.duration;
    var id = nextId++;

    var c = ensureContainer();
    var el = document.createElement("div");
    el.className = "vox-toast is-" + type;

    var iconWrap = document.createElement("div");
    iconWrap.className = "vox-toast-icon";
    var icon = document.createElement("i");
    icon.setAttribute("data-lucide", iconFor(type));
    icon.className = "icon-18";
    iconWrap.appendChild(icon);
    el.appendChild(iconWrap);

    var body = document.createElement("div");
    body.className = "vox-toast-body";
    if (opts.title) {
        var title = document.createElement("div");
        title.className = "vox-toast-title";
        title.textContent = opts.title;
        body.appendChild(title);
    }
    if (opts.message) {
        var msg = document.createElement("div");
        msg.className = "vox-toast-message";
        msg.textContent = opts.message;
        body.appendChild(msg);
    }
    if (opts.actions && opts.actions.length > 0) {
        var actionsRow = document.createElement("div");
        actionsRow.className = "vox-toast-actions";
        opts.actions.forEach(function (a) {
            var btn = document.createElement("button");
            btn.type = "button";
            btn.className = "vox-toast-action" + (a.primary ? " is-primary" : "");
            btn.textContent = a.label;
            btn.addEventListener("click", function () {
                try { if (a.onClick) a.onClick(); } finally {
                    if (a.keepOpen !== true) dismiss(id);
                }
            });
            actionsRow.appendChild(btn);
        });
        body.appendChild(actionsRow);
    }
    el.appendChild(body);

    var close = document.createElement("button");
    close.type = "button";
    close.className = "vox-toast-close";
    close.setAttribute("aria-label", "Close");
    close.innerHTML = '<i data-lucide="x" class="icon-14"></i>';
    close.addEventListener("click", function () { dismiss(id); });
    el.appendChild(close);

    c.appendChild(el);
    if (window.lucide) lucide.createIcons({ nodes: [el] });

    var timer = duration > 0 ? setTimeout(function () { dismiss(id); }, duration) : null;
    toasts.set(id, { el: el, timer: timer });

    return id;
}

export function dismiss(id) {
    var t = toasts.get(id);
    if (!t) return;
    if (t.timer) { clearTimeout(t.timer); t.timer = null; }
    toasts.delete(id);
    t.el.classList.add("is-leaving");
    var done = false;
    function finalize() {
        if (done) return;
        done = true;
        if (t.el.parentNode) t.el.parentNode.removeChild(t.el);
    }
    t.el.addEventListener("animationend", function (e) {
        if (e.animationName === "vox-toast-out") finalize();
    });
    // Fallback на случай если animationend не сработает (display:none и т.п.)
    setTimeout(finalize, 400);
}

export function dismissAll() {
    var ids = Array.from(toasts.keys());
    ids.forEach(dismiss);
}

// Удобные обёртки.

export function showError(message, opts) {
    return showToast(Object.assign({ title: undefined }, opts || {}, {
        type: "error",
        message: message,
    }));
}

export function showSuccess(message, opts) {
    return showToast(Object.assign({}, opts || {}, {
        type: "success",
        message: message,
    }));
}

export function showInfo(message, opts) {
    return showToast(Object.assign({}, opts || {}, {
        type: "info",
        message: message,
    }));
}
