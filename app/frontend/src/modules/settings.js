"use strict";

import { applyThemePreference, svc, flog, state, dom, Events, showScreen, svcQuiet } from "./core.js";
import { t, setLocale, getLocale } from "./i18n.js";
import { checkForUpdates, rollback as doRollback, isAutoCheckEnabled, setAutoCheckEnabled, lastUpdateCheck, pendingUpdateVersion } from "./updates.js";
import { showInfo } from "./toast.js";
import { buildDialogOverlay, escapeHtml, confirmDialog } from "./ui.js";
import { applyAvatar } from "./avatar.js";
import { renderSettingsAvatar } from "./avatar-editor.js";
import { isInviteLink, joinGroupByLink } from "./group-join.js";
import { loadBlocked, unblockPeer, blockedCount } from "./blocked.js";
import { CHAT_COLORS, getBubbleColor, setBubbleColor } from "./chat-prefs.js";
import { getSoundPrefs, setSoundVolume, setSoundEvent, playSoundPreview } from "./sounds.js";

// escapeAttr — экранирование для вставки в HTML-атрибут (кавычки + угловые).
function escapeAttr(s) {
    return escapeHtml(s);
}

// Modal info dialog — replaces native alert() for the mesh-related controls.
// Returns the overlay element so callers that need to auto-dismiss (e.g. the
// WiFi "rebooting" modal which closes itself when the device comes back) can
// hold a handle. Callers that don't care can ignore the return value.
function _meshSettingsInfo(title, message) {
    var body = '<p style="margin: 8px 0 16px;">' +
        String(message).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;") +
        "</p>";
    buildDialogOverlay(title, body, t("dialog.close") || "Close", null, null);
    var overlays = document.querySelectorAll(".backup-overlay");
    return overlays.length ? overlays[overlays.length - 1] : null;
}

// --- Settings ---
export async function showSettings() {
    showScreen("settings");
    renderSettingsAvatar();
    try {
        var nick = await svc("GetNickname");
        dom.nicknameInput.value = nick;
        dom.settingsMyTag.textContent = (nick || "user") + "#" + state.myShortID;
    } catch (e) {}
    try {
        var sharing = await svc("GetContactSharing");
        document.getElementById("contact-sharing-toggle").checked = sharing;
    } catch (e) {}
    var current = localStorage.getItem("vox-theme") || "light"; // light | dark | system
    document.querySelectorAll(".theme-card").forEach(function (c) {
        c.classList.toggle("active", c.dataset.theme === current);
    });
    // Ссылка-контакт (вкладка Профиль) и ID узла (О программе)
    loadConnConfig();
    var aboutId = document.getElementById("about-node-id");
    if (aboutId) aboutId.textContent = state.myID || "—";
    var platEl = document.getElementById("about-platform");
    if (platEl) platEl.textContent = (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "";
    // Set language selector to current locale
    var langSelect = document.getElementById("language-select");
    if (langSelect) langSelect.value = getLocale();

    var notifySelect = document.getElementById("notify-mode-select");
    if (notifySelect) {
        svc("GetSetting", "notify_mode").then(function (v) {
            notifySelect.value = v || "custom";
        }).catch(function () { notifySelect.value = "custom"; });
    }
    renderBubbleSwatches();
    syncSoundControls();

    // Updates section
    try {
        var cur = await svc("GetCurrentVersion");
        var curEl = document.getElementById("settings-update-current");
        if (curEl) curEl.textContent = cur || "dev";
        var latest = await svc("GetLatestRelease");
        var latestEl = document.getElementById("settings-update-latest");
        if (latestEl) latestEl.textContent = (latest && latest.version) ? latest.version : t("update.no_updates");
        var canRollback = await svc("CanRollback");
        var rbBtn = document.getElementById("settings-update-rollback");
        if (rbBtn) rbBtn.style.display = canRollback ? "" : "none";
        var autoChk = document.getElementById("settings-update-auto-check");
        if (autoChk) autoChk.checked = isAutoCheckEnabled();
    } catch (e) {}
    renderUpdateStatus(null);

    flog("INF", "frontend.settings", "opened");
}

export function closeSettings() {
    if (micTesting) stopMicTest();
    // «Слушать себя» — только пока открыты Настройки (без наушников свист).
    if (micMonitorToggle && micMonitorToggle.checked) {
        micMonitorToggle.checked = false;
        svcQuiet("SetMicMonitor", false);
    }
    showScreen("main");
}

dom.settingsClose.addEventListener("click", closeSettings);

// Language selector
var langSelect = document.getElementById("language-select");
if (langSelect) {
    langSelect.addEventListener("change", function () {
        setLocale(langSelect.value);
    });
}


// openSettingsTab — настройки сразу на вкладке (меню звонка: «Настройки звука»).
export function openSettingsTab(tab) {
    showSettings();
    switchSettingsTab(tab);
}

// Settings tab switching — 7 вкладок (макет SettingsMap). Разделы грузятся лениво.
export function switchSettingsTab(tab) {
    document.querySelectorAll("#settings-screen .st-tabs li[data-tab]").forEach(function (l) { l.classList.remove("active"); });
    var li = document.querySelector('#settings-screen .st-tabs li[data-tab="' + tab + '"]');
    if (li) li.classList.add("active");
    document.querySelectorAll(".settings-tab").forEach(function (t) { t.style.display = "none"; });
    var target = document.getElementById("settings-tab-" + tab);
    if (target) target.style.display = "";
    if (tab === "network") { loadSignalServers(); loadTURNConfig(); loadNetworkStatus(); }
    if (tab === "advanced") { loadSFUSettings(); loadGroupRelaySettings(); loadOpusSettings(); loadVADSettings(); refreshBitrateOnce(); }
    if (tab === "sound") loadAudioDevices();
    if (tab === "meshtastic") loadMeshtasticSettings();
    if (tab === "about") { renderUpdateStatus(null); loadVerboseLogging(); }
}
document.querySelectorAll("#settings-screen .st-tabs li[data-tab]").forEach(function (li) {
    li.addEventListener("click", function () { switchSettingsTab(li.dataset.tab); });
});

// Индикатор автосохранения в шапке: любое изменение select/checkbox/radio/range
// в настройках сохраняется сразу (обработчики ниже) — показываем «Изменения
// сохранены» на 2 с. Текстовые поля с явными кнопками (TURN, серверы) не считаем.
var _savedTimer = null;
export function showSaved() {
    var el = document.getElementById("settings-saved");
    if (!el) return;
    el.hidden = false;
    if (_savedTimer) clearTimeout(_savedTimer);
    _savedTimer = setTimeout(function () { el.hidden = true; }, 2000);
}
dom.settingsScreen.addEventListener("change", function (e) {
    var el = e.target;
    if (!el) return;
    var auto = el.tagName === "SELECT" || (el.tagName === "INPUT" && (el.type === "checkbox" || el.type === "radio" || el.type === "range")) || el.id === "nickname-input";
    if (auto && !el.closest(".st-manual")) showSaved();
});

// Свёрнутые блоки (нейрофильтр, Wi-Fi/BT/экран платы)
document.querySelectorAll("#settings-screen .st-collapse-head[data-collapse]").forEach(function (head) {
    head.addEventListener("click", function () {
        var body = document.getElementById(head.dataset.collapse);
        if (!body) return;
        body.hidden = !body.hidden;
        var wrap = head.closest(".st-collapse");
        if (wrap) wrap.classList.toggle("open", !body.hidden);
    });
});

// Копирование тега / ID узла
var tagCopyBtn = document.getElementById("settings-tag-copy");
if (tagCopyBtn) tagCopyBtn.addEventListener("click", function () {
    try { navigator.clipboard.writeText(dom.settingsMyTag.textContent); } catch (e) {}
});
["net-status-nodeid-copy", "about-node-id-copy"].forEach(function (id) {
    var b = document.getElementById(id);
    if (b) b.addEventListener("click", function () { try { navigator.clipboard.writeText(state.myID || ""); } catch (e) {} });
});

// Статус-полоса вкладки «Сеть»: сигнал-сервер, пиры онлайн, TURN, ID узла.
async function loadNetworkStatus() {
    var dot = document.getElementById("net-status-signal-dot");
    var sig = document.getElementById("net-status-signal");
    var peersEl = document.getElementById("net-status-peers");
    var turnEl = document.getElementById("net-status-turn");
    var idEl = document.getElementById("net-status-nodeid");
    if (idEl) idEl.textContent = state.myShortID ? state.myShortID + "…" : "—";
    var online = 0;
    for (var i = 0; i < (state.peers || []).length; i++) if (state.peers[i].online) online++;
    if (peersEl) peersEl.textContent = String(online);
    try {
        var servers = (await svc("GetSignalServers")) || [];
        var connected = servers.some(function (x) { return x.online; });
        if (sig) sig.textContent = connected ? t("settings.network.connected") : t("settings.network.disconnected");
        if (dot) dot.className = "st-dot" + (connected ? " on" : "");
    } catch (e) {
        if (sig) sig.textContent = t("settings.network.disconnected");
    }
    try {
        var source = await svc("GetICEServersSource");
        if (turnEl) turnEl.textContent = source === "manual" ? t("settings.network.turn_manual") : (source === "server" ? t("settings.network.turn_server") : "—");
    } catch (e) { if (turnEl) turnEl.textContent = "—"; }
}

// Nickname — автосохранение по change (blur / Enter), кнопки нет
dom.nicknameInput.addEventListener("keydown", function (e) { if (e.key === "Enter") dom.nicknameInput.blur(); });
dom.nicknameInput.addEventListener("change", async function () {
    var nick = dom.nicknameInput.value.trim();
    try {
        await svc("SetNickname", nick);
        state.myNickname = nick || "";
        var displayName = nick || state.myShortID;
        dom.usernameEl.textContent = displayName;
        applyAvatar(dom.userAvatar, state.myID, displayName);
        renderSettingsAvatar();
        dom.settingsMyTag.textContent = (nick || "user") + "#" + state.myShortID;
        flog("INF", "frontend.settings", "nickname saved, len=" + (nick || "").length);
    } catch (err) {
        console.error("save nickname:", err);
    }
});

// Contact Sharing Toggle
document.getElementById("contact-sharing-toggle").addEventListener("change", async function () {
    try {
        await svc("SetContactSharing", this.checked);
        flog("INF", "frontend.settings", "contact sharing: " + this.checked);
    } catch (err) {
        console.error("set contact sharing:", err);
    }
});

// Notifications mode: custom (своё окно) | system | off — читает NotifyService
var notifyModeSelect = document.getElementById("notify-mode-select");
if (notifyModeSelect) {
    notifyModeSelect.addEventListener("change", function () {
        svc("SaveSetting", "notify_mode", notifyModeSelect.value).catch(function () {});
        flog("INF", "frontend.settings", "notify mode: " + notifyModeSelect.value);
    });
}

// Theme: light | dark | system («Как в системе» — по prefers-color-scheme, core.js)
document.querySelectorAll(".theme-card").forEach(function (card) {
    card.addEventListener("click", function () {
        var theme = card.dataset.theme;
        applyThemePreference(theme);
        localStorage.setItem("vox-theme", theme);
        showSaved();
        // Дублируем в БД чтобы main.go при следующем старте мог поставить
        // нативный фон окна под выбранную тему — иначе при resize виден
        // белый/тёмный нативный фон webview2 пока CSS не перерисовался.
        svc("SaveSetting", "ui_theme", theme).catch(function () {});
        document.querySelectorAll(".theme-card").forEach(function (c) {
            c.classList.toggle("active", c.dataset.theme === theme);
        });
        flog("INF", "frontend.settings", "theme changed: " + theme);
    });
});

// «Цвет исходящих» (фаза 7c): «как в теме» + палитра цвета чата (chat-prefs.js).
var bubbleSwatches = document.getElementById("bubble-swatches");
function renderBubbleSwatches() {
    if (!bubbleSwatches) return;
    var cur = getBubbleColor().toLowerCase();
    bubbleSwatches.innerHTML = "";
    [{ hex: "", name: "color.theme" }].concat(CHAT_COLORS).forEach(function (c) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "bubble-swatch" + (c.hex ? "" : " bubble-swatch-theme") + (cur === c.hex.toLowerCase() ? " active" : "");
        if (c.hex) b.style.background = c.hex;
        b.title = t(c.name);
        b.setAttribute("aria-label", t(c.name));
        b.addEventListener("click", function () {
            setBubbleColor(c.hex);
            renderBubbleSwatches();
            showSaved();
            flog("INF", "frontend.settings", "bubble color: " + (c.hex || "theme"));
        });
        bubbleSwatches.appendChild(b);
    });
}

// «Звуки событий» (фаза 7c, sounds.js): громкость + выключатели событий.
var soundVolumeSlider = document.getElementById("sound-volume");
var soundVolumeLabel = document.getElementById("sound-volume-label");
function syncSoundControls() {
    var p = getSoundPrefs();
    if (soundVolumeSlider) soundVolumeSlider.value = p.volume;
    if (soundVolumeLabel) soundVolumeLabel.textContent = p.volume + "%";
    document.querySelectorAll("#settings-screen [data-sound-event]").forEach(function (cb) {
        cb.checked = !!p.events[cb.dataset.soundEvent];
    });
}
if (soundVolumeSlider) {
    soundVolumeSlider.addEventListener("input", function () {
        if (soundVolumeLabel) soundVolumeLabel.textContent = soundVolumeSlider.value + "%";
    });
    soundVolumeSlider.addEventListener("change", function () {
        setSoundVolume(soundVolumeSlider.value);
        playSoundPreview();
    });
}
document.querySelectorAll("#settings-screen [data-sound-event]").forEach(function (cb) {
    cb.addEventListener("change", function () {
        setSoundEvent(cb.dataset.soundEvent, cb.checked);
        flog("INF", "frontend.settings", "sound " + cb.dataset.soundEvent + ": " + cb.checked);
    });
});

async function loadSignalServers() {
    try {
        var servers = await svc("GetSignalServers");
        var customSeeds = await svc("GetCustomSeeds");
        dom.signalServersBody.innerHTML = "";

        // Collect discovered addresses for matching
        var discoveredAddrs = {};
        if (servers) {
            for (var i = 0; i < servers.length; i++) {
                discoveredAddrs[servers[i].addr] = true;
            }
        }

        var hasRows = false;

        // Show discovered servers
        if (servers && servers.length > 0) {
            for (var i = 0; i < servers.length; i++) {
                var s = servers[i];
                var tr = document.createElement("tr");
                var statusClass = s.online ? "online" : "offline";
                // Check if this is a custom seed (show remove button)
                var isCustom = customSeeds && customSeeds.indexOf(s.addr) !== -1;
                // Данные приходят с signal-сервера/discovery — недоверенны при
                // компрометации сервера; экранируем addr/pubkey/shortId.
                var removeCell = isCustom
                    ? '<td><button class="settings-icon-btn remove-signal-btn" data-addr="' + escapeAttr(s.addr) + '" title="' + t("settings.signal.remove") + '"><i data-lucide="x" class="icon-14"></i></button></td>'
                    : '<td></td>';
                var pubkeyCell = s.pubkey
                    ? '<td class="mono" title="' + escapeAttr(s.pubkey) + '">' + escapeHtml(s.pubkey.substring(0, 16)) + '…</td>'
                    : '<td class="mono" style="color:#b22;">' + t("settings.signal.pubkey_tofu") + '</td>';
                tr.innerHTML =
                    '<td><span class="status-dot ' + statusClass + '"></span></td>' +
                    '<td class="mono">' + escapeHtml(s.shortId) + '</td>' +
                    '<td class="mono">' + escapeHtml(s.addr) + '</td>' +
                    pubkeyCell +
                    removeCell;
                dom.signalServersBody.appendChild(tr);
                hasRows = true;
            }
        }

        // Show custom seeds not yet discovered
        if (customSeeds) {
            for (var j = 0; j < customSeeds.length; j++) {
                if (!discoveredAddrs[customSeeds[j]]) {
                    var tr = document.createElement("tr");
                    tr.innerHTML =
                        '<td><span class="status-dot offline"></span></td>' +
                        '<td class="mono">—</td>' +
                        '<td class="mono">' + escapeHtml(customSeeds[j]) + '</td>' +
                        '<td class="mono">—</td>' +
                        '<td><button class="settings-icon-btn remove-signal-btn" data-addr="' + escapeAttr(customSeeds[j]) + '" title="' + t("settings.signal.remove") + '"><i data-lucide="x" class="icon-14"></i></button></td>';
                    dom.signalServersBody.appendChild(tr);
                    hasRows = true;
                }
            }
        }

        if (!hasRows) {
            var tr = document.createElement("tr");
            tr.innerHTML = '<td colspan="5" class="table-empty">' + t("settings.signal.empty") + '</td>';
            dom.signalServersBody.appendChild(tr);
        }

        // Re-render lucide icons for new buttons
        if (window.lucide) lucide.createIcons();

        // Bind remove buttons
        dom.signalServersBody.querySelectorAll(".remove-signal-btn").forEach(function (btn) {
            btn.addEventListener("click", async function () {
                var addr = this.getAttribute("data-addr");
                try {
                    await svc("RemoveSignalServer", addr);
                    loadSignalServers();
                } catch (err) {
                    flog("ERR", "frontend.settings", "remove server: " + err);
                }
            });
        });
    } catch (err) {
        flog("ERR", "frontend.settings", "load servers: " + err);
    }
    // Load connection URI for export
    try {
        var uri = await svc("GetMyContactURI");
        document.getElementById("conn-config-output").value = uri;
    } catch (e) {}
}

// Add signal server button
document.getElementById("add-signal-btn").addEventListener("click", async function () {
    var input = document.getElementById("add-signal-input");
    var pubkeyInput = document.getElementById("add-signal-pubkey");
    var addr = input.value.trim();
    var pubkey = pubkeyInput.value.trim().toLowerCase();
    if (!addr) return;
    if (pubkey && !/^[0-9a-f]{64}$/.test(pubkey)) {
        alert(t("settings.signal.pubkey_invalid"));
        return;
    }
    try {
        await svc("AddSignalServer", addr, pubkey);
        input.value = "";
        pubkeyInput.value = "";
        loadSignalServers();
    } catch (err) {
        flog("ERR", "frontend.settings", "add server: " + err);
    }
});
function triggerAddSignal(e) {
    if (e.key === "Enter") document.getElementById("add-signal-btn").click();
}
document.getElementById("add-signal-input").addEventListener("keydown", triggerAddSignal);
document.getElementById("add-signal-pubkey").addEventListener("keydown", triggerAddSignal);

// Reconnect to signal server
document.getElementById("reconnect-signal-btn").addEventListener("click", async function () {
    var btn = this;
    btn.disabled = true;
    btn.textContent = t("settings.signal.reconnecting");
    try {
        await svc("Disconnect");
        await svc("Connect", ":0", ":0", "");
        btn.textContent = t("settings.signal.reconnected");
        setTimeout(function () {
            btn.textContent = t("settings.signal.reconnect");
            btn.disabled = false;
        }, 2000);
    } catch (err) {
        btn.textContent = t("settings.signal.reconnect_error");
        flog("ERR", "frontend.settings", "reconnect: " + err);
        setTimeout(function () {
            btn.textContent = t("settings.signal.reconnect");
            btn.disabled = false;
        }, 3000);
    }
});

// Connection config copy
var connCopyBtn = document.getElementById("conn-config-copy-btn");
if (connCopyBtn) {
    connCopyBtn.addEventListener("click", function () {
        navigator.clipboard.writeText(document.getElementById("conn-config-output").value);
        var btn = this;
        var orig = btn.innerHTML;
        btn.textContent = t("settings.connconfig.copied");
        setTimeout(function () { btn.innerHTML = orig; }, 1500);
    });
}

// Connection config import
var connImportBtn = document.getElementById("conn-config-import-btn");
if (connImportBtn) {
    connImportBtn.addEventListener("click", async function () {
        var input = document.getElementById("conn-config-input");
        var uri = input.value.trim();
        // Ссылка-приглашение в группу — не контакт: отдельный путь вступления.
        if (isInviteLink(uri)) {
            joinGroupByLink(uri);
            input.value = "";
            return;
        }
        if (!uri || !uri.startsWith("vox://")) {
            input.focus();
            return;
        }
        var statusEl = document.getElementById("conn-config-status");
        try {
            await svc("AddContact", uri);
            statusEl.textContent = t("settings.connconfig.import_success");
            statusEl.style.color = "";
            statusEl.style.display = "block";
            input.value = "";
            flog("INF", "frontend.settings", "connection config imported");
        } catch (err) {
            statusEl.textContent = t("settings.connconfig.import_error") + ": " + err;
            statusEl.style.color = "var(--danger)";
            statusEl.style.display = "block";
            flog("ERR", "frontend.settings", "import failed: " + err);
        }
    });
}

// Export for events module
export { loadSignalServers, loadTURNConfig };

// --- Audio Devices ---
var micSelect = document.getElementById("mic-select");
var speakerSelect = document.getElementById("speaker-select");
var micGainSlider = document.getElementById("mic-gain");
var micGainLabel = document.getElementById("mic-gain-label");
var micTestBtn = document.getElementById("mic-test-btn");
var vuMeterFill = document.getElementById("vu-meter-fill");
var voiceVolumeSlider = document.getElementById("voice-master-volume");
var voiceVolumeLabel = document.getElementById("voice-master-volume-label");
var nsSelect = document.getElementById("ns-select");
var dfnStrengthField = document.getElementById("dfn-strength-field");
var dfnStrengthSlider = document.getElementById("dfn-strength");
var dfnStrengthLabel = document.getElementById("dfn-strength-label");
var nsDesc = document.getElementById("ns-desc");
var micTesting = false;
var vuTimer = null;

async function loadAudioDevices() {
    try {
        var mics = await svc("GetMicrophones");
        micSelect.innerHTML = "";
        var savedMic = await svc("GetSelectedMicrophone");
        if (mics) {
            for (var i = 0; i < mics.length; i++) {
                var opt = document.createElement("option");
                opt.value = mics[i].index;
                opt.textContent = mics[i].name + (mics[i].isDefault ? t("settings.device.default") : "");
                // бэк отдаёт строку; не выбран (или после сброса) — системный
                if (String(mics[i].index) === String(savedMic) || (mics[i].isDefault && (!savedMic || String(savedMic) === "-1"))) opt.selected = true;
                micSelect.appendChild(opt);
            }
        }

        var speakers = await svc("GetSpeakers");
        speakerSelect.innerHTML = "";
        var savedSpeaker = "";
        try { savedSpeaker = await svc("GetSelectedSpeaker"); } catch (e) {}
        if (speakers) {
            for (var i = 0; i < speakers.length; i++) {
                var opt = document.createElement("option");
                opt.value = speakers[i].index;
                opt.textContent = speakers[i].name + (speakers[i].isDefault ? t("settings.device.default") : "");
                if (String(speakers[i].index) === savedSpeaker || (speakers[i].isDefault && (!savedSpeaker || savedSpeaker === "-1"))) opt.selected = true;
                speakerSelect.appendChild(opt);
            }
        }

        // Mic gain
        try {
            var gain = await svc("GetMicGain");
            var gpct = Math.round((gain || 1.0) * 100);
            micGainSlider.value = gpct;
            micGainLabel.textContent = gpct + "%";
        } catch (e) {}

        // Master voice volume
        try {
            var masterVol = await svc("GetMasterVoiceVolume");
            var pct = Math.round((masterVol || 1.0) * 100);
            voiceVolumeSlider.value = pct;
            voiceVolumeLabel.textContent = pct + "%";
        } catch (e) {}

        // Noise suppression
        try {
            var ns = await svc("GetNoiseSuppression");
            if (ns) {
                nsSelect.value = String(ns.level);
                // Если RNNoise не вкомпилирована — пометить опции 2/3 как "недоступно"
                if (!ns.rnnoiseAvailable) {
                    var unavail = " — " + t("settings.ns.unavailable");
                    var optRnn = nsSelect.querySelector('option[value="2"]');
                    var optDeep = nsSelect.querySelector('option[value="3"]');
                    if (optRnn && optRnn.textContent.indexOf("—") < 0) optRnn.textContent += unavail;
                    if (optDeep && optDeep.textContent.indexOf("—") < 0) optDeep.textContent += unavail;
                }
                // DFN strength slider: загружаем значение + показываем только когда ns=Deep
                var strength = (ns.dfnStrength > 0) ? ns.dfnStrength : 100;
                dfnStrengthSlider.value = strength;
                dfnStrengthLabel.textContent = strength + "%";
                dfnStrengthField.style.display = (ns.level === 3) ? "" : "none";
            }
        } catch (e) {}

        // Neural NS: единый селектор Off/FE/GTCRN + подопции для активного режима.
        try {
            var neuralAvail = await svc("GetNeuralAvailable");
            flog("INF", "frontend.settings", "Neural NS available=" + neuralAvail);
            var nField = document.getElementById("neural-field");
            if (nField) nField.style.display = neuralAvail ? "" : "none";
            if (neuralAvail) {
                var mode = await svc("GetNeuralMode");
                var modeRadio = document.querySelector('input[name="neural-mode"][value="' + mode + '"]');
                if (modeRadio) modeRadio.checked = true;
                syncNeuralOptionsVisibility(mode);

                // FE options
                var feSize = await svc("GetFastEnhancerSize");
                var feRadio = document.querySelector('input[name="fe-size"][value="' + feSize + '"]');
                if (feRadio) feRadio.checked = true;
                var feMix = await svc("GetFastEnhancerMix");
                var fePct = Math.round((feMix != null ? feMix : 1.0) * 100);
                var feMixEl = document.getElementById("fe-mix");
                if (feMixEl) feMixEl.value = fePct;
                var feMixLbl = document.getElementById("fe-mix-label");
                if (feMixLbl) feMixLbl.textContent = fePct + "%";

                // GTCRN options
                var gVariant = await svc("GetGTCRNVariant");
                var gRadio = document.querySelector('input[name="gtcrn-variant"][value="' + gVariant + '"]');
                if (gRadio) gRadio.checked = true;
                var gMix = await svc("GetGTCRNMix");
                var gPct = Math.round((gMix != null ? gMix : 1.0) * 100);
                var gMixEl = document.getElementById("gtcrn-mix");
                if (gMixEl) gMixEl.value = gPct;
                var gMixLbl = document.getElementById("gtcrn-mix-label");
                if (gMixLbl) gMixLbl.textContent = gPct + "%";
            }
        } catch (e) {
            flog("ERR", "frontend.settings", "Neural NS init failed: " + (e && e.message ? e.message : e));
        }

    } catch (err) {
        console.error("load audio devices:", err);
    }
}

// --- Neural NS handlers (Off / FastEnhancer / GTCRN) ---

function syncNeuralOptionsVisibility(mode) {
    var feOpts = document.getElementById("fe-options");
    var gOpts = document.getElementById("gtcrn-options");
    if (feOpts) feOpts.style.display = mode === "fe" ? "" : "none";
    if (gOpts) gOpts.style.display = mode === "gtcrn" ? "" : "none";
}

document.querySelectorAll('input[name="neural-mode"]').forEach(function (r) {
    r.addEventListener("change", function () {
        if (r.checked) {
            svcQuiet("SetNeuralMode", r.value);
            syncNeuralOptionsVisibility(r.value);
        }
    });
});

document.querySelectorAll('input[name="fe-size"]').forEach(function (r) {
    r.addEventListener("change", function () {
        if (r.checked) svcQuiet("SetFastEnhancerSize", r.value);
    });
});

var _feMixTimeout = null;
var _feMix = document.getElementById("fe-mix");
if (_feMix) {
    _feMix.addEventListener("input", function () {
        var pct = parseInt(_feMix.value);
        var lbl = document.getElementById("fe-mix-label");
        if (lbl) lbl.textContent = pct + "%";
        if (_feMixTimeout) clearTimeout(_feMixTimeout);
        _feMixTimeout = setTimeout(function () {
            svcQuiet("SetFastEnhancerMix", pct / 100);
        }, 150);
    });
}

document.querySelectorAll('input[name="gtcrn-variant"]').forEach(function (r) {
    r.addEventListener("change", function () {
        if (r.checked) svcQuiet("SetGTCRNVariant", r.value);
    });
});

var _gtcrnMixTimeout = null;
var _gtcrnMix = document.getElementById("gtcrn-mix");
if (_gtcrnMix) {
    _gtcrnMix.addEventListener("input", function () {
        var pct = parseInt(_gtcrnMix.value);
        var lbl = document.getElementById("gtcrn-mix-label");
        if (lbl) lbl.textContent = pct + "%";
        if (_gtcrnMixTimeout) clearTimeout(_gtcrnMixTimeout);
        _gtcrnMixTimeout = setTimeout(function () {
            svcQuiet("SetGTCRNMix", pct / 100);
        }, 150);
    });
}

async function loadSFUSettings() {
    try {
        var sfuEnabled = await svc("GetSFUEnabled");
        if (sfuToggle) sfuToggle.checked = sfuEnabled;
        var sfuForce = await svc("GetSFUForce");
        if (sfuForceToggle) sfuForceToggle.checked = sfuForce;
        var sfuThresh = await svc("GetSFUThreshold");
        if (sfuThresholdInput && sfuThresh > 0) sfuThresholdInput.value = sfuThresh;
        updateSFUFieldsVisibility();
    } catch (e) {}
}

// --- SFU toggle ---
var sfuToggle = document.getElementById("sfu-toggle");
var sfuThresholdField = document.getElementById("sfu-threshold-field");
var sfuThresholdInput = document.getElementById("sfu-threshold");
var sfuForceField = document.getElementById("sfu-force-field");
var sfuForceToggle = document.getElementById("sfu-force");

function updateSFUFieldsVisibility() {
    var on = sfuToggle && sfuToggle.checked;
    if (sfuThresholdField) sfuThresholdField.style.display = on ? "" : "none";
    if (sfuForceField) sfuForceField.style.display = on ? "" : "none";
}

if (sfuToggle) {
    sfuToggle.addEventListener("change", function () {
        svcQuiet("SetSFUEnabled", sfuToggle.checked);
        updateSFUFieldsVisibility();
    });
}
if (sfuThresholdInput) {
    sfuThresholdInput.addEventListener("change", function () {
        var n = parseInt(sfuThresholdInput.value);
        if (n >= 2) svcQuiet("SetSFUThreshold", n);
    });
}
if (sfuForceToggle) {
    sfuForceToggle.addEventListener("change", function () {
        svcQuiet("SetSFUForce", sfuForceToggle.checked);
    });
}


// --- Opus DTX toggle + bitrate readout ---
var opusDtxToggle = document.getElementById("opus-dtx-toggle");
var opusBitrateReadout = document.getElementById("opus-bitrate-readout");

async function loadOpusSettings() {
    if (!opusDtxToggle) return;
    try {
        var dtx = await svc("GetOpusDTX");
        opusDtxToggle.checked = !!dtx;
    } catch (e) {}
}

if (opusDtxToggle) {
    opusDtxToggle.addEventListener("change", function () {
        svcQuiet("SetOpusDTX", opusDtxToggle.checked);
    });
}


function formatBytes(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / 1024 / 1024).toFixed(2) + " MB";
}

// Применить snapshot битрейта к UI. Вызывается из обработчика
// vox:voice-bitrate (push-event с Go) или вручную при открытии вкладки.
export function applyBitrateSnapshot(b) {
    if (!opusBitrateReadout) return;
    if (!b || !b.active) {
        opusBitrateReadout.textContent = t("settings.opus.bitrate_idle");
        return;
    }
    var inst = b.instantKbps.toFixed(1);
    var avg = b.avgKbps.toFixed(1);
    var total = formatBytes(b.totalBytes);
    var sec = b.sessionSec.toFixed(0);
    opusBitrateReadout.textContent = inst + " kbps (avg " + avg + ") · " + total + " in " + sec + "s";
}

// Один запрос initial-snapshot'а при открытии вкладки. Дальнейшие обновления
// приходят push-event'ом vox:voice-bitrate (см. events.js).
async function refreshBitrateOnce() {
    try {
        var b = await svc("GetVoiceBitrate");
        applyBitrateSnapshot(b);
    } catch (e) {}
}

// --- VAD (Voice Activity Detection) toggle + threshold slider ---
var vadToggle = document.getElementById("vad-toggle");
var vadThresholdField = document.getElementById("vad-threshold-field");
var vadThresholdSlider = document.getElementById("vad-threshold");
var vadThresholdLabel = document.getElementById("vad-threshold-label");
var _vadThresholdTimeout = null;

async function loadVADSettings() {
    if (!vadToggle) return;
    try {
        var enabled = await svc("GetVADEnabled");
        vadToggle.checked = !!enabled;
        var thr = await svc("GetVADThreshold");
        if (vadThresholdSlider && thr > 0) {
            var pct = Math.round(thr * 100);
            vadThresholdSlider.value = pct;
            if (vadThresholdLabel) vadThresholdLabel.textContent = pct + "%";
        }
        updateVADFieldsVisibility();
    } catch (e) {}
}

function updateVADFieldsVisibility() {
    if (vadThresholdField) vadThresholdField.style.display = (vadToggle && vadToggle.checked) ? "" : "none";
}

if (vadToggle) {
    vadToggle.addEventListener("change", function () {
        svcQuiet("SetVADEnabled", vadToggle.checked);
        updateVADFieldsVisibility();
    });
}

if (vadThresholdSlider) {
    vadThresholdSlider.addEventListener("input", function () {
        var pct = parseInt(vadThresholdSlider.value);
        if (vadThresholdLabel) vadThresholdLabel.textContent = pct + "%";
        if (_vadThresholdTimeout) clearTimeout(_vadThresholdTimeout);
        _vadThresholdTimeout = setTimeout(function () {
            svcQuiet("SetVADThreshold", pct / 100);
        }, 300);
    });
}

// --- Group text relay toggle ---
var groupRelayForce = document.getElementById("group-relay-force");
var groupRelayThreshold = document.getElementById("group-relay-threshold");

async function loadGroupRelaySettings() {
    try {
        var force = await svc("GetGroupForceRelay");
        if (groupRelayForce) groupRelayForce.checked = !!force;
        var thr = await svc("GetGroupRelayThreshold");
        if (groupRelayThreshold && thr >= 2) groupRelayThreshold.value = thr;
    } catch (e) {}
}

if (groupRelayForce) {
    groupRelayForce.addEventListener("change", function () {
        svcQuiet("SetGroupForceRelay", groupRelayForce.checked);
    });
}
if (groupRelayThreshold) {
    groupRelayThreshold.addEventListener("change", function () {
        var n = parseInt(groupRelayThreshold.value);
        if (n >= 2) svcQuiet("SetGroupRelayThreshold", n);
    });
}

nsSelect.addEventListener("change", function () {
    var level = parseInt(nsSelect.value);
    svcQuiet("SetNoiseSuppression", level);
    // Показываем DFN strength только на ns=3 (Deep)
    dfnStrengthField.style.display = (level === 3) ? "" : "none";
    // Если идёт mic test — перезапустить чтобы сразу услышать разницу.
    // В activeGroupVoice применяется на лету через volatile флаг в C — рестарт не нужен.
    if (micTesting) {
        stopMicTest();
        setTimeout(startMicTest, 100);
    }
});

var _dfnStrengthTimeout = null;
dfnStrengthSlider.addEventListener("input", function () {
    var pct = parseInt(dfnStrengthSlider.value);
    dfnStrengthLabel.textContent = pct + "%";
    if (_dfnStrengthTimeout) clearTimeout(_dfnStrengthTimeout);
    _dfnStrengthTimeout = setTimeout(function () {
        svcQuiet("SetDFNStrength", pct);
    }, 150);
});

var _voiceVolumeTimeout = null;
voiceVolumeSlider.addEventListener("input", function () {
    var pct = parseInt(voiceVolumeSlider.value);
    voiceVolumeLabel.textContent = pct + "%";
    if (_voiceVolumeTimeout) clearTimeout(_voiceVolumeTimeout);
    _voiceVolumeTimeout = setTimeout(function () {
        svcQuiet("SetMasterVoiceVolume", pct / 100.0);
    }, 150);
});

micSelect.addEventListener("change", function () {
    var micIdx = parseInt(micSelect.value, 10);
    if (isNaN(micIdx)) return;
    svcQuiet("SetMicrophone", micIdx);
    if (micTesting) {
        stopMicTest();
        startMicTest();
    }
});

speakerSelect.addEventListener("change", function () {
    var spkIdx = parseInt(speakerSelect.value, 10);
    if (isNaN(spkIdx)) return;
    svcQuiet("SetSpeaker", spkIdx);
});

var _gainTimeout = null;
micGainSlider.addEventListener("input", function () {
    var pct = parseInt(micGainSlider.value);
    micGainLabel.textContent = pct + "%";
    if (_gainTimeout) clearTimeout(_gainTimeout);
    _gainTimeout = setTimeout(function () {
        svcQuiet("SetMicGain", pct / 100.0);
    }, 150);
});

micTestBtn.addEventListener("click", function () {
    if (micTesting) {
        stopMicTest();
    } else {
        startMicTest();
    }
});

// «Слушать себя» (фаза 7c): бэкенд включает возврат звука, если проверка идёт,
// иначе — при следующем StartMicTest. В базу не пишется.
var micMonitorToggle = document.getElementById("mic-monitor-toggle");
if (micMonitorToggle) {
    micMonitorToggle.addEventListener("change", function () {
        svcQuiet("SetMicMonitor", micMonitorToggle.checked);
        flog("INF", "frontend.settings", "mic monitor: " + micMonitorToggle.checked);
    });
}

// «Сбросить настройки звука» (фаза 7c): всё из вкладки «Звук» — по умолчанию.
var audioResetBtn = document.getElementById("audio-reset-btn");
if (audioResetBtn) {
    audioResetBtn.addEventListener("click", function () {
        confirmDialog(t("settings.audio.reset_title"), t("settings.audio.reset_text"), t("settings.audio.reset_confirm"), async function () {
            var wasTesting = micTesting;
            if (wasTesting) stopMicTest();
            try {
                await svc("ResetAudioSettings");
                flog("INF", "frontend.settings", "audio settings reset");
            } catch (err) {
                flog("ERR", "frontend.settings", "audio reset: " + err);
            }
            await loadAudioDevices();
            showSaved();
            if (wasTesting) startMicTest();
        }, { danger: false });
    });
}

// startMicTest — экспорт для «Проверить микрофон» в меню звонка.
export async function startMicTest() {
    if (micTesting) return;
    var idx = parseInt(micSelect.value, 10);
    if (isNaN(idx)) idx = -1; // «|| -1» превращал устройство #0 в «default»
    try {
        await svc("StartMicTest", idx);
        micTesting = true;
        micTestBtn.textContent = t("settings.mic.stop");
        // Цепочка setTimeout вместо setInterval: следующий тик планируется
        // ТОЛЬКО после завершения (или падения) GetMicLevel. Это предотвращает
        // накопление pending-promise'ов если Go-сторона задержалась >50ms.
        var tick = async function () {
            if (!micTesting) return;
            try {
                var level = await svc("GetMicLevel");
                var pct = Math.min(level * 100, 100);
                vuMeterFill.style.width = pct + "%";
                if (pct > 80) {
                    vuMeterFill.style.background = "var(--red)";
                } else if (pct > 50) {
                    vuMeterFill.style.background = "var(--yellow)";
                } else {
                    vuMeterFill.style.background = "var(--green)";
                }
            } catch (e) {}
            if (micTesting) {
                vuTimer = setTimeout(tick, 50);
            }
        };
        vuTimer = setTimeout(tick, 50);
    } catch (err) {
        console.error("mic test start:", err);
    }
}

function stopMicTest() {
    svcQuiet("StopMicTest");
    micTesting = false;
    micTestBtn.textContent = t("settings.mic.test");
    if (vuTimer) { clearTimeout(vuTimer); vuTimer = null; }
    vuMeterFill.style.width = "0%";
}

// --- Password visibility toggle ---
document.querySelectorAll(".password-toggle").forEach(function (btn) {
    btn.addEventListener("click", function () {
        var input = document.getElementById(btn.dataset.target);
        if (!input) return;
        if (input.type === "password") {
            input.type = "text";
            btn.innerHTML = '<i data-lucide="eye-off" class="icon-18"></i>';
        } else {
            input.type = "password";
            btn.innerHTML = '<i data-lucide="eye" class="icon-18"></i>';
        }
        lucide.createIcons();
        input.focus();
    });
});

// --- TURN Server ---

async function loadTURNConfig() {
    try {
        var json = await svc("GetICEServers");
        var source = await svc("GetICEServersSource");
        var sourceEl = document.getElementById("turn-source-info");

        var resetBtn = document.getElementById("turn-reset-btn");
        if (source === "server") {
            sourceEl.textContent = t("settings.turn.from_server");
            sourceEl.style.display = "block";
            resetBtn.style.display = "none";
        } else if (source === "manual") {
            sourceEl.textContent = t("settings.turn.manual_override");
            sourceEl.style.display = "block";
            resetBtn.style.display = "";
        } else {
            sourceEl.style.display = "none";
            resetBtn.style.display = "none";
        }

        if (!json) return;
        var servers = JSON.parse(json);
        for (var i = 0; i < servers.length; i++) {
            var s = servers[i];
            if (s.urls && s.urls[0] && s.urls[0].startsWith("turn:")) {
                document.getElementById("turn-url").value = s.urls[0];
                document.getElementById("turn-username").value = s.username || "";
                document.getElementById("turn-credential").value = s.credential || "";
                break;
            }
        }
    } catch (e) {
        flog("ERR", "frontend.settings", "load TURN: " + e);
    }
}

document.getElementById("turn-save-btn").addEventListener("click", async function () {
    var url = document.getElementById("turn-url").value.trim();
    var servers = [
        { urls: ["stun:stun.l.google.com:19302"] },
        { urls: ["stun:stun1.l.google.com:19302"] }
    ];
    if (url) {
        servers.push({
            urls: [url],
            username: document.getElementById("turn-username").value.trim(),
            credential: document.getElementById("turn-credential").value.trim()
        });
    }
    var statusEl = document.getElementById("turn-save-status");
    try {
        await svc("SaveICEServers", JSON.stringify(servers));
        flog("INF", "frontend.settings", "TURN config saved");
        if (statusEl) {
            statusEl.textContent = t("settings.turn.saved");
            statusEl.style.display = "inline";
            setTimeout(function () { statusEl.style.display = "none"; }, 3000);
        }
    } catch (e) {
        flog("ERR", "frontend.settings", "save TURN: " + e);
        if (statusEl) {
            statusEl.textContent = t("settings.turn.error");
            statusEl.style.display = "inline";
        }
    }
    loadTURNConfig();
});

document.getElementById("turn-reset-btn").addEventListener("click", async function () {
    try {
        await svc("ResetICEServers");
        flog("INF", "frontend.settings", "TURN config reset to server");
        loadTURNConfig();
    } catch (e) {
        flog("ERR", "frontend.settings", "reset TURN: " + e);
    }
});

// --- Updates section handlers ---
var checkBtn = document.getElementById("settings-update-check");
if (checkBtn) {
    checkBtn.addEventListener("click", async function () {
        var statusEl = document.getElementById("settings-update-status");
        var latestEl = document.getElementById("settings-update-latest");
        var originalLabel = checkBtn.textContent;

        checkBtn.disabled = true;
        checkBtn.textContent = t("update.checking");
        if (statusEl) {
            statusEl.className = "settings-updates-status is-checking";
            statusEl.textContent = t("update.checking");
            statusEl.style.display = "block";
        }

        try {
            var res = await checkForUpdates();
            if (latestEl && res && res.latest) latestEl.textContent = res.latest;
            renderUpdateStatus(res || {});
        } catch (err) {
            if (statusEl) {
                statusEl.className = "settings-updates-status is-error";
                statusEl.textContent = t("update.check_failed") + ": " + err;
            }
        } finally {
            checkBtn.disabled = false;
            checkBtn.textContent = originalLabel;
        }
    });
}

// renderUpdateStatus — строка статуса обновлений (фаза 7d, макет SettingsAbout):
// «✓ У вас последняя версия. Проверено 5 мин назад.». res — ответ
// CheckForUpdates; null — при открытии, по сохранённому времени проверки.
function checkedAgo(ts) {
    var min = Math.floor((Date.now() - ts) / 60000);
    if (min < 1) return t("update.just_now");
    if (min < 60) return t("update.min_ago").replace("{n}", min);
    var h = Math.floor(min / 60);
    if (h < 24) return t("update.hours_ago").replace("{n}", h);
    return new Date(ts).toLocaleDateString(getLocale());
}

function renderUpdateStatus(res) {
    var statusEl = document.getElementById("settings-update-status");
    if (!statusEl) return;
    if (res && res.error) {
        statusEl.className = "settings-updates-status is-error";
        statusEl.textContent = t("update.check_failed") + ": " + res.error;
        statusEl.style.display = "block";
        return;
    }
    var pending = res ? (res.available && res.supported_for_platform ? res.latest || "" : "") : pendingUpdateVersion();
    if (pending) {
        statusEl.className = "settings-updates-status is-available";
        statusEl.textContent = t("update.available_banner_hint").replace("{version}", pending);
        statusEl.style.display = "block";
        return;
    }
    var ts = lastUpdateCheck();
    if (!res && !ts) {
        statusEl.style.display = "none";
        return;
    }
    statusEl.className = "settings-updates-status is-ok";
    statusEl.innerHTML = '<i data-lucide="check" class="icon-14"></i><span></span>';
    statusEl.lastChild.textContent = t("update.up_to_date") + "." + (ts ? " " + t("update.checked_ago").replace("{when}", checkedAgo(ts)) : "");
    statusEl.style.display = "block";
    if (window.lucide) lucide.createIcons({ nodes: [statusEl] });
}

// «Диагностика» (фаза 7d, app_logs.go): папка логов, архив, подробное логирование.
var verboseToggle = document.getElementById("verbose-logging-toggle");
async function loadVerboseLogging() {
    if (!verboseToggle) return;
    try {
        var v = (await svc("GetVerboseLogging")) || {};
        verboseToggle.checked = !!v.on;
        verboseToggle.disabled = !!v.always;
        var desc = document.getElementById("verbose-logging-desc");
        if (desc) desc.textContent = t(v.always ? "settings.diag.verbose_always" : "settings.diag.verbose_desc");
    } catch (e) {}
}
if (verboseToggle) {
    verboseToggle.addEventListener("change", function () {
        svc("SetVerboseLogging", verboseToggle.checked).catch(function (err) {
            flog("ERR", "frontend.settings", "verbose logging: " + err);
            loadVerboseLogging();
        });
    });
}
var logsOpenBtn = document.getElementById("logs-open-btn");
if (logsOpenBtn) {
    logsOpenBtn.addEventListener("click", function () {
        svc("OpenLogsFolder").catch(function (err) {
            flog("ERR", "frontend.settings", "open logs folder: " + err);
            showInfo(t("settings.diag.open_failed"));
        });
    });
}
var logsExportBtn = document.getElementById("logs-export-btn");
if (logsExportBtn) {
    logsExportBtn.addEventListener("click", function () {
        logsExportBtn.disabled = true;
        svc("ExportLogsArchive").then(function (path) {
            if (path) showInfo(t("settings.diag.exported"));
        }).catch(function (err) {
            flog("ERR", "frontend.settings", "export logs: " + err);
            showInfo(t("settings.diag.export_failed"));
        }).finally(function () { logsExportBtn.disabled = false; });
    });
}

var autoCheckChk = document.getElementById("settings-update-auto-check");
if (autoCheckChk) {
    autoCheckChk.addEventListener("change", function () {
        setAutoCheckEnabled(autoCheckChk.checked);
    });
}

var rollbackBtn = document.getElementById("settings-update-rollback");
if (rollbackBtn) {
    rollbackBtn.addEventListener("click", async function () {
        if (!confirm(t("update.button.rollback") + "?")) return;
        await doRollback();
    });
}

// --- Meshtastic settings ---
// State of the dropdown. Repopulated from MeshtasticService.ListSerialPorts on
// every tab open and Rescan click — boards may be plugged or unplugged at any
// time so we never cache.
var _meshLastTestPort = "";

async function loadMeshtasticSettings() {
    var toggle = document.getElementById("mesh-toggle");
    var portSelect = document.getElementById("mesh-port-select");
    var portnumInput = document.getElementById("mesh-portnum");
    if (!toggle || !portSelect) return;

    try {
        var status = await svc("Status");
        toggle.checked = !!(status && status.enabled);
        // Восстанавливаем ранее выбранный транспорт из Status (или из текущего
        // settings-state). При disabled у бэка transport="" — тогда просто
        // оставляем выбранное в UI ранее (по умолчанию serial).
        if (status && status.transport) {
            _setMeshTransportUI(status.transport, status.port || "");
        }
        renderMeshStatus(status);
    } catch (e) { flog("WRN", "frontend.mesh", "Status failed: " + e); }

    try {
        var pn = await svc("PortNum");
        if (typeof pn === "number" && pn > 0) portnumInput.value = pn;
    } catch (e) {}

    await refreshMeshPorts();
    await reloadSavedMeshWiFi();
}

// Переключает радио "USB/WiFi" + показывает/прячет соответствующий блок.
// transport: "serial" | "tcp". addr — текущий адрес (COM5 / host:port).
function _setMeshTransportUI(transport, addr) {
    var radios = document.querySelectorAll('input[name="mesh-transport"]');
    radios.forEach(function (r) { r.checked = r.value === transport; });
    var serialBlock = document.getElementById("mesh-transport-serial");
    var tcpBlock = document.getElementById("mesh-transport-tcp");
    if (serialBlock) serialBlock.style.display = transport === "tcp" ? "none" : "";
    if (tcpBlock) tcpBlock.style.display = transport === "tcp" ? "" : "none";
    if (transport === "tcp") {
        var hostInput = document.getElementById("mesh-wifi-host");
        if (hostInput && addr && !hostInput.value) hostInput.value = addr;
    }
}

// Текущий выбранный в UI транспорт и адрес. Вызывается из handler'ов Enable/Test.
function _readMeshTransportFromUI() {
    var radios = document.querySelectorAll('input[name="mesh-transport"]:checked');
    var transport = radios.length ? radios[0].value : "serial";
    var addr = "";
    if (transport === "tcp") {
        var hostInput = document.getElementById("mesh-wifi-host");
        addr = hostInput ? hostInput.value.trim() : "";
    } else {
        var portSelect = document.getElementById("mesh-port-select");
        addr = portSelect ? portSelect.value : "";
    }
    return { transport: transport, addr: addr };
}

// Перечень сохранённых WiFi-сетей устройства — dropdown над полем SSID.
// Подгружается при открытии Meshtastic-таба и после каждой successfull apply/forget.
async function reloadSavedMeshWiFi() {
    var sel = document.getElementById("mesh-wifi-saved");
    var forget = document.getElementById("mesh-wifi-forget");
    if (!sel) return;
    var prev = sel.value;
    try {
        var list = await svc("ListMeshWiFiNetworks");
        sel.innerHTML = "";
        var optNew = document.createElement("option");
        optNew.value = "";
        optNew.textContent = t("settings.mesh.wifi.saved_new");
        sel.appendChild(optNew);
        (list || []).forEach(function (n) {
            var o = document.createElement("option");
            o.value = n.ssid;
            o.textContent = n.ssid;
            sel.appendChild(o);
        });
        // Если та же сеть ещё есть — восстановить выбор, иначе сбросить.
        if (prev && Array.from(sel.options).some(function (o) { return o.value === prev; })) {
            sel.value = prev;
        } else {
            sel.value = "";
        }
        if (forget) forget.disabled = !sel.value;
    } catch (e) {
        flog("WRN", "frontend.mesh.wifi", "ListMeshWiFiNetworks failed: " + e);
    }
}

async function refreshMeshPorts() {
    var portSelect = document.getElementById("mesh-port-select");
    var portEmpty = document.getElementById("mesh-port-empty");
    if (!portSelect) return;
    try {
        var ports = await svc("ListSerialPorts");
        portSelect.innerHTML = "";
        if (!ports || ports.length === 0) {
            portEmpty.style.display = "";
            portSelect.disabled = true;
            return;
        }
        portEmpty.style.display = "none";
        portSelect.disabled = false;
        // Sort: likely-Meshtastic ports first
        ports.sort(function (a, b) {
            if (a.likelyMeshtastic !== b.likelyMeshtastic) return a.likelyMeshtastic ? -1 : 1;
            return a.name.localeCompare(b.name);
        });
        ports.forEach(function (p) {
            var opt = document.createElement("option");
            opt.value = p.name;
            opt.textContent = p.friendly || p.name;
            portSelect.appendChild(opt);
        });
        // Pre-select previously enabled port if possible
        try {
            var status = await svc("Status");
            if (status && status.port) {
                portSelect.value = status.port;
            }
        } catch (e) {}
    } catch (e) {
        flog("WRN", "frontend.mesh", "ListSerialPorts failed: " + e);
        portEmpty.style.display = "";
        portEmpty.textContent = String(e);
    }
}

// Трекаем переход reconnecting → disabled: если было true, а стало false без
// возврата к enabled — значит все 12 попыток провалились. Показываем юзеру
// inline-красное сообщение. Флаг живёт в модуле, сбрасывается при enable=true.
var _lastReconnectInFlight = false;
var _lastReconnectFailed = false;

function renderMeshStatus(status) {
    var line = document.getElementById("mesh-status-line");
    var testBtn = document.getElementById("mesh-test-btn");
    var testStatus = document.getElementById("mesh-test-status");
    var portSelect = document.getElementById("mesh-port-select");
    var portnumInput = document.getElementById("mesh-portnum");
    var rescanBtn = document.getElementById("mesh-rescan-btn");
    var wifiSection = document.getElementById("mesh-wifi-section");
    var wifiApply = document.getElementById("mesh-wifi-apply");
    if (!line) return;

    var enabled = !!(status && status.enabled);
    var reconnecting = !!(status && status.reconnecting);
    var showMeshUI = enabled || reconnecting;

    // Обнаружение failed reconnect: было reconnecting=true → стало false, но
    // адаптер так и не поднялся (enabled=false).
    if (_lastReconnectInFlight && !reconnecting && !enabled) {
        _lastReconnectFailed = true;
    }
    if (enabled) {
        _lastReconnectFailed = false;
    }
    _lastReconnectInFlight = reconnecting;

    if (enabled) {
        var portLabel = status.port || "—";
        if (status.transport === "tcp") portLabel += " (WiFi)";
        var label = t("settings.mesh.status_enabled").replace("{port}", portLabel);
        if (typeof status.nodesSeen === "number") {
            label += " · " + t("settings.mesh.nodes_seen") + ": " + status.nodesSeen;
        }
        line.textContent = label;
        if (testBtn) testBtn.disabled = true;
        if (portSelect) portSelect.disabled = true;
        if (portnumInput) portnumInput.disabled = true;
        if (rescanBtn) rescanBtn.disabled = true;
        if (testStatus) testStatus.style.display = "none";
    } else if (reconnecting) {
        // Во время reconnect mesh-status-line показывает прогресс,
        // config-controls остаются disabled (порт всё ещё наш).
        var att = status.reconnectAttempt || 0, mx = status.reconnectMax || 0;
        line.textContent = t("settings.mesh.wifi.reconnecting")
            .replace("{n}", att).replace("{total}", mx);
        if (testBtn) testBtn.disabled = true;
        if (portSelect) portSelect.disabled = true;
        if (portnumInput) portnumInput.disabled = true;
        if (rescanBtn) rescanBtn.disabled = true;
    } else {
        line.textContent = t("settings.mesh.status_disabled");
        if (testBtn) testBtn.disabled = false;
        if (portSelect) portSelect.disabled = false;
        if (portnumInput) portnumInput.disabled = false;
        if (rescanBtn) rescanBtn.disabled = false;
    }

    if (wifiSection) wifiSection.style.display = showMeshUI ? "" : "none";
    // Apply заблокирован пока adapter либо мёртв, либо ребутится — чтобы
    // нельзя было накидать три клика подряд во время reboot'а устройства.
    if (wifiApply) wifiApply.disabled = !enabled || reconnecting;

    // Синхронизируем WiFi-контролы с реальным состоянием прошивки (из Config).
    // userEdited-флаг предохраняет: если юзер уже что-то ввёл/переключил —
    // не затираем. Но как только server state догоняет текущий checkbox/input —
    // сбрасываем guard, чтобы дальше UI снова был live.
    var wifiChk = document.getElementById("mesh-wifi-enabled");
    var ssidInput = document.getElementById("mesh-wifi-ssid");
    if (wifiChk && status && status.wifi) {
        if (wifiChk.dataset.userEdited) {
            if (!!status.wifi.enabled === wifiChk.checked) {
                delete wifiChk.dataset.userEdited;
            }
        } else {
            wifiChk.checked = !!status.wifi.enabled;
        }
    }
    if (ssidInput && status && status.wifi && status.wifi.ssid) {
        if (ssidInput.dataset.userEdited) {
            if (status.wifi.ssid === ssidInput.value) {
                delete ssidInput.dataset.userEdited;
            }
        } else {
            ssidInput.value = status.wifi.ssid;
        }
    }
    _syncWiFiCredsVisibility();

    // Bluetooth-секция — та же логика видимости, что у WiFi.
    var btSection = document.getElementById("mesh-bt-section");
    var btApply = document.getElementById("mesh-bt-apply");
    if (btSection) btSection.style.display = showMeshUI ? "" : "none";
    if (btApply) btApply.disabled = !enabled || reconnecting;

    // Device-секция (LED + screen + wake). Та же логика видимости + userEdited-guard.
    // Пока device-config не прочитан — Apply disabled, чтобы user не применил
    // дефолтные значения чекбоксов по ошибке.
    var devSection = document.getElementById("mesh-device-section");
    var devApply = document.getElementById("mesh-device-apply");
    var devLoaded = !!(status && status.device);
    if (devSection) devSection.style.display = showMeshUI ? "" : "none";
    if (devApply) devApply.disabled = !enabled || reconnecting || !devLoaded;
    var devStatusEl = document.getElementById("mesh-device-status");
    if (devStatusEl) {
        if (enabled && !reconnecting && !devLoaded) {
            devStatusEl.textContent = t("settings.mesh.device.loading") || "Reading device config…";
            devStatusEl.style.color = "";
            devStatusEl.style.display = "";
        } else if (devStatusEl.style.color === "") {
            // Свободная строка — прячем, когда device подгрузился.
            // Красные error-строки не трогаем (они ставятся через bindMeshDevice).
            devStatusEl.style.display = "none";
        }
    }
    if (status && status.device) {
        var d = status.device;
        var ledChk = document.getElementById("mesh-device-led");
        var scrInput = document.getElementById("mesh-device-screen-secs");
        var wakeChk = document.getElementById("mesh-device-wake");
        if (ledChk) {
            if (ledChk.dataset.userEdited) {
                if (!!d.ledHeartbeat === ledChk.checked) delete ledChk.dataset.userEdited;
            } else {
                ledChk.checked = !!d.ledHeartbeat;
            }
        }
        if (scrInput) {
            var srvSecs = String(d.screenOnSecs || 0);
            // Если устройство вернуло нестандартное значение (не из списка
            // опций dropdown'а) — добавляем "custom"-option на лету, иначе
            // select игнорирует value и пользователь видит первую опцию.
            if (scrInput.tagName === "SELECT") {
                var has = false;
                for (var i = 0; i < scrInput.options.length; i++) {
                    if (scrInput.options[i].value === srvSecs) { has = true; break; }
                }
                if (!has) {
                    var custom = document.createElement("option");
                    custom.value = srvSecs;
                    custom.textContent = t("settings.mesh.device.screen_custom").replace("{n}", srvSecs);
                    custom.dataset.customFromDevice = "1";
                    scrInput.appendChild(custom);
                }
            }
            if (scrInput.dataset.userEdited) {
                if (srvSecs === scrInput.value) delete scrInput.dataset.userEdited;
            } else {
                scrInput.value = srvSecs;
            }
        }
        if (wakeChk) {
            if (wakeChk.dataset.userEdited) {
                if (!!d.wakeOnTap === wakeChk.checked) delete wakeChk.dataset.userEdited;
            } else {
                wakeChk.checked = !!d.wakeOnTap;
            }
        }
    }

    // Синхронизируем BT-контролы с Config. Та же логика: userEdited-guard
    // автосбрасывается, когда server догнал текущий checkbox/mode.
    var btChk = document.getElementById("mesh-bt-enabled");
    var btMode = document.getElementById("mesh-bt-mode");
    if (btChk && status && status.bluetooth) {
        if (btChk.dataset.userEdited) {
            if (!!status.bluetooth.enabled === btChk.checked) {
                delete btChk.dataset.userEdited;
            }
        } else {
            btChk.checked = !!status.bluetooth.enabled;
        }
    }
    if (btMode && status && status.bluetooth && status.bluetooth.mode) {
        var modeMap = { "RANDOM_PIN": "random", "FIXED_PIN": "fixed", "NO_PIN": "no_pin" };
        var v = modeMap[status.bluetooth.mode];
        if (v) {
            if (btMode.dataset.userEdited) {
                if (v === btMode.value) {
                    delete btMode.dataset.userEdited;
                }
            } else {
                btMode.value = v;
            }
        }
    }
    _syncBtCredsVisibility();
    _renderMeshBtInfo(status);

    // Device info line — модель + localNodeId. Видна только когда mesh
    // действительно открыт (hwModel приходит из tracker).
    var deviceLine = document.getElementById("mesh-device-line");
    if (deviceLine) {
        if (enabled && status && (status.hwModel || status.localNodeId)) {
            var parts = [];
            if (status.hwModel) parts.push(status.hwModel);
            if (status.localNodeId) parts.push(status.localNodeId);
            deviceLine.textContent = parts.join(" · ");
            deviceLine.style.display = "";
        } else {
            deviceLine.style.display = "none";
        }
    }

    _renderMeshWiFiInfo(status, reconnecting);
    _syncWiFiCredsVisibility();
}

function _renderMeshWiFiInfo(status, reconnecting) {
    var info = document.getElementById("mesh-wifi-info");
    var ln = document.getElementById("mesh-wifi-info-line");
    if (!info || !ln) return;

    if (reconnecting) {
        var att = status.reconnectAttempt || 0, mx = status.reconnectMax || 0;
        ln.textContent = t("settings.mesh.wifi.reconnecting")
            .replace("{n}", att).replace("{total}", mx);
        info.style.color = "";
        info.style.display = "";
        return;
    }
    if (_lastReconnectFailed) {
        ln.textContent = t("settings.mesh.wifi.reconnect_failed");
        info.style.color = "var(--red,#d33)";
        info.style.display = "";
        return;
    }
    if (status && status.wifi) {
        var w = status.wifi;
        if (!w.enabled) {
            // WiFi в конфиге выключен — SSID может быть сохранён в прошивке,
            // но адаптер не поднят. Показываем "off" независимо от SSID.
            ln.textContent = t("settings.mesh.wifi.disconnected");
        } else if (w.connected && w.ssid) {
            ln.textContent = t("settings.mesh.wifi.info")
                .replace("{ssid}", w.ssid)
                .replace("{ip}", w.ip || "—")
                .replace("{rssi}", w.rssi != null ? w.rssi : "—");
        } else if (w.ssid) {
            // WiFi включён + SSID задан, но connected=false —
            // идёт ассоциация/DHCP/неверный пароль.
            ln.textContent = t("settings.mesh.wifi.connecting").replace("{ssid}", w.ssid);
        } else {
            ln.textContent = t("settings.mesh.wifi.disconnected");
        }
        info.style.color = "";
        info.style.display = "";
        return;
    }
    info.style.display = "none";
}

// Inline-статус Bluetooth: включён/выключен, + факт активного BLE-клиента
// (спарен ли сейчас телефон). У BT нет промежуточного "connecting" —
// либо модуль поднят и слушает, либо выключен.
function _renderMeshBtInfo(status) {
    var info = document.getElementById("mesh-bt-info");
    var ln = document.getElementById("mesh-bt-info-line");
    if (!info || !ln) return;
    if (!status || !status.bluetooth) { info.style.display = "none"; return; }
    var b = status.bluetooth;
    if (b.enabled && b.connected) {
        ln.textContent = t("settings.mesh.bt.info_on_connected");
    } else if (b.enabled) {
        ln.textContent = t("settings.mesh.bt.info_on_idle");
    } else {
        ln.textContent = t("settings.mesh.bt.info_off");
    }
    info.style.color = "";
    info.style.display = "";
}

// Поля SSID/PSK имеют смысл только когда чекбокс "WiFi enabled" установлен —
// выключенный WiFi на устройстве задаётся одним флагом.
function _syncWiFiCredsVisibility() {
    var en = document.getElementById("mesh-wifi-enabled");
    var creds = document.getElementById("mesh-wifi-creds");
    if (en && creds) creds.style.display = en.checked ? "" : "none";
}

// Режим и PIN имеют смысл только когда BT включён. Плюс PIN-поле видно только
// при mode=fixed.
function _syncBtCredsVisibility() {
    var en = document.getElementById("mesh-bt-enabled");
    var creds = document.getElementById("mesh-bt-creds");
    if (en && creds) creds.style.display = en.checked ? "" : "none";
    var mode = document.getElementById("mesh-bt-mode");
    var pinRow = document.getElementById("mesh-bt-pin-row");
    if (mode && pinRow) pinRow.style.display = mode.value === "fixed" ? "" : "none";
}

document.getElementById("mesh-toggle").addEventListener("change", async function () {
    var enabled = this.checked;
    try {
        if (enabled) {
            var sel = _readMeshTransportFromUI();
            if (!sel.addr) {
                this.checked = false;
                var msg = sel.transport === "tcp"
                    ? t("settings.mesh.err_invalid_addr")
                    : t("settings.mesh.no_ports");
                _meshSettingsInfo(t("settings.mesh.title"), msg);
                return;
            }
            await svc("Enable", sel.transport, sel.addr);
            flog("INF", "frontend.mesh", "enabled transport=" + sel.transport + " addr=" + sel.addr);
        } else {
            await svc("Disable");
            flog("INF", "frontend.mesh", "disabled");
        }
        var status = await svc("Status");
        renderMeshStatus(status);
    } catch (err) {
        this.checked = !enabled;
        flog("ERR", "frontend.mesh", "toggle failed: " + err);
        _meshSettingsInfo(t("settings.mesh.title"), String(err));
    }
});

// Переключение транспорта — показывает/прячет serial/tcp блоки. Если mesh
// сейчас включён — сразу переподключаемся к выбранному адресу, чтобы
// пользователю не пришлось руками дёргать toggle Enable.
document.querySelectorAll('input[name="mesh-transport"]').forEach(function (r) {
    r.addEventListener("change", function () {
        _setMeshTransportUI(r.value, "");
        _applyTransportIfActive();
    });
});

// Auto-reconnect при изменении IP для WiFi (после blur или Enter).
(function () {
    var hostInput = document.getElementById("mesh-wifi-host");
    if (!hostInput) return;
    hostInput.addEventListener("change", _applyTransportIfActive);
    hostInput.addEventListener("keydown", function (e) {
        if (e.key === "Enter") { e.preventDefault(); _applyTransportIfActive(); }
    });
})();

// Если mesh уже enabled и выбранный транспорт/адрес отличается от
// активного — Disable + Enable. Тихо игнорирует случаи когда делать
// нечего (mesh off или адрес не изменился).
async function _applyTransportIfActive() {
    var toggle = document.getElementById("mesh-toggle");
    if (!toggle || !toggle.checked) return; // mesh off — просто UI-смена, apply не нужен
    var sel = _readMeshTransportFromUI();
    if (!sel.addr) return; // пустое поле ввода

    // Сравниваем с тем что сейчас активно в сервисе — если совпадает,
    // пересоздавать адаптер ни к чему.
    try {
        var current = await svc("Status");
        if (current && current.transport === sel.transport && current.port === sel.addr) return;
    } catch (_) {}

    try {
        await svc("Disable");
        await svc("Enable", sel.transport, sel.addr);
        flog("INF", "frontend.mesh", "transport switched transport=" + sel.transport + " addr=" + sel.addr);
    } catch (err) {
        flog("ERR", "frontend.mesh", "transport switch failed: " + err);
        _meshSettingsInfo(t("settings.mesh.title"), String(err));
        // Если Enable провалился — mesh остался off, синхронизируем toggle.
        toggle.checked = false;
    }
    try {
        var status = await svc("Status");
        renderMeshStatus(status);
    } catch (_) {}
}

document.getElementById("mesh-rescan-btn").addEventListener("click", function () {
    refreshMeshPorts();
});

// Auto-reconnect при смене COM-порта (если mesh enabled).
(function () {
    var sel = document.getElementById("mesh-port-select");
    if (sel) sel.addEventListener("change", _applyTransportIfActive);
})();

document.getElementById("mesh-portnum").addEventListener("change", async function () {
    var n = parseInt(this.value, 10);
    if (isNaN(n)) return;
    try {
        await svc("SetPortNum", n);
        flog("INF", "frontend.mesh", "portnum=" + n);
    } catch (err) {
        flog("ERR", "frontend.mesh", "SetPortNum failed: " + err);
        _meshSettingsInfo(t("settings.mesh.title"), String(err));
    }
});

document.getElementById("mesh-test-btn").addEventListener("click", async function () {
    var statusEl = document.getElementById("mesh-test-status");
    var sel = _readMeshTransportFromUI();
    if (!sel.addr) return;
    var btn = this;
    btn.disabled = true;
    statusEl.style.display = "";
    statusEl.textContent = t("settings.mesh.test_running");
    statusEl.style.color = "";
    _meshLastTestPort = sel.addr;
    try {
        var res = await svc("TestConnection", sel.transport, sel.addr);
        if (res && res.ok) {
            statusEl.textContent = t("settings.mesh.test_ok")
                .replace("{nodeId}", res.nodeId || "—")
                .replace("{hwModel}", res.hwModel || "—");
            statusEl.style.color = "var(--green, #4caf50)";
        } else {
            statusEl.textContent = t("settings.mesh.test_fail").replace("{error}", (res && res.error) || "unknown");
            statusEl.style.color = "var(--red, #d33)";
        }
    } catch (err) {
        statusEl.textContent = t("settings.mesh.test_fail").replace("{error}", String(err));
        statusEl.style.color = "var(--red, #d33)";
    } finally {
        btn.disabled = false;
    }
});

// Last known status snapshot kept in module scope so we can update its
// nodesSeen counter from frontend-tracked node IDs without an RPC for every
// new node. renderMeshStatus is a pure function of this object.
var _lastMeshStatus = null;
var _meshSeenNodeNums = new Set();

Events.On("meshtastic:status-change", function (ev) {
    _lastMeshStatus = (ev && ev.data) || null;
    if (_lastMeshStatus && !_lastMeshStatus.enabled) _meshSeenNodeNums.clear();
    if (_lastMeshStatus) _lastMeshStatus.nodesSeen = Math.max(_lastMeshStatus.nodesSeen || 0, _meshSeenNodeNums.size);
    renderMeshStatus(_lastMeshStatus);
    var toggle = document.getElementById("mesh-toggle");
    if (toggle && _lastMeshStatus) toggle.checked = !!_lastMeshStatus.enabled;
});

Events.On("meshtastic:node-update", function (ev) {
    var snap = ev && ev.data;
    if (!snap || typeof snap.nodeNum !== "number") return;
    var was = _meshSeenNodeNums.size;
    _meshSeenNodeNums.add(snap.nodeNum);
    if (_meshSeenNodeNums.size === was) return; // not a new node
    if (!_lastMeshStatus) return;
    _lastMeshStatus.nodesSeen = _meshSeenNodeNums.size;
    if (document.getElementById("settings-tab-meshtastic").style.display !== "none") {
        renderMeshStatus(_lastMeshStatus);
    }
});

// --- WiFi on device ---------------------------------------------------------

(function bindMeshWiFi() {
    var enabledChk = document.getElementById("mesh-wifi-enabled");
    var ssidInput = document.getElementById("mesh-wifi-ssid");
    var pskInput = document.getElementById("mesh-wifi-psk");
    var applyBtn = document.getElementById("mesh-wifi-apply");
    var errEl = document.getElementById("mesh-wifi-status"); // красные ошибки
    var savedSel = document.getElementById("mesh-wifi-saved");
    var forgetBtn = document.getElementById("mesh-wifi-forget");
    if (!enabledChk || !ssidInput || !pskInput || !applyBtn || !errEl) return;

    // Show/hide для PSK-инпута обслуживается глобальным .password-toggle
    // handler'ом выше в этом же модуле — ничего локально не делаем.

    function setError(msg) {
        if (!msg) { errEl.style.display = "none"; return; }
        errEl.textContent = msg;
        errEl.style.display = "";
    }

    enabledChk.addEventListener("change", function () {
        enabledChk.dataset.userEdited = "1";
        _syncWiFiCredsVisibility();
        setError("");
    });
    ssidInput.addEventListener("input", function () {
        ssidInput.dataset.userEdited = "1";
        setError("");
    });

    // Выбор из сохранённых сетей → подставляем SSID+PSK, включаем WiFi-toggle
    // (по смыслу сохранённая запись = сеть, которую пользователь хочет поднять).
    if (savedSel) {
        savedSel.addEventListener("change", async function () {
            setError("");
            if (forgetBtn) forgetBtn.disabled = !savedSel.value;
            if (!savedSel.value) {
                ssidInput.value = "";
                pskInput.value = "";
                return;
            }
            try {
                var list = await svc("ListMeshWiFiNetworks");
                var rec = (list || []).find(function (n) { return n.ssid === savedSel.value; });
                if (!rec) return;
                ssidInput.value = rec.ssid;
                pskInput.value = rec.psk;
                enabledChk.checked = true;
                _syncWiFiCredsVisibility();
            } catch (e) {
                flog("WRN", "frontend.mesh.wifi", "lookup saved failed: " + e);
            }
        });
    }

    // Забыть выбранную сеть — confirm + ForgetMeshWiFiNetwork + перезалить список.
    if (forgetBtn && savedSel) {
        forgetBtn.addEventListener("click", function () {
            var ssid = savedSel.value;
            if (!ssid) return;
            var msg = t("settings.mesh.wifi.forget_confirm").replace("{ssid}", ssid);
            buildDialogOverlay(
                t("settings.mesh.wifi.forget"),
                '<p style="margin:8px 0 16px;">' +
                    msg.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;") +
                    "</p>",
                t("dialog.cancel") || "Cancel",
                t("settings.mesh.wifi.forget") || "Forget",
                async function (overlay) {
                    overlay.remove();
                    try {
                        await svc("ForgetMeshWiFiNetwork", ssid);
                    } catch (e) {
                        flog("ERR", "frontend.mesh.wifi", "ForgetMeshWiFiNetwork failed: " + e);
                        setError(String(e));
                        return;
                    }
                    // Если в инпутах был именно этот SSID — очистим, чтобы юзер
                    // не подумал что удалил только из списка.
                    if (ssidInput.value === ssid) {
                        ssidInput.value = "";
                        pskInput.value = "";
                    }
                    await reloadSavedMeshWiFi();
                }
            );
        });
    }

    applyBtn.addEventListener("click", async function () {
        var enabled = enabledChk.checked;
        var ssid = ssidInput.value.trim();
        var psk = pskInput.value;

        if (enabled) {
            if (!ssid) { setError(t("settings.mesh.wifi.err_ssid_empty")); return; }
            if (ssid.length > 32) { setError(t("settings.mesh.wifi.err_ssid_long")); return; }
            if (psk.length < 8 || psk.length > 63) {
                setError(t("settings.mesh.wifi.err_psk_short"));
                return;
            }
        }

        applyBtn.disabled = true;
        setError("");
        try {
            await svc("SetWiFi", enabled, ssid, psk);
            // Бэк автосохраняет сеть в журнал при enabled=true — обновим список,
            // чтобы юзер сразу увидел свежую запись в dropdown.
            if (enabled) await reloadSavedMeshWiFi();
            // Дальше бэк переводится в reconnecting → через status-change
            // renderMeshStatus отрисует прогресс. После reboot придёт
            // свежий Config.Network — снимаем userEdited, чтобы UI снова
            // мог синхронизироваться с source-of-truth.
            delete enabledChk.dataset.userEdited;
            delete ssidInput.dataset.userEdited;
        } catch (err) {
            flog("ERR", "frontend.mesh.wifi", "SetWiFi failed: " + err);
            setError(String(err));
            applyBtn.disabled = false;
        }
    });

    // При полном выключении mesh (disable пользователем или failed reconnect)
    // очищаем чувствительные поля.
    Events.On("meshtastic:status-change", function (ev) {
        var data = ev && ev.data;
        if (data && (data.enabled || data.reconnecting)) return;
        ssidInput.value = "";
        pskInput.value = "";
        enabledChk.checked = false;
        _syncWiFiCredsVisibility();
        if (pskInput.type === "text") {
            // Глобальный .password-toggle сбросит иконку только по клику —
            // здесь делаем это на всякий случай руками.
            pskInput.type = "password";
            var btn = document.querySelector('.password-toggle[data-target="mesh-wifi-psk"]');
            if (btn) {
                btn.innerHTML = '<i data-lucide="eye" class="icon-18"></i>';
                if (window.lucide && window.lucide.createIcons) {
                    window.lucide.createIcons({ nameAttr: "data-lucide", attrs: {} });
                }
            }
        }
    });

    _syncWiFiCredsVisibility();
})();

// --- Bluetooth on device --------------------------------------------------

(function bindMeshBt() {
    var enabledChk = document.getElementById("mesh-bt-enabled");
    var modeSel = document.getElementById("mesh-bt-mode");
    var pinInput = document.getElementById("mesh-bt-pin");
    var applyBtn = document.getElementById("mesh-bt-apply");
    var errEl = document.getElementById("mesh-bt-status");
    if (!enabledChk || !modeSel || !pinInput || !applyBtn || !errEl) return;

    function setError(msg) {
        if (!msg) { errEl.style.display = "none"; return; }
        errEl.textContent = msg;
        errEl.style.display = "";
    }

    enabledChk.addEventListener("change", function () {
        enabledChk.dataset.userEdited = "1";
        _syncBtCredsVisibility();
        setError("");
    });
    modeSel.addEventListener("change", function () {
        modeSel.dataset.userEdited = "1";
        _syncBtCredsVisibility();
        setError("");
    });
    // PIN — только цифры, 6 символов.
    pinInput.addEventListener("input", function () {
        pinInput.value = pinInput.value.replace(/\D/g, "").slice(0, 6);
        setError("");
    });

    applyBtn.addEventListener("click", async function () {
        var enabled = enabledChk.checked;
        var mode = modeSel.value || "random";
        var pin = 0;

        if (enabled && mode === "fixed") {
            if (!pinInput.value) {
                setError(t("settings.mesh.bt.err_pin_required"));
                return;
            }
            pin = parseInt(pinInput.value, 10);
            if (isNaN(pin) || pin < 0 || pin > 999999) {
                setError(t("settings.mesh.bt.err_pin_range"));
                return;
            }
        }

        applyBtn.disabled = true;
        setError("");
        try {
            await svc("SetBluetooth", enabled, mode, pin);
            // Устройство ребутится — renderMeshStatus подхватит reconnecting,
            // после reconnect — свежий BT-статус через queryDeviceConnectionStatus.
            delete enabledChk.dataset.userEdited;
            delete modeSel.dataset.userEdited;
        } catch (err) {
            flog("ERR", "frontend.mesh.bt", "SetBluetooth failed: " + err);
            setError(String(err));
            applyBtn.disabled = false;
        }
    });

    // Сброс на полном выключении mesh (reconnecting не считается — UI должен
    // оставаться стабильным во время reboot).
    Events.On("meshtastic:status-change", function (ev) {
        var data = ev && ev.data;
        if (data && (data.enabled || data.reconnecting)) return;
        enabledChk.checked = false;
        modeSel.value = "random";
        pinInput.value = "";
        _syncBtCredsVisibility();
        setError("");
    });

    _syncBtCredsVisibility();
})();

// --- Device (LED + screen + wake) -----------------------------------------

(function bindMeshDevice() {
    var ledChk = document.getElementById("mesh-device-led");
    var scrInput = document.getElementById("mesh-device-screen-secs");
    var wakeChk = document.getElementById("mesh-device-wake");
    var applyBtn = document.getElementById("mesh-device-apply");
    var errEl = document.getElementById("mesh-device-status");
    if (!ledChk || !scrInput || !wakeChk || !applyBtn || !errEl) return;

    function setError(msg) {
        if (!msg) { errEl.style.display = "none"; return; }
        errEl.textContent = msg;
        errEl.style.display = "";
    }

    ledChk.addEventListener("change", function () {
        ledChk.dataset.userEdited = "1";
        setError("");
    });
    wakeChk.addEventListener("change", function () {
        wakeChk.dataset.userEdited = "1";
        setError("");
    });
    // <select> → change; <input type="number"> тоже примет change, но не input.
    // Оставляем оба на всякий случай если select когда-нибудь обратно станет input.
    scrInput.addEventListener("change", function () {
        scrInput.dataset.userEdited = "1";
        setError("");
    });
    scrInput.addEventListener("input", function () {
        scrInput.dataset.userEdited = "1";
        setError("");
    });

    applyBtn.addEventListener("click", async function () {
        var secs = parseInt(scrInput.value, 10);
        if (isNaN(secs) || secs < 0 || secs > 3600) {
            setError(t("settings.mesh.device.err_screen_range"));
            return;
        }
        applyBtn.disabled = true;
        setError("");
        try {
            await svc("SetMeshDeviceConfig", ledChk.checked, secs, wakeChk.checked);
            // После reboot'а прошивка пришлёт свежий Config — сброс userEdited
            // позволит renderMeshStatus синхронизировать UI с новыми значениями.
            delete ledChk.dataset.userEdited;
            delete scrInput.dataset.userEdited;
            delete wakeChk.dataset.userEdited;
        } catch (err) {
            flog("ERR", "frontend.mesh.device", "SetMeshDeviceConfig failed: " + err);
            setError(String(err));
            applyBtn.disabled = false;
        }
    });

    // На полный disable mesh — сбрасываем поля в дефолт.
    Events.On("meshtastic:status-change", function (ev) {
        var data = ev && ev.data;
        if (data && (data.enabled || data.reconnecting)) return;
        ledChk.checked = false;
        wakeChk.checked = false;
        scrInput.value = "0";
        delete ledChk.dataset.userEdited;
        delete wakeChk.dataset.userEdited;
        delete scrInput.dataset.userEdited;
        setError("");
    });
})();



// Ссылка-контакт (vox://) — карточка «Ссылка-контакт» во вкладке Профиль.
async function loadConnConfig() {
    try {
        var uri = await svc("GetMyContactURI");
        var out = document.getElementById("conn-config-output");
        if (out) out.value = uri || "";
    } catch (e) { /* нет identity */ }
}

// --- «Заблокированные» (фаза 7b): строка в «Приватности» и список ---

function updateBlockedRow() {
    var el = document.getElementById("blocked-count");
    if (!el) return;
    var n = blockedCount();
    el.textContent = n > 0 ? t("block.count").replace("{n}", String(n)) : t("block.list_empty");
}
document.addEventListener("vox:ui-blocked-changed", updateBlockedRow);

async function showBlockedList() {
    var list = await loadBlocked();
    var d = buildDialogOverlay(t("block.list_title"), "", t("dialog.close"), null, null);
    function render() {
        d.body.innerHTML = "";
        if (list.length === 0) {
            var empty = document.createElement("p");
            empty.className = "nm-empty";
            empty.textContent = t("block.list_empty");
            d.body.appendChild(empty);
            return;
        }
        list.forEach(function (p) {
            var row = document.createElement("div");
            row.className = "bl-row";
            var av = document.createElement("span");
            av.className = "nm-av";
            applyAvatar(av, p.peerId, p.nickname || p.peerId, { letters: 1 });
            var name = document.createElement("span");
            name.className = "nm-name";
            name.textContent = p.nickname || p.peerId.substring(0, 16);
            var btn = document.createElement("button");
            btn.type = "button";
            btn.className = "bl-unblock";
            btn.textContent = t("block.unblock");
            btn.addEventListener("click", function () {
                unblockPeer(p.peerId).then(function () {
                    list = list.filter(function (x) { return x.peerId !== p.peerId; });
                    render();
                });
            });
            row.append(av, name, btn);
            d.body.appendChild(row);
        });
    }
    render();
}

var blockedListRow = document.getElementById("blocked-list-row");
if (blockedListRow) {
    blockedListRow.addEventListener("click", showBlockedList);
    blockedListRow.addEventListener("keydown", function (e) { if (e.key === "Enter") showBlockedList(); });
}
