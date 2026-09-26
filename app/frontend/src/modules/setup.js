"use strict";

import { svc, flog, state, dom, showScreen } from "./core.js";
import { t } from "./i18n.js";
import { refreshPeers } from "./contacts-list.js";
import { voiceState } from "./voice-state.js";
import { loadMuted } from "./mute.js";
import { loadChatPrefs } from "./chat-prefs.js";
import { loadSoundPrefs } from "./sounds.js";
import { loadBlocked } from "./blocked.js";
import { applyAvatar, loadAvatarHashes } from "./avatar.js";

// --- Init ---
export async function init() {
    flog("INF", "frontend.init", "starting");
    try {
        var recovery = await svc("IsRecoveryMode");
        if (recovery) {
            flog("INF", "frontend.init", "recovery mode — key was lost");
            showRecovery();
            return;
        }

        var hasIdent = await svc("HasIdentity");
        if (hasIdent) {
            state.myID = await svc("LoadIdentity");
            voiceState.init(state.myID);
            flog("INF", "frontend.init", "identity loaded: " + state.myID.substring(0, 16));
            showChat();
            autoConnect();
        } else {
            flog("INF", "frontend.init", "no identity, showing setup");
            showSetup();
        }
    } catch (err) {
        console.error("init error:", err);
        showSetup();
    }
}

function showSetup() {
    showScreen("setup");
    if (window.lucide) lucide.createIcons();
}

function showRecovery() {
    showScreen("recovery");
    if (window.lucide) lucide.createIcons();
}

export async function showChat() {
    showScreen("main");
    loadMuted();
    loadChatPrefs();
    loadSoundPrefs();
    loadBlocked();
    state.myShortID = state.myID.substring(0, 16);
    var nick = "";
    try { nick = await svc("GetNickname"); } catch (e) {}
    state.myNickname = nick || "";
    var displayName = nick || state.myShortID;
    dom.usernameEl.textContent = displayName;
    applyAvatar(dom.userAvatar, state.myID, displayName);
    loadAvatarHashes();
    dom.statusEl.textContent = t("chat.offline");
    dom.statusEl.style.color = "var(--yellow)";
}

async function autoConnect() {
    refreshPeers();
    flog("INF", "frontend.connect", "connecting...");
    try {
        await svc("Connect", ":0", ":0", "");
        flog("INF", "frontend.connect", "connected");
        dom.statusEl.textContent = t("chat.online");
        dom.statusEl.style.color = "var(--green)";
        refreshPeers();
    } catch (err) {
        flog("ERR", "frontend.connect", "failed: " + err);
        console.error("connect error:", err);
        dom.statusEl.textContent = t("chat.offline");
        dom.statusEl.style.color = "var(--yellow)";
    }
}

// --- Setup: Create Account ---
var _creating = false;
dom.generateBtn.addEventListener("click", async function () {
    if (_creating) return;
    var passphrase = document.getElementById("create-passphrase").value.trim();
    if (!passphrase) {
        dom.setupError.textContent = t("setup.create.pass_error");
        document.getElementById("create-passphrase").focus();
        return;
    }
    _creating = true;
    dom.setupError.textContent = "";
    try {
        state.myID = await svc("CreateIdentity", passphrase);
        voiceState.init(state.myID);
        // Генерируем backup key и показываем пользователю
        var backupKey = await svc("ExportKey", passphrase);
        showChat();
        autoConnect();
        // Показываем backup dialog поверх чата (не блокирующий)
        showBackupDialog(backupKey);
    } catch (err) {
        dom.setupError.textContent = "Error: " + (err.message || err);
    } finally {
        _creating = false;
    }
});

// --- Setup: Toggle import section ---
document.getElementById("setup-show-import").addEventListener("click", function (e) {
    e.preventDefault();
    var section = document.getElementById("setup-import-section");
    section.style.display = section.style.display === "none" ? "block" : "none";
    if (window.lucide) lucide.createIcons();
});

// --- Setup: Import ---
var importBtn = document.getElementById("import-btn");
var importKeyInput = document.getElementById("import-key-input");

importBtn.addEventListener("click", async function () {
    var keyStr = importKeyInput.value.trim();
    var passphrase = document.getElementById("import-passphrase").value.trim();
    if (!passphrase) {
        dom.setupError.textContent = t("setup.create.pass_error");
        return;
    }
    if (!keyStr) {
        dom.setupError.textContent = t("setup.import.key_error");
        return;
    }
    dom.setupError.textContent = "";
    try {
        state.myID = await svc("ImportKey", keyStr, passphrase);
        voiceState.init(state.myID);
        showChat();
        autoConnect();
    } catch (err) {
        dom.setupError.textContent = "Import failed: " + (err.message || err);
    }
});

importKeyInput.addEventListener("keydown", function (e) {
    if (e.key === "Enter") importBtn.click();
});

// --- Recovery: Restore from backup ---
var recoveryRestoreBtn = document.getElementById("recovery-restore-btn");
var recoveryError = document.getElementById("recovery-error");

recoveryRestoreBtn.addEventListener("click", async function () {
    var passphrase = document.getElementById("recovery-passphrase").value.trim();
    var keyStr = document.getElementById("recovery-key-input").value.trim();
    if (!passphrase) {
        recoveryError.textContent = t("setup.create.pass_error");
        return;
    }
    if (!keyStr) {
        recoveryError.textContent = t("setup.import.key_error");
        return;
    }
    recoveryError.textContent = "";
    try {
        state.myID = await svc("ImportKey", keyStr, passphrase);
        voiceState.init(state.myID);
        // Ключ восстановлен. Показываем сообщение о рестарте для восстановления БД.
        recoveryError.style.color = "var(--green)";
        recoveryError.textContent = t("recovery.restart");
    } catch (err) {
        recoveryError.style.color = "";
        recoveryError.textContent = "Restore failed: " + (err.message || err);
    }
});

// --- Recovery: Create new account ---
document.getElementById("recovery-fresh-btn").addEventListener("click", async function () {
    if (_creating) return;
    _creating = true;
    recoveryError.textContent = "";
    try {
        state.myID = await svc("CreateIdentity", "");
        voiceState.init(state.myID);
        showChat();
        autoConnect();
    } catch (err) {
        recoveryError.textContent = "Error: " + err.message;
    } finally {
        _creating = false;
    }
});

// --- Backup Key Dialog (used from settings export) ---
export function showBackupDialog(backupKey) {
    return new Promise(function (resolve) {
        var overlay = document.createElement("div");
        overlay.className = "backup-overlay";

        var dialog = document.createElement("div");
        dialog.className = "backup-dialog";
        // Каркас диалогов (макет Dialogs): шапка, тело, подвал с кнопками справа.
        dialog.innerHTML =
            '<div class="dialog-header"><h3>' + t("setup.backup.title") + '</h3></div>' +
            '<div class="dialog-body">' +
            '<p class="backup-warning">' + t("setup.backup.warning") + '</p>' +
            '<textarea readonly rows="3" class="backup-key-text">' + backupKey + '</textarea>' +
            '</div>' +
            '<div class="backup-actions">' +
            '  <button class="backup-copy-btn">' + t("setup.backup.copy") + '</button>' +
            '  <button class="backup-done-btn">' + t("setup.backup.done") + '</button>' +
            '</div>';

        dialog.querySelector(".backup-copy-btn").addEventListener("click", function () {
            navigator.clipboard.writeText(backupKey);
            this.textContent = "Copied!";
        });

        var doneBtn = dialog.querySelector(".backup-done-btn");
        doneBtn.addEventListener("click", function () {
            document.body.removeChild(overlay);
            resolve(true);
        });

        overlay.addEventListener("keydown", function (e) {
            if (e.key === "Enter" || e.key === "Escape") {
                e.stopPropagation();
                doneBtn.click();
            }
        });

        overlay.appendChild(dialog);
        document.body.appendChild(overlay);
        doneBtn.focus();
    });
}
