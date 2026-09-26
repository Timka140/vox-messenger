// peer-volume.js — локальная громкость / mute участников «для меня».
//
// Один владелец на голосовой канал и звонок (раньше канал хранил в
// offcanvas.js по localStorage, звонок — в calls.js в памяти, и у сцены
// было два источника правды). scope — groupId голосового канала или
// "call:<callId>" для звонка. Хранится в localStorage под ключом
// voice_volumes_<scope>: { [peerId]: vol%, ["mute_"+peerId]: true }.
// Бэкенд получает SetPeerVolume(peerId, 0.0–2.0).

import { svc, flog } from "./core.js";

function key(scope) {
    return "voice_volumes_" + scope;
}

function load(scope) {
    try {
        return JSON.parse(localStorage.getItem(key(scope)) || "{}");
    } catch (e) { return {}; }
}

function save(scope, data) {
    try { localStorage.setItem(key(scope), JSON.stringify(data)); } catch (e) { /* storage недоступен */ }
}

export function getPeerVolume(scope, peerId) {
    const v = load(scope)[peerId];
    return v != null ? v : 100;
}

export function setPeerVolume(scope, peerId, vol) {
    const data = load(scope);
    if (vol === 100) delete data[peerId]; else data[peerId] = vol;
    save(scope, data);
    svc("SetPeerVolume", peerId, vol / 100).catch(() => {});
}

export function isPeerMuted(scope, peerId) {
    return load(scope)["mute_" + peerId] === true;
}

export function setPeerMuted(scope, peerId, muted) {
    const data = load(scope);
    if (muted) data["mute_" + peerId] = true; else delete data["mute_" + peerId];
    save(scope, data);
    svc("SetPeerVolume", peerId, muted ? 0 : getPeerVolume(scope, peerId) / 100).catch(() => {});
}

// applyGroupVolumes — при входе в канал вернуть бэкенду сохранённые громкости.
export function applyGroupVolumes(scope) {
    const data = load(scope);
    const keys = Object.keys(data);
    flog("INF", "voice", "applyGroupVolumes scope=" + scope + " entries=" + keys.length);
    for (const k of keys) {
        if (k.indexOf("mute_") === 0) {
            if (data[k]) svc("SetPeerVolume", k.substring(5), 0).catch(() => {});
        } else {
            svc("SetPeerVolume", k, data[k] / 100).catch(() => {});
        }
    }
}
