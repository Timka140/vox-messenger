// screen-decoder.js — декодер одного потока демонстрации экрана в <canvas>.
//
// Вынесен из screenshare.js без изменения логики: H.264 (Annex-B) через
// WebCodecs VideoDecoder (аппаратный в WebView2), JPEG (fallback-кодек бэка)
// через createImageBitmap. Модуль не импортирует core.js, чтобы тот же код
// мог работать во втором окне (screen-view.html, фаза 1b).
//
// createScreenDecoder({ canvas, onNeedKeyframe, onFrame })
//   .push(payload, fid, w, h) → true, если кадр принят к декодированию
//   .suspend(bool)            → приостановить/возобновить (при возобновлении
//                               ждём keyframe и просим PLI)
//   .close()

const MAX_DECODE_QUEUE = 4; // глубже — декодер не успевает, ждём keyframe

export function base64ToBytes(b64) {
    const binStr = atob(b64);
    const bytes = new Uint8Array(binStr.length);
    for (let i = 0; i < binStr.length; i++) bytes[i] = binStr.charCodeAt(i);
    return bytes;
}

async function decodeBase64ToImageBitmap(b64, mime) {
    const blob = new Blob([base64ToBytes(b64)], { type: mime });
    return await createImageBitmap(blob);
}

export function createScreenDecoder(opts) {
    const canvas = opts.canvas;
    const ctx = canvas.getContext("2d");
    const onNeedKeyframe = opts.onNeedKeyframe || function () {};
    const onFrame = opts.onFrame || function () {};

    const d = {
        decoder: null,      // WebCodecs VideoDecoder (h264)
        waitingKey: true,   // ждём keyframe (старт / после ошибки)
        pending: 0,         // кадров в очереди декодера
        decoding: false,    // JPEG: createImageBitmap в полёте
        suspended: false,
    };

    function ensureDecoder() {
        if (d.decoder && d.decoder.state !== "closed") return d.decoder;
        if (typeof VideoDecoder === "undefined") return null;
        const decoder = new VideoDecoder({
            output: (frame) => {
                try {
                    if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
                        canvas.width = frame.displayWidth;
                        canvas.height = frame.displayHeight;
                    }
                    ctx.drawImage(frame, 0, 0);
                } finally {
                    frame.close();
                }
                d.pending = Math.max(0, d.pending - 1);
                onFrame();
            },
            error: (err) => {
                console.warn("[SCREEN] decoder error, waiting keyframe:", err && err.message || err);
                resetDecoder();
                onNeedKeyframe();
            },
        });
        // Annex-B поток (без description) — декодер сам берёт SPS/PPS из keyframe.
        decoder.configure({ codec: "avc1.42E01E", optimizeForLatency: true, hardwareAcceleration: "prefer-hardware" });
        d.decoder = decoder;
        d.waitingKey = true;
        d.pending = 0;
        return decoder;
    }

    function resetDecoder() {
        if (d.decoder) {
            try { d.decoder.close(); } catch (e) { /* ignore */ }
        }
        d.decoder = null;
        d.waitingKey = true;
        d.pending = 0;
    }

    function pushH264(payload, fid) {
        const decoder = ensureDecoder();
        if (!decoder) return false;
        const key = !!payload.keyframe;
        if (d.waitingKey && !key) {
            onNeedKeyframe();
            return false;
        }
        // Декодер отстаёт (слабая машина / всплеск) — дропаем до следующего
        // keyframe, иначе задержка растёт секундами. Delta-кадры дропать
        // поштучно нельзя: ссылаются на предыдущие.
        if (!key && d.pending > MAX_DECODE_QUEUE) {
            d.waitingKey = true;
            onNeedKeyframe();
            return false;
        }
        d.waitingKey = false;
        try {
            d.pending++;
            decoder.decode(new EncodedVideoChunk({
                type: key ? "key" : "delta",
                timestamp: fid * 33333, // мкс; важна только монотонность
                data: base64ToBytes(payload.data),
            }));
            return true;
        } catch (err) {
            console.warn("[SCREEN] decode() threw:", err && err.message || err);
            resetDecoder();
            onNeedKeyframe();
            return false;
        }
    }

    function pushJPEG(payload, w, h) {
        // Если предыдущий кадр ещё декодится — дропаем новый.
        if (d.decoding) return false;
        d.decoding = true;
        decodeBase64ToImageBitmap(payload.data, "image/jpeg")
            .then((bitmap) => {
                ctx.drawImage(bitmap, 0, 0, w, h);
                bitmap.close && bitmap.close();
                onFrame();
            })
            .catch((err) => {
                console.warn("screen frame decode failed:", err);
            })
            .finally(() => {
                d.decoding = false;
            });
        return true;
    }

    return {
        push(payload, fid, w, h) {
            if (d.suspended) return false;
            if ((payload.codec || "jpeg") === "h264") return pushH264(payload, fid);
            return pushJPEG(payload, w, h);
        },
        suspend(flag) {
            const next = !!flag;
            if (next === d.suspended) return;
            d.suspended = next;
            if (next) {
                resetDecoder(); // освобождаем аппаратный декодер, пока не смотрим
            } else {
                d.waitingKey = true;
                onNeedKeyframe();
            }
        },
        get suspended() { return d.suspended; },
        close() {
            resetDecoder();
        },
    };
}
