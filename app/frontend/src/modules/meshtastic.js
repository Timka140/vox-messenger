"use strict";

// Live Meshtastic mesh nodes panel + plaintext broadcast chat.
//
// Two surfaces:
//   1. #network-panel bottom section — live nodes list (Phase 3 of M5)
//   2. Pseudo-contact "Mesh broadcast" + #mesh-chat-view — plaintext text
//      messaging on TEXT_MESSAGE_APP=1, interop with vanilla Meshtastic
//      clients (M6.1)
//
// Both react to backend events without polling.

import { svc, flog, state, dom, Events } from "./core.js";
import { t } from "./i18n.js";
import { selectPeer } from "./contacts-list.js";
import { buildDialogOverlay } from "./ui.js";
import { showContextMenu } from "./contextmenu.js";

// Tunables — extracted as named constants so they're greppable and so the
// "why this number" lives in one place rather than buried in handlers.
var STALE_THRESHOLD_SEC = 30 * 60; // node not heard for 30 min → grey dot
var TICK_INTERVAL_MS    = 10 * 1000; // refresh "X сек назад" labels
var MAX_TEXT_BYTES      = 200; // LongFast practical payload before fragmentation
var HISTORY_LOAD_LIMIT  = 200; // mesh chat scrollback
var SCROLL_NEAR_BOTTOM  = 80; // px tolerance for auto-scroll on append

var _nodes = new Map(); // nodeNum -> snapshot
var _enabled = false;
var _reconnecting = false; // device rebooting after admin command; UI stays visible
var _localNodeId = "";
var _tickTimer = 0;

// Список узлов живёт во вкладке Meshtastic настроек (панель «Сеть» удалена).
function isPanelVisible() {
    var tab = document.getElementById("settings-tab-meshtastic");
    var screen = document.getElementById("settings-screen");
    return !!tab && tab.style.display !== "none" && !!screen && screen.style.display !== "none";
}

function classifySignal(snap) {
    if (snap.isLocal) return "local";
    if (!snap.lastSeen || ageSeconds(snap.lastSeen) > STALE_THRESHOLD_SEC) return "stale";
    if (snap.rxSnr >= 0) return "strong";
    if (snap.rxSnr >= -10) return "ok";
    return "weak";
}

function ageSeconds(iso) {
    if (!iso) return Infinity;
    var t = Date.parse(iso);
    if (isNaN(t)) return Infinity;
    return Math.max(0, (Date.now() - t) / 1000);
}

function formatLastSeen(iso) {
    var sec = ageSeconds(iso);
    if (sec < 10) return t("network.mesh.last_seen_now");
    if (sec < 60) return t("network.mesh.last_seen_seconds").replace("{n}", Math.floor(sec));
    if (sec < 3600) return t("network.mesh.last_seen_minutes").replace("{n}", Math.floor(sec / 60));
    return t("network.mesh.last_seen_hours").replace("{n}", Math.floor(sec / 3600));
}

function renderItem(snap) {
    var quality = classifySignal(snap);
    var name = snap.longName || snap.shortName || "(unnamed)";

    var li = document.createElement("li");
    li.className = "mesh-node-item" + (snap.isLocal ? " is-local" : "");
    li.dataset.nodeNum = snap.nodeNum;

    var dot = document.createElement("span");
    dot.className = "mesh-node-signal";
    dot.dataset.quality = quality;
    li.appendChild(dot);

    var nameEl = document.createElement("span");
    nameEl.className = "mesh-node-name";
    nameEl.textContent = name;
    nameEl.title = name;
    li.appendChild(nameEl);

    var idEl = document.createElement("span");
    idEl.className = "mesh-node-id";
    idEl.textContent = snap.nodeId;
    li.appendChild(idEl);

    var meta = document.createElement("span");
    meta.className = "mesh-node-meta";
    var lastEl = document.createElement("span");
    lastEl.className = "mesh-node-lastseen";
    lastEl.textContent = formatLastSeen(snap.lastSeen);
    meta.appendChild(lastEl);
    if (!snap.isLocal) {
        if (snap.rxRssi) {
            var rssiEl = document.createElement("span");
            rssiEl.className = "mesh-node-rssi";
            rssiEl.textContent = snap.rxRssi + " dBm";
            meta.appendChild(rssiEl);
        }
        if (snap.rxSnr) {
            var snrEl = document.createElement("span");
            snrEl.className = "mesh-node-snr";
            snrEl.textContent = "SNR " + snap.rxSnr.toFixed(1);
            meta.appendChild(snrEl);
        }
    }
    if (snap.viaMqtt) {
        var via = document.createElement("span");
        via.className = "mesh-node-via";
        via.textContent = t("network.mesh.via_mqtt");
        meta.appendChild(via);
    }
    li.appendChild(meta);

    return li;
}

function sortNodes(arr) {
    return arr.sort(function (a, b) {
        if (a.isLocal !== b.isLocal) return a.isLocal ? -1 : 1;
        var ta = a.lastSeen ? Date.parse(a.lastSeen) : 0;
        var tb = b.lastSeen ? Date.parse(b.lastSeen) : 0;
        return tb - ta;
    });
}

function renderAll() {
    var section = document.getElementById("mesh-network-section");
    if (!section) return;
    var visible = _enabled || _reconnecting;
    section.style.display = visible ? "" : "none";
    if (!visible) return;

    var localEl = document.getElementById("mesh-net-local-node");
    var countEl = document.getElementById("mesh-net-nodes-count");
    var listEl = document.getElementById("mesh-nodes-list");
    var emptyEl = document.getElementById("mesh-nodes-empty");

    if (localEl) localEl.textContent = _localNodeId || "—";
    if (countEl) countEl.textContent = String(_nodes.size);

    if (!listEl) return;
    var arr = sortNodes(Array.from(_nodes.values()));

    if (arr.length === 0) {
        listEl.innerHTML = "";
        if (emptyEl) emptyEl.style.display = "";
        return;
    }
    if (emptyEl) emptyEl.style.display = "none";

    var frag = document.createDocumentFragment();
    arr.forEach(function (snap) { frag.appendChild(renderItem(snap)); });
    listEl.innerHTML = "";
    listEl.appendChild(frag);
}

function applyStatus(status) {
    if (!status) return;
    var wasVisible = _enabled || _reconnecting;
    _enabled = !!status.enabled;
    _reconnecting = !!status.reconnecting;
    _localNodeId = status.localNodeId || "";
    var nowVisible = _enabled || _reconnecting;

    if (!_enabled) {
        // Во время reconnect _nodes не чистим (хранится последний снимок,
        // визуально остаётся последний список). Чистим только при полном
        // отключении.
        if (!_reconnecting) _nodes.clear();
        stopTick();
    } else if (isPanelVisible()) {
        startTick();
    }
    renderAll();

    var navMesh = document.querySelector('.nav-item[data-nav="mesh"]');
    if (navMesh) navMesh.style.display = nowVisible ? "" : "none";

    if (wasVisible !== nowVisible) {
        if (navMesh && navMesh.classList.contains("active")) {
            if (nowVisible) {
                renderMeshContactsList();
            } else {
                // Tab just disappeared — fall back to Messages so the user
                // isn't stranded on a hidden tab.
                var msgNav = document.querySelector('.nav-item[data-nav="messages"]');
                if (msgNav) msgNav.click();
            }
        }
    }
}

var _meshListRefreshScheduled = false;
function applyNodeUpdate(snap) {
    if (!snap || typeof snap.nodeNum !== "number") return;
    _nodes.set(snap.nodeNum, snap);
    if (snap.isLocal) _localNodeId = snap.nodeId;
    if (isPanelVisible()) renderAll();
    var countEl = document.getElementById("mesh-net-nodes-count");
    if (countEl) countEl.textContent = String(_nodes.size);

    // If the user is on the Mesh nav tab, the contacts list needs to grow
    // when a new node appears. Coalesce bursts via requestAnimationFrame so
    // 18 nodes at startup don't trigger 18 re-renders.
    var navMesh = document.querySelector('.nav-item[data-nav="mesh"]');
    if (navMesh && navMesh.classList.contains("active") && !_meshListRefreshScheduled) {
        _meshListRefreshScheduled = true;
        requestAnimationFrame(function () {
            _meshListRefreshScheduled = false;
            renderMeshContactsList();
        });
    }
}

function startTick() {
    if (_tickTimer) return;
    _tickTimer = setInterval(function () {
        // Refresh only "last seen" labels — no full re-render to keep DOM stable
        var listEl = document.getElementById("mesh-nodes-list");
        if (!listEl || !isPanelVisible()) return;
        Array.from(listEl.children).forEach(function (li) {
            var num = parseInt(li.dataset.nodeNum, 10);
            var snap = _nodes.get(num);
            if (!snap) return;
            var lastEl = li.querySelector(".mesh-node-lastseen");
            if (lastEl) lastEl.textContent = formatLastSeen(snap.lastSeen);
            var dot = li.querySelector(".mesh-node-signal");
            if (dot) dot.dataset.quality = classifySignal(snap);
        });
    }, TICK_INTERVAL_MS);
}

function stopTick() {
    if (_tickTimer) { clearInterval(_tickTimer); _tickTimer = 0; }
}

// --- Bootstrap ---

export async function initMeshtasticPanel() {
    try {
        var status = await svc("Status");
        applyStatus(status);
        if (_enabled) {
            var nodes = await svc("ListNodes");
            (nodes || []).forEach(function (n) { _nodes.set(n.nodeNum, n); });
            renderAll();
        }
    } catch (e) {
        flog("WRN", "frontend.mesh.panel", "init failed: " + e);
    }

    Events.On("meshtastic:status-change", function (ev) {
        applyStatus(ev && ev.data);
    });
    Events.On("meshtastic:node-update", function (ev) {
        applyNodeUpdate(ev && ev.data);
    });

    // When the network panel opens, refresh from authoritative source so the
    // user sees a complete list even if some node-update events were missed,
    // and start the relative-time tick. When it closes, stop the tick to
    // avoid waking idle frames.
    var panel = document.getElementById("settings-tab-meshtastic");
    if (panel) {
        var observer = new MutationObserver(function () {
            if (isPanelVisible()) {
                if (_enabled) {
                    svc("ListNodes").then(function (nodes) {
                        _nodes.clear();
                        (nodes || []).forEach(function (n) { _nodes.set(n.nodeNum, n); });
                        renderAll();
                    }).catch(function (e) {
                        flog("WRN", "frontend.mesh.panel", "ListNodes refresh failed: " + e);
                    });
                    startTick();
                }
            } else {
                stopTick();
            }
        });
        observer.observe(panel, { attributes: true, attributeFilter: ["style"] });
    }

    initMeshChat();
}

// =============================================================================
// Mesh broadcast plaintext chat (M6.1)
// =============================================================================

var _meshUnread = 0;
// Currently-selected mesh chat target: "bcast" or numeric nodeNum string.
// Drives which conversation slice renders in the chat view + which target
// receives outgoing messages.
var _selectedMeshTarget = "bcast";

// Render the mesh contacts list — top "Общий чат" pin + one item per known
// node sorted by signal quality. Replaces the entire #contacts-list when nav
// is on the Mesh tab. Triggered by contacts-list.js dispatching
// vox:render-mesh-list when viewMode === "mesh".
function renderMeshContactsList() {
    if (!dom.contactsList) return;
    dom.contactsList.innerHTML = "";

    if (!_enabled) {
        var empty = document.createElement("div");
        empty.className = "contacts-empty";
        empty.innerHTML = '<i data-lucide="radio-tower" style="width:40px;height:40px;color:#ccc"></i><span>' +
            escapeHtml(t("mesh.list.disabled_hint")) + "</span>";
        dom.contactsList.appendChild(empty);
        if (window.lucide && window.lucide.createIcons) {
            window.lucide.createIcons({ nameAttr: "data-lucide", attrs: {} });
        }
        return;
    }

    // Pinned "Общий чат" item — always first.
    dom.contactsList.appendChild(buildMeshContactItem({
        peerId: "__mesh:bcast__",
        name: t("mesh.list.broadcast"),
        subtitle: t("mesh.list.broadcast_hint"),
        iconLucide: "radio-tower",
        quality: "local",
        unread: _meshUnread, // sum of unread for now; per-node later
    }));

    // Per-node items — sort by signal quality (best first), then alphabetically
    // within the same group. Ranks start at 1 (NOT 0) because `rank || 9`
    // would treat 0 as falsy and demote "strong" to the bottom — classic JS
    // gotcha that bit us once already.
    var qualityRank = { "strong": 1, "ok": 2, "weak": 3, "stale": 4 };
    var arr = Array.from(_nodes.values())
        .filter(function (n) { return !n.isLocal; })
        .sort(function (a, b) {
            var ra = qualityRank[classifySignal(a)] || 9;
            var rb = qualityRank[classifySignal(b)] || 9;
            if (ra !== rb) return ra - rb;
            var na = (a.longName || a.shortName || a.nodeId || "").toLowerCase();
            var nb = (b.longName || b.shortName || b.nodeId || "").toLowerCase();
            return na.localeCompare(nb);
        });

    arr.forEach(function (n) {
        var subtitle = "";
        if (n.rxRssi || n.rxSnr) {
            subtitle = (n.rxRssi ? n.rxRssi + " dBm" : "—") +
                " · " + (n.rxSnr ? "SNR " + n.rxSnr.toFixed(1) : "—");
        } else {
            subtitle = formatLastSeen(n.lastSeen);
        }
        dom.contactsList.appendChild(buildMeshContactItem({
            peerId: "__mesh:" + n.nodeNum + "__",
            name: n.longName || n.shortName || "(unnamed)",
            subtitle: subtitle,
            sub2: n.nodeId,
            quality: classifySignal(n),
            unread: 0, // per-node unread tracking is M6.2+
        }));
    });

    if (window.lucide && window.lucide.createIcons) {
        window.lucide.createIcons({ nameAttr: "data-lucide", attrs: {} });
    }
}

function buildMeshContactItem(opts) {
    var item = document.createElement("div");
    item.className = "contact-item is-mesh" +
        (state.selectedPeerID === opts.peerId ? " active" : "");
    item.dataset.peerId = opts.peerId;

    var avatar = document.createElement("div");
    avatar.className = "contact-avatar";
    if (opts.iconLucide) {
        avatar.innerHTML = '<i data-lucide="' + opts.iconLucide + '" style="width:20px;height:20px;color:#fff"></i>';
    } else {
        avatar.textContent = (opts.name || "??").substring(0, 2).toUpperCase();
    }
    // Tiny corner-dot showing signal quality.
    var dot = document.createElement("span");
    dot.className = "mesh-contact-signal";
    dot.dataset.quality = opts.quality;
    avatar.appendChild(dot);

    var body = document.createElement("div");
    body.className = "contact-body";

    var topRow = document.createElement("div");
    topRow.className = "contact-top-row";
    var nameSpan = document.createElement("span");
    nameSpan.className = "contact-name";
    nameSpan.textContent = opts.name;
    topRow.appendChild(nameSpan);
    if (opts.sub2) {
        var idSpan = document.createElement("span");
        idSpan.className = "contact-time mono";
        idSpan.textContent = opts.sub2;
        topRow.appendChild(idSpan);
    }

    var bottomRow = document.createElement("div");
    bottomRow.className = "contact-bottom-row";
    var preview = document.createElement("span");
    preview.className = "contact-preview";
    preview.textContent = opts.subtitle || "";
    bottomRow.appendChild(preview);
    if (opts.unread > 0) {
        var badge = document.createElement("span");
        badge.className = "unread-badge";
        badge.textContent = opts.unread > 99 ? "99+" : opts.unread;
        bottomRow.appendChild(badge);
    }

    body.appendChild(topRow);
    body.appendChild(bottomRow);
    item.appendChild(avatar);
    item.appendChild(body);

    item.addEventListener("click", function () { selectPeer(opts.peerId); });

    // Правый клик: скопировать nodeId + показать детали.
    // "Открыть чат" не включаем — левый клик уже это делает.
    item.addEventListener("contextmenu", function (e) {
        var items = [];
        if (opts.sub2) {
            items.push({
                label: t("ctx.copy_node_id"),
                icon: "copy",
                action: function () {
                    try { navigator.clipboard.writeText(opts.sub2); } catch (err) {}
                }
            });
        }
        var meshTarget = parseMeshTarget(opts.peerId);
        if (meshTarget && meshTarget !== "bcast") {
            var nodeNum = parseInt(meshTarget, 10);
            var snap = _nodes.get(nodeNum);
            if (snap) {
                items.push({
                    label: t("ctx.mesh_info"),
                    icon: "info",
                    action: function () {
                        var details =
                            (snap.longName || snap.shortName || "(unnamed)") +
                            "\nID: " + (snap.nodeId || "—") +
                            "\nRSSI: " + (snap.rxRssi != null ? snap.rxRssi + " dBm" : "—") +
                            "\nSNR: " + (snap.rxSnr != null ? snap.rxSnr.toFixed(1) : "—") +
                            "\n" + formatLastSeen(snap.lastSeen);
                        showMeshInfo(snap.longName || snap.shortName || "Node", details);
                    }
                });
            }
        }
        if (items.length > 0) showContextMenu(e, items);
    });
    return item;
}

// Parse pseudo-peerID "__mesh:N__" → numeric nodeNum (or "bcast").
function parseMeshTarget(peerID) {
    if (typeof peerID !== "string" || peerID.indexOf("__mesh:") !== 0) return null;
    var inner = peerID.slice(7, -2); // strip "__mesh:" prefix and "__" suffix
    return inner; // "bcast" or numeric string
}

function escapeHtml(s) {
    if (!s) return "";
    return String(s).replace(/[&<>"']/g, function (c) {
        return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
}

// showMeshInfo replaces native alert() — modal with single Close button,
// matches existing app dialog style (buildDialogOverlay from ui.js).
function showMeshInfo(title, message) {
    var body = '<p style="margin: 8px 0 16px;">' + escapeHtml(message) + "</p>";
    buildDialogOverlay(title, body, t("dialog.close") || "Close", null, null);
}

// showMeshConfirm replaces native confirm() — modal with Cancel + Confirm.
// onConfirm receives the overlay so it can dismiss after the action runs.
function showMeshConfirm(title, message, confirmLabel, onConfirm) {
    var body = '<p style="margin: 8px 0 16px;">' + escapeHtml(message) + "</p>";
    buildDialogOverlay(title, body,
        t("dialog.cancel") || "Cancel",
        confirmLabel,
        function (overlay) {
            overlay.remove();
            onConfirm();
        });
}

function formatMsgTime(epochSec) {
    var d = new Date(epochSec * 1000);
    var hh = String(d.getHours()).padStart(2, "0");
    var mm = String(d.getMinutes()).padStart(2, "0");
    return hh + ":" + mm;
}

function renderMeshMsg(rec) {
    var div = document.createElement("div");
    div.className = "mesh-chat-msg" + (rec.direction === "outgoing" ? " outgoing" : "");
    div.dataset.id = rec.id;

    var meta = document.createElement("div");
    meta.className = "mesh-chat-msg-meta";
    var name = document.createElement("span");
    name.className = "mesh-chat-msg-name";
    name.textContent = rec.direction === "outgoing"
        ? (state.myNickname || t("you"))
        : (rec.srcName || rec.srcNodeId || "(unknown)");
    meta.appendChild(name);
    var idSpan = document.createElement("span");
    idSpan.className = "mesh-chat-msg-id";
    idSpan.textContent = rec.srcNodeId;
    meta.appendChild(idSpan);
    if (rec.direction === "incoming" && rec.rxRssi) {
        var sig = document.createElement("span");
        sig.className = "mesh-chat-msg-sig";
        sig.textContent = rec.rxRssi + " dBm · " + (typeof rec.rxSnr === "number" ? rec.rxSnr.toFixed(1) : "—") + " dB";
        meta.appendChild(sig);
    }
    var time = document.createElement("span");
    time.className = "mesh-chat-msg-time";
    time.textContent = formatMsgTime(rec.createdAt);
    meta.appendChild(time);
    div.appendChild(meta);

    var text = document.createElement("div");
    text.className = "mesh-chat-msg-text";
    text.textContent = rec.text;
    div.appendChild(text);

    return div;
}

function renderMeshHistory(records) {
    var list = document.getElementById("mesh-chat-messages");
    if (!list) return;
    list.innerHTML = "";
    if (!records || records.length === 0) {
        var empty = document.createElement("div");
        empty.className = "empty-state";
        empty.textContent = t("mesh.chat.empty");
        list.appendChild(empty);
        return;
    }
    // Records arrive newest-first; reverse to chronological order.
    for (var i = records.length - 1; i >= 0; i--) {
        list.appendChild(renderMeshMsg(records[i]));
    }
    list.scrollTop = list.scrollHeight;
}

function appendMeshMsg(rec) {
    var list = document.getElementById("mesh-chat-messages");
    if (!list) return;
    // Drop any "empty-state" placeholder.
    var empty = list.querySelector(".empty-state");
    if (empty) empty.remove();
    var nearBottom = list.scrollHeight - list.clientHeight - list.scrollTop < SCROLL_NEAR_BOTTOM;
    list.appendChild(renderMeshMsg(rec));
    if (nearBottom) list.scrollTop = list.scrollHeight;
}

function isMeshChatOpen() {
    var v = document.getElementById("mesh-chat-view");
    return v && v.style.display !== "none";
}

// Does this incoming/outgoing record belong to the conversation slice the
// user is currently viewing? Drives append-to-current-chat decisions.
function messageMatchesCurrentTarget(rec) {
    if (_selectedMeshTarget === "bcast") {
        // Broadcast view shows: outgoing-to-bcast + incoming-bcast.
        if (rec.direction === "outgoing") {
            return rec.dstNodeNum === 0xFFFFFFFF || rec.dstNodeNum === 0;
        }
        return rec.dstNodeNum === 0xFFFFFFFF;
    }
    var num = parseInt(_selectedMeshTarget, 10);
    if (rec.direction === "outgoing") return rec.dstNodeNum === num;
    return rec.srcNodeNum === num && rec.dstNodeNum !== 0xFFFFFFFF;
}

async function openMeshChat(ev) {
    var view = document.getElementById("mesh-chat-view");
    if (!view) return;
    var peerID = ev && ev.detail && ev.detail.peerID;
    var target = parseMeshTarget(peerID) || "bcast";
    _selectedMeshTarget = target;

    view.style.display = "flex";
    if (dom.chatHeader) dom.chatHeader.style.display = "none";
    if (dom.messagesDiv) dom.messagesDiv.style.display = "none";
    if (dom.inputArea) dom.inputArea.style.display = "none";

    // Update header to reflect target.
    var titleEl = view.querySelector(".mesh-chat-title h2");
    var subEl = view.querySelector(".mesh-chat-subtitle");
    if (target === "bcast") {
        if (titleEl) titleEl.textContent = t("mesh.list.broadcast");
        if (subEl) subEl.textContent = t("mesh.chat.subtitle");
    } else {
        var snap = _nodes.get(parseInt(target, 10));
        var name = snap ? (snap.longName || snap.shortName) : ("!" + parseInt(target, 10).toString(16).padStart(8, "0"));
        if (titleEl) titleEl.textContent = name;
        if (subEl) subEl.textContent = t("mesh.chat.subtitle_direct");
    }

    try {
        var records;
        if (target === "bcast") {
            records = await svc("ListMeshTextsBroadcast", HISTORY_LOAD_LIMIT);
        } else {
            records = await svc("ListMeshTextsForNode", parseInt(target, 10), HISTORY_LOAD_LIMIT);
        }
        renderMeshHistory(records || []);
    } catch (e) {
        flog("WRN", "frontend.mesh.chat", "load history failed: " + e);
    }

    try {
        await svc("MarkMeshTextsRead");
    } catch (e) {
        flog("WRN", "frontend.mesh.chat", "mark read failed: " + e);
    }
    _meshUnread = 0;
    updateMeshNavBadge();

    var input = document.getElementById("mesh-chat-input");
    if (input) input.focus();
}

function closeMeshChat() {
    var view = document.getElementById("mesh-chat-view");
    if (view) view.style.display = "none";
    // Restore the normal chat's messages container (selectPeer/selectGroup
    // will explicitly set chatHeader and inputArea, but they don't touch
    // messagesDiv — and openMeshChat hid it).
    if (dom.messagesDiv) dom.messagesDiv.style.display = "";
}

// Update the small unread badge on the Mesh nav button. Hidden when 0.
function updateMeshNavBadge() {
    var badge = document.getElementById("mesh-nav-badge");
    if (!badge) return;
    if (_meshUnread > 0) {
        badge.textContent = _meshUnread > 99 ? "99+" : String(_meshUnread);
        badge.style.display = "";
    } else {
        badge.style.display = "none";
    }
}

function bindMeshChatHandlers() {
    var input = document.getElementById("mesh-chat-input");
    var send = document.getElementById("mesh-chat-send");
    var cnt = document.getElementById("mesh-chat-charcount");
    var clr = document.getElementById("mesh-chat-clear");

    function updateCount() {
        if (!input || !cnt) return;
        var n = new TextEncoder().encode(input.value).length;
        var over = n > MAX_TEXT_BYTES;
        if (over) {
            // Spell out the alternative — the limit is protocol-imposed (vanilla
            // Meshtastic can only carry single-packet TEXT_MESSAGE_APP), so we
            // can't fragment here without breaking interop. Encrypted DM has
            // its own framing that does fragment, point the user there.
            cnt.textContent = n + " / " + MAX_TEXT_BYTES + " — " + t("mesh.chat.too_long_hint");
        } else {
            cnt.textContent = n + " / " + MAX_TEXT_BYTES;
        }
        cnt.classList.toggle("over", over);
        if (send) send.disabled = n === 0 || over || !_enabled;
    }
    if (input) {
        input.addEventListener("input", updateCount);
        input.addEventListener("keydown", function (e) {
            if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                doSend();
            }
        });
    }
    if (send) {
        send.addEventListener("click", doSend);
        send.disabled = true;
    }
    if (clr) {
        clr.addEventListener("click", function () {
            showMeshConfirm(
                t("mesh.chat.clear"),
                t("mesh.chat.clear_confirm"),
                t("mesh.chat.clear"),
                async function () {
                    try {
                        await svc("ClearMeshTexts");
                        renderMeshHistory([]);
                    } catch (e) {
                        flog("ERR", "frontend.mesh.chat", "clear failed: " + e);
                        showMeshInfo(t("mesh.chat.clear"), String(e));
                    }
                });
        });
    }
    updateCount();

    async function doSend() {
        if (!input) return;
        var text = input.value.trim();
        if (!text) return;
        if (!_enabled) {
            showMeshInfo(t("mesh.chat.title"), t("mesh.chat.send_disabled"));
            return;
        }
        var bytes = new TextEncoder().encode(text).length;
        if (bytes > MAX_TEXT_BYTES) {
            // Char counter already shows red — no need for a modal too. Inline
            // assist via the counter is enough, just bail.
            return;
        }
        send.disabled = true;
        try {
            if (_selectedMeshTarget === "bcast") {
                await svc("SendBroadcastText", text);
            } else {
                await svc("SendDirectText", parseInt(_selectedMeshTarget, 10), text);
            }
            input.value = "";
            updateCount();
        } catch (err) {
            showMeshInfo(t("mesh.chat.title"), String(err));
            send.disabled = false;
        }
    }
}

function initMeshChat() {
    bindMeshChatHandlers();

    // Mesh nav tab requested a list re-render.
    document.addEventListener("vox:render-mesh-list", renderMeshContactsList);
    document.addEventListener("vox:open-mesh-chat", openMeshChat);
    document.addEventListener("vox:close-mesh-chat", closeMeshChat);

    // Receive new mesh text → if relevant to current view, append; bump unread+badge if not.
    Events.On("meshtastic:text-received", function (ev) {
        var rec = ev && ev.data;
        if (!rec) return;
        if (rec.direction === "incoming" && !isMeshChatOpen()) {
            _meshUnread++;
            updateMeshNavBadge();
        }
        if (isMeshChatOpen() && messageMatchesCurrentTarget(rec)) {
            appendMeshMsg(rec);
            // Only mark-read if there's something new to mark — outgoing echos
            // and broadcast spam would otherwise trigger an SQL UPDATE per packet.
            if (rec.direction === "incoming" && !rec.read) {
                svc("MarkMeshTextsRead").catch(function (e) {
                    flog("WRN", "frontend.mesh.chat", "mark read failed: " + e);
                });
                _meshUnread = 0;
                updateMeshNavBadge();
            }
        }
    });

    // Initial badge sync.
    updateMeshNavBadge();

    // When mesh enable state changes, refresh the contact item and the send button.
    Events.On("meshtastic:status-change", function () {
        var send = document.getElementById("mesh-chat-send");
        var input = document.getElementById("mesh-chat-input");
        if (send && input) {
            var n = new TextEncoder().encode(input.value).length;
            send.disabled = !_enabled || n === 0 || n > 200;
        }
    });

    // Initial unread count on app start
    svc("GetMeshUnreadCount").then(function (n) {
        _meshUnread = n || 0;
        updateMeshNavBadge(); // badge рисовался до ответа — счётчик терялся
    }).catch(function (e) {
        flog("WRN", "frontend.mesh.chat", "GetMeshUnreadCount failed: " + e);
    });
}
