"use strict";

// avatar-editor.js — редактор фото профиля и группы (макет AvatarEditor).
//
// Круг — область экспорта. Фото двигается перетаскиванием, масштаб — колесом
// или слайдером, поворот на 90°. Справа живые превью в реальных размерах
// приложения: 72 / 44 / 32 / 22. Экспорт — JPEG 256×256 не больше 40 КБ
// (качество подбирается): столько влезает в одно сообщение DataChannel.
// Оригинал никуда не копируется — остаётся у пользователя на диске.
//
// Здесь же карточка «Фото профиля» в Настройки → Профиль.

import { svc, state } from "./core.js";
import { t } from "./i18n.js";
import { buildDialogOverlay } from "./ui.js";
import { showSuccess, showError } from "./toast.js";
import { applyAvatar, avatarHash, setLocalAvatarHash } from "./avatar.js";

const VIEW = 320;           // сторона canvas редактора, px
const CROP = 280;           // диаметр круга в canvas
const OUT = 256;            // сторона экспорта
const MAX_BYTES = 40 * 1024;
const MAX_FILE = 10 * 1024 * 1024;
const PREVIEWS = [72, 44, 32, 22];
const MAX_ZOOM = 4;         // во сколько раз можно приблизить от «вписано»

function pickFile() {
    return new Promise((resolve) => {
        const input = document.createElement("input");
        input.type = "file";
        input.accept = "image/jpeg,image/png";
        input.addEventListener("change", () => resolve((input.files && input.files[0]) || null));
        input.click();
    });
}

// openAvatarEditor — выбор файла → редактор. opts.title; opts.onSave(b64) → Promise;
// opts.file — готовый File/Blob вместо диалога выбора (перетаскивание, тесты).
export async function openAvatarEditor(opts) {
    const file = opts.file || await pickFile();
    if (!file) return;
    if (file.size > MAX_FILE) {
        showError(t("avatar.too_big"));
        return;
    }
    let bmp;
    try {
        bmp = await createImageBitmap(file); // EXIF-поворот Chromium применяет сам
    } catch (err) {
        showError(t("avatar.bad_file"));
        return;
    }
    showEditor(bmp, opts);
}

function showEditor(bmp, opts) {
    const dlg = buildDialogOverlay(opts.title, "", t("dialog.cancel"), t("avatar.save"), null);
    dlg.dialog.classList.add("ae-dialog");
    const body = dlg.body;
    body.innerHTML =
        '<div class="ae">' +
            '<canvas class="ae-canvas" width="' + VIEW + '" height="' + VIEW + '"></canvas>' +
            '<div class="ae-side"><div class="ae-cap"></div><div class="ae-previews"></div><div class="ae-sub"></div></div>' +
        '</div>' +
        '<div class="ae-controls">' +
            '<i data-lucide="zoom-out" class="icon-16"></i>' +
            '<input type="range" class="ae-zoom" min="0" max="100" step="1" value="0">' +
            '<i data-lucide="zoom-in" class="icon-16"></i>' +
            '<button type="button" class="ae-btn ae-rotate"><i data-lucide="rotate-cw" class="icon-16"></i><span></span></button>' +
            '<button type="button" class="ae-btn ae-reset"><i data-lucide="undo-2" class="icon-16"></i><span></span></button>' +
        '</div>' +
        '<div class="ae-hint"></div>';
    body.querySelector(".ae-cap").textContent = t("avatar.preview");
    body.querySelector(".ae-sub").textContent = t("avatar.preview_sub");
    body.querySelector(".ae-rotate span").textContent = t("avatar.rotate");
    body.querySelector(".ae-reset span").textContent = t("avatar.reset");
    body.querySelector(".ae-hint").textContent = t("avatar.hint");
    const zoomEl = body.querySelector(".ae-zoom");
    zoomEl.title = t("avatar.zoom");
    const canvas = body.querySelector(".ae-canvas");
    const ctx = canvas.getContext("2d");
    const previews = PREVIEWS.map((s) => {
        const c = document.createElement("canvas");
        c.width = s * 2;   // 2× для чёткости
        c.height = s * 2;
        c.style.width = s + "px";
        c.style.height = s + "px";
        c.className = "ae-prev";
        body.querySelector(".ae-previews").appendChild(c);
        return c;
    });
    if (window.lucide) lucide.createIcons({ nodes: [body] });

    const st = { rot: 0, scale: 1, x: 0, y: 0 };
    const dims = () => (st.rot % 180 === 0 ? [bmp.width, bmp.height] : [bmp.height, bmp.width]);
    const minScale = () => CROP / Math.min(dims()[0], dims()[1]);

    // clamp — фото всегда закрывает круг целиком.
    function clamp() {
        const [w, h] = dims();
        const min = minScale();
        st.scale = Math.min(Math.max(st.scale, min), min * MAX_ZOOM);
        const mx = Math.max(0, (w * st.scale - CROP) / 2);
        const my = Math.max(0, (h * st.scale - CROP) / 2);
        st.x = Math.min(mx, Math.max(-mx, st.x));
        st.y = Math.min(my, Math.max(-my, st.y));
    }

    // paint — фото в canvas g с центром круга (cx, cy); k — px на единицу редактора.
    function paint(g, cx, cy, k) {
        g.save();
        g.translate(cx + st.x * k, cy + st.y * k);
        g.rotate((st.rot * Math.PI) / 180);
        g.scale(st.scale * k, st.scale * k);
        g.drawImage(bmp, -bmp.width / 2, -bmp.height / 2);
        g.restore();
    }

    function draw() {
        ctx.fillStyle = "#1b1e25";
        ctx.fillRect(0, 0, VIEW, VIEW);
        paint(ctx, VIEW / 2, VIEW / 2, 1);
        // затемнение вне круга
        ctx.save();
        ctx.fillStyle = "rgba(0, 0, 0, 0.55)";
        ctx.beginPath();
        ctx.rect(0, 0, VIEW, VIEW);
        ctx.arc(VIEW / 2, VIEW / 2, CROP / 2, 0, Math.PI * 2, true);
        ctx.fill("evenodd");
        ctx.strokeStyle = "rgba(255, 255, 255, 0.85)";
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(VIEW / 2, VIEW / 2, CROP / 2, 0, Math.PI * 2);
        ctx.stroke();
        ctx.restore();
        previews.forEach((c) => {
            const g = c.getContext("2d");
            g.fillStyle = "#ffffff";
            g.fillRect(0, 0, c.width, c.height);
            paint(g, c.width / 2, c.height / 2, c.width / CROP);
        });
        zoomEl.value = String(Math.round(((st.scale / minScale() - 1) / (MAX_ZOOM - 1)) * 100));
    }

    function reset() {
        st.rot = 0;
        st.x = 0;
        st.y = 0;
        st.scale = minScale();
        clamp();
        draw();
    }

    // перетаскивание
    let drag = null;
    canvas.addEventListener("pointerdown", (e) => {
        drag = { x: e.clientX, y: e.clientY, sx: st.x, sy: st.y };
        canvas.setPointerCapture(e.pointerId);
    });
    canvas.addEventListener("pointermove", (e) => {
        if (!drag) return;
        st.x = drag.sx + (e.clientX - drag.x);
        st.y = drag.sy + (e.clientY - drag.y);
        clamp();
        draw();
    });
    const endDrag = () => { drag = null; };
    canvas.addEventListener("pointerup", endDrag);
    canvas.addEventListener("pointercancel", endDrag);
    canvas.addEventListener("wheel", (e) => {
        e.preventDefault();
        st.scale *= Math.exp(-e.deltaY * 0.0015);
        clamp();
        draw();
    }, { passive: false });
    zoomEl.addEventListener("input", () => {
        st.scale = minScale() * (1 + ((MAX_ZOOM - 1) * Number(zoomEl.value)) / 100);
        clamp();
        draw();
    });
    body.querySelector(".ae-rotate").addEventListener("click", () => {
        st.rot = (st.rot + 90) % 360;
        clamp();
        draw();
    });
    body.querySelector(".ae-reset").addEventListener("click", reset);

    dlg.confirmBtn.addEventListener("click", async () => {
        dlg.confirmBtn.disabled = true;
        try {
            const b64 = await exportJPEG(paint);
            await opts.onSave(b64);
            dlg.overlay.remove();
            showSuccess(t("avatar.saved"));
        } catch (err) {
            dlg.confirmBtn.disabled = false;
            showError(String((err && err.message) || err), { title: opts.title });
        }
    });

    reset();
}

// exportJPEG — 256×256, качество от 0.9 вниз, пока файл не станет ≤ 40 КБ.
async function exportJPEG(paint) {
    const out = document.createElement("canvas");
    out.width = OUT;
    out.height = OUT;
    const g = out.getContext("2d");
    g.fillStyle = "#ffffff"; // прозрачность PNG → белый фон (в JPEG альфы нет)
    g.fillRect(0, 0, OUT, OUT);
    paint(g, OUT / 2, OUT / 2, OUT / CROP);
    let blob = null;
    for (let q = 0.9; q >= 0.3; q -= 0.1) {
        blob = await new Promise((resolve) => out.toBlob(resolve, "image/jpeg", q));
        if (blob && blob.size <= MAX_BYTES) break;
    }
    if (!blob || blob.size > MAX_BYTES) throw new Error(t("avatar.bad_file"));
    const dataUrl = await new Promise((resolve, reject) => {
        const r = new FileReader();
        r.onload = () => resolve(r.result);
        r.onerror = () => reject(r.error);
        r.readAsDataURL(blob);
    });
    return String(dataUrl).split(",")[1];
}

// --- Своё фото и фото группы ---

function myName() {
    return state.myNickname || state.myShortID || "?";
}

export function editMyAvatar() {
    openAvatarEditor({
        title: t("avatar.title"),
        onSave: async (b64) => {
            const hash = await svc("SetMyAvatar", b64);
            setLocalAvatarHash(state.myID, hash);
        },
    });
}

export function editGroupAvatar(groupId) {
    openAvatarEditor({
        title: t("avatar.group_title"),
        onSave: async (b64) => {
            const hash = await svc("SetGroupAvatar", groupId, b64);
            setLocalAvatarHash("g:" + groupId, hash);
        },
    });
}

// renderSettingsAvatar — карточка «Фото профиля» и аватар в панели пользователя.
export function renderSettingsAvatar() {
    if (!state.myID) return;
    const box = document.getElementById("settings-avatar");
    if (box) applyAvatar(box, state.myID, myName());
    const rm = document.getElementById("settings-avatar-remove");
    if (rm) rm.hidden = !avatarHash(state.myID);
}

function removeMyAvatar() {
    svc("ClearMyAvatar").then(() => {
        setLocalAvatarHash(state.myID, "");
        showSuccess(t("avatar.removed"));
    }).catch((err) => showError(String((err && err.message) || err)));
}

["settings-avatar-upload", "settings-avatar-cam"].forEach((id) => {
    const b = document.getElementById(id);
    if (b) b.addEventListener("click", editMyAvatar);
});
const removeBtn = document.getElementById("settings-avatar-remove");
if (removeBtn) removeBtn.addEventListener("click", removeMyAvatar);

document.addEventListener("vox:ui-avatars-loaded", renderSettingsAvatar);
document.addEventListener("vox:ui-avatar-changed", (e) => {
    if (e.detail && e.detail.id === state.myID) renderSettingsAvatar();
});
