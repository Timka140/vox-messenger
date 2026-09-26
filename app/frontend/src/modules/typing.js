"use strict";

// typing.js — «печатает…» (макет MessagesText).
//
// Отправка: ввод в поле → SendTyping не чаще раза в 3 с (Go шлёт только по
// уже открытым P2P-каналам, без очереди). Не шлём при редактировании и в
// mesh-чатах. Приём: vox:typing {peerId[, groupId, name]} → строка над полем
// ввода «alina печатает…» для открытого чата; гаснет через 5 с без сигнала и
// сразу, когда от этого человека приходит сообщение.

import { svc, state, dom, Events } from "./core.js";
import { t } from "./i18n.js";

var SEND_EVERY_MS = 3000;
var SHOW_MS = 5000;

var lastSentAt = 0;
var lastSentKey = "";
var typers = new Map(); // "<chat>|<peerId>" → {chat, name, timer}

function chatKey(peerId, groupId) {
    return groupId ? "g:" + groupId : "p:" + peerId;
}

function currentKey() {
    if (state.selectedGroupID) return "g:" + state.selectedGroupID;
    if (state.selectedPeerID) return "p:" + state.selectedPeerID;
    return "";
}

function onInput() {
    var pid = state.selectedPeerID, gid = state.selectedGroupID;
    if (!pid && !gid) return;
    if (!gid && typeof pid === "string" && pid.indexOf("__mesh:") === 0) return;
    if (dom.inputArea.classList.contains("is-editing")) return;
    if (!dom.messageInput.value.trim()) return;
    var key = currentKey();
    var now = Date.now();
    if (key === lastSentKey && now - lastSentAt < SEND_EVERY_MS) return;
    lastSentKey = key;
    lastSentAt = now;
    svc("SendTyping", gid ? "" : pid, gid || "").catch(function () { /* нет канала — не страшно */ });
}

function nameFor(d) {
    if (d.name) return d.name;
    var peers = state.peers || [];
    for (var i = 0; i < peers.length; i++) {
        if (peers[i].id === d.peerId) return peers[i].nickname || peers[i].shortId || "";
    }
    return String(d.peerId || "").substring(0, 8);
}

function render() {
    var el = document.getElementById("typing-indicator");
    if (!el) return;
    var key = currentKey();
    var names = [];
    typers.forEach(function (v) { if (v.chat === key) names.push(v.name); });
    if (names.length === 0) {
        el.hidden = true;
        return;
    }
    var text;
    if (names.length === 1) text = t("typing.one").replace("{name}", names[0]);
    else if (names.length === 2) text = t("typing.two").replace("{a}", names[0]).replace("{b}", names[1]);
    else text = t("typing.many");
    el.querySelector(".typing-text").textContent = text;
    el.hidden = false;
}

function stopTyper(chat, peerId) {
    var id = chat + "|" + peerId;
    var cur = typers.get(id);
    if (!cur) return;
    clearTimeout(cur.timer);
    typers.delete(id);
    render();
}

export function initTyping() {
    if (!dom.messageInput) return;
    dom.messageInput.addEventListener("input", onInput);

    Events.On("vox:typing", function (event) {
        var d = (event && event.data) || {};
        if (!d.peerId || d.peerId === state.myID) return;
        var chat = chatKey(d.peerId, d.groupId);
        var id = chat + "|" + d.peerId;
        var cur = typers.get(id);
        if (cur) clearTimeout(cur.timer);
        typers.set(id, {
            chat: chat,
            name: nameFor(d),
            timer: setTimeout(function () { typers.delete(id); render(); }, SHOW_MS),
        });
        render();
    });

    // Сообщение пришло — человек дописал.
    Events.On("vox:message", function (event) {
        var m = (event && event.data) || {};
        if (m.senderId && !m.outbound) stopTyper("p:" + m.senderId, m.senderId);
    });
    Events.On("vox:group-message", function (event) {
        var d = (event && event.data) || {};
        var m = d.message || {};
        if (d.groupId && m.senderId) stopTyper("g:" + d.groupId, m.senderId);
    });

    document.addEventListener("vox:ui-chat-changed", render);
}
