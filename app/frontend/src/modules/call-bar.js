// call-bar.js — полоска 32px над шапкой чужого чата, пока идёт звонок или мы
// сидим в голосовом канале другой группы (макет OtherChat). Показывает
// «● имя · mm:ss · кто демонстрирует · [К звонку]». Сама сессия живёт в
// stage.js: он шлёт vox:ui-session-changed после каждого рендера и
// vox:ui-stage-tick раз в секунду, пока сессия есть.

import { state } from "./core.js";
import { t } from "./i18n.js";
import { getActiveSession, stageContext, formatSessionTimer } from "./stage.js";
import { peerLabel } from "./screenshare.js";
import { selectPeer, selectGroup, showMessagesList, showGroupsList } from "./contacts-list.js";

const el = {};

function render() {
    const session = getActiveSession();
    const visible = !!session && !stageContext();
    el.bar.hidden = !visible;
    if (!visible) return;
    el.name.textContent = session.name;
    el.timer.textContent = formatSessionTimer(session);
    const sharers = session.sharers || [];
    if (sharers.length === 0) {
        el.sharerWrap.hidden = true;
    } else {
        el.sharerWrap.hidden = false;
        let name = peerLabel(sharers[0]);
        if (sharers.length > 1) name = t("callbar.sharers_n").replace("{name}", name).replace("{n}", String(sharers.length - 1));
        el.sharer.textContent = t("stage.sharing_by").replace("{name}", name);
    }
}

function tick() {
    if (el.bar.hidden) return;
    const session = getActiveSession();
    if (session) el.timer.textContent = formatSessionTimer(session);
}

// goToSession — открыть чат-владелец звонка / группу голосового канала.
function goToSession() {
    const session = getActiveSession();
    if (!session) return;
    const key = session.key || "";
    if (key.indexOf("group:") === 0) {
        const gid = key.slice(6);
        const g = (state.groups || []).find((x) => x.groupId === gid);
        showGroupsList();
        selectGroup(gid, g || { groupId: gid, name: session.name });
        return;
    }
    if (key.indexOf("peer:") === 0) {
        const pid = key.slice(5);
        const p = (state.peers || []).find((x) => x.id === pid);
        showMessagesList();
        selectPeer(pid, p || { id: pid, nickname: session.name });
        return;
    }
    // Ad-hoc групповой звонок без чата-владельца: сцена занимает пустую область
    // чата — снимаем выделение.
    document.dispatchEvent(new CustomEvent("vox:ui-deselect-chat"));
}

export function initCallBar() {
    el.bar = document.getElementById("call-bar");
    if (!el.bar) return;
    el.name = document.getElementById("call-bar-name");
    el.timer = document.getElementById("call-bar-timer");
    el.sharerWrap = document.getElementById("call-bar-sharer-wrap");
    el.sharer = document.getElementById("call-bar-sharer");
    document.getElementById("call-bar-goto").addEventListener("click", goToSession);
    document.addEventListener("vox:ui-session-changed", render);
    document.addEventListener("vox:ui-goto-session", goToSession); // user-bar (calls.js)
    document.addEventListener("vox:ui-stage-tick", tick);
    render();
}
