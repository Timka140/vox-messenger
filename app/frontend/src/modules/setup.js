"use strict";

import { svc, flog, state, dom, showScreen } from "./core.js";
import { t, getLocale } from "./i18n.js";
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

// --- Секретная фраза: создание и восстановление профиля ---
// Фраза живёт только в памяти этого модуля и никуда не пишется (ни в лог, ни в state).

var WORDS = 12;
var phrase = null;        // { ru: "...", en: "..." } — сгенерированная фраза
var phraseLang = "ru";
var confirmIdx = [];      // номера проверяемых слов (с 0)
var busy = false;

function $(id) { return document.getElementById(id); }

function showStep(prefix, name) {
    document.querySelectorAll("[id^='" + prefix + "-step-']").forEach(function (el) {
        el.hidden = el.id !== prefix + "-step-" + name;
    });
    if (window.lucide) lucide.createIcons();
}

function normWord(w) {
    return String(w || "").trim().toLowerCase().replace(/ё/g, "е");
}

function phraseWords(lang) {
    return phrase ? phrase[lang].split(" ") : [];
}

// Текст ошибки по коду из IdentityService.ApplyMnemonic.
function mnemonicError(err) {
    var msg = String(err && err.message || err);
    var m = msg.match(/unknown_word:(\d+)/);
    if (m) return { text: t("setup.err.unknown_word").replace("{n}", m[1]), word: Number(m[1]) - 1 };
    if (msg.indexOf("phrase_mismatch") !== -1) return { mismatch: true };
    if (msg.indexOf("word_count") !== -1) return { text: t("setup.err.word_count") };
    if (msg.indexOf("checksum") !== -1) return { text: t("setup.err.checksum") };
    if (msg.indexOf("mixed_lang") !== -1) return { text: t("setup.err.mixed_lang") };
    return { text: t("setup.err.generic") + " " + msg };
}

// --- Поля ввода 12 слов (восстановление, «ключ утерян») ---
function buildInputGrid(grid, onChange) {
    grid.innerHTML = "";
    var inputs = [];
    for (var i = 0; i < WORDS; i++) {
        var cell = document.createElement("label");
        cell.className = "phrase-cell";
        var num = document.createElement("span");
        num.className = "phrase-num";
        num.textContent = String(i + 1);
        var input = document.createElement("input");
        input.type = "text";
        input.autocomplete = "off";
        input.spellcheck = false;
        input.setAttribute("autocapitalize", "off");
        input.setAttribute("aria-label", t("setup.confirm.word") + " " + (i + 1));
        cell.appendChild(num);
        cell.appendChild(input);
        grid.appendChild(cell);
        inputs.push(input);
    }
    inputs.forEach(function (input, i) {
        input.addEventListener("input", function () {
            // Вставка или ввод с пробелом — раскладываем слова по полям дальше.
            var parts = input.value.split(/\s+/);
            if (parts.length > 1) {
                var j = i;
                parts.forEach(function (p) {
                    if (!p || j >= WORDS) return;
                    inputs[j].value = p;
                    j++;
                });
                if (parts[parts.length - 1] === "" && j < WORDS) inputs[j].value = "";
                inputs[Math.min(j, WORDS - 1)].focus();
            }
            input.parentNode.classList.remove("bad");
            onChange();
        });
        input.addEventListener("keydown", function (e) {
            if (e.key === "Backspace" && input.value === "" && i > 0) {
                e.preventDefault();
                inputs[i - 1].focus();
            }
        });
    });
    return {
        inputs: inputs,
        value: function () { return inputs.map(function (x) { return normWord(x.value); }).join(" "); },
        filled: function () { return inputs.every(function (x) { return normWord(x.value) !== ""; }); },
        markBad: function (i) { if (inputs[i]) { inputs[i].parentNode.classList.add("bad"); inputs[i].focus(); } },
        clear: function () { inputs.forEach(function (x) { x.value = ""; x.parentNode.classList.remove("bad"); }); },
    };
}

async function applyPhrase(value, discardBackup, onError) {
    if (busy) return;
    busy = true;
    try {
        await svc("ApplyMnemonic", value, !!discardBackup);
        showScreen("setup");
        showStep("setup", "creating");
        // Процесс сейчас завершится и запустится заново.
    } catch (err) {
        var e = mnemonicError(err);
        if (/relaunch/.test(String(err && err.message || err))) {
            showScreen("setup");
            showStep("setup", "creating");
            $("setup-creating-desc").textContent = t("setup.creating.manual");
        } else {
            onError(e);
        }
    } finally {
        busy = false;
    }
}

// --- Шаг 1: приветствие ---
$("setup-create-btn").addEventListener("click", async function () {
    try {
        phrase = await svc("NewMnemonic");
    } catch (err) {
        flog("ERR", "frontend.setup", "NewMnemonic: " + err);
        return;
    }
    phraseLang = getLocale() === "ru" ? "ru" : "en";
    $("setup-phrase-saved").checked = false;
    $("setup-phrase-next").disabled = true;
    renderPhrase();
    showStep("setup", "phrase");
});

$("setup-restore-btn").addEventListener("click", function () {
    restoreGrid.clear();
    $("setup-restore-error").textContent = "";
    $("setup-restore-submit").disabled = true;
    showStep("setup", "restore");
    restoreGrid.inputs[0].focus();
});

// --- Шаг 2: фраза ---
function renderPhrase() {
    var grid = $("setup-phrase-grid");
    grid.innerHTML = "";
    phraseWords(phraseLang).forEach(function (w, i) {
        var cell = document.createElement("div");
        cell.className = "phrase-cell";
        cell.innerHTML = '<span class="phrase-num"></span><span class="phrase-word"></span>';
        cell.firstChild.textContent = String(i + 1);
        cell.lastChild.textContent = w;
        grid.appendChild(cell);
    });
    $("setup-lang-ru").setAttribute("aria-pressed", phraseLang === "ru" ? "true" : "false");
    $("setup-lang-en").setAttribute("aria-pressed", phraseLang === "en" ? "true" : "false");
}

$("setup-lang-ru").addEventListener("click", function () { phraseLang = "ru"; renderPhrase(); });
$("setup-lang-en").addEventListener("click", function () { phraseLang = "en"; renderPhrase(); });

$("setup-phrase-copy").addEventListener("click", async function () {
    if (!phrase) return;
    // Через Go: буфер очищается через минуту, если в нём всё ещё фраза.
    try {
        await svc("CopyMnemonic", phrase[phraseLang]);
    } catch (err) {
        flog("ERR", "frontend.setup", "CopyMnemonic failed");
        return;
    }
    var label = $("setup-phrase-copy-label");
    label.textContent = t("setup.phrase.copied");
    setTimeout(function () { label.textContent = t("setup.phrase.copy"); }, 1500);
});

$("setup-phrase-saved").addEventListener("change", function () {
    $("setup-phrase-next").disabled = !this.checked;
});

$("setup-phrase-back").addEventListener("click", function () {
    phrase = null;
    $("setup-phrase-grid").innerHTML = "";
    showStep("setup", "welcome");
});

$("setup-phrase-next").addEventListener("click", function () {
    // Три случайных разных номера по возрастанию.
    var all = [];
    for (var i = 0; i < WORDS; i++) all.push(i);
    var rnd = new Uint32Array(WORDS);
    crypto.getRandomValues(rnd);
    all.sort(function (a, b) { return rnd[a] - rnd[b]; });
    confirmIdx = all.slice(0, 3).sort(function (a, b) { return a - b; });
    renderConfirm();
    showStep("setup", "confirm");
    $("setup-confirm-rows").querySelector("input").focus();
});

// --- Шаг 3: проверка слов ---
function renderConfirm() {
    $("setup-confirm-desc").textContent = t("setup.confirm.desc")
        .replace("{a}", confirmIdx[0] + 1).replace("{b}", confirmIdx[1] + 1).replace("{c}", confirmIdx[2] + 1);
    $("setup-confirm-error").textContent = "";
    var rows = $("setup-confirm-rows");
    rows.innerHTML = "";
    confirmIdx.forEach(function (idx) {
        var row = document.createElement("div");
        row.className = "confirm-row";
        var lbl = document.createElement("span");
        lbl.className = "setup-lbl";
        lbl.textContent = t("setup.confirm.word") + " " + (idx + 1);
        var cell = document.createElement("label");
        cell.className = "phrase-cell";
        var input = document.createElement("input");
        input.type = "text";
        input.autocomplete = "off";
        input.spellcheck = false;
        input.setAttribute("aria-label", lbl.textContent);
        input.dataset.idx = String(idx);
        input.addEventListener("input", updateConfirm);
        input.addEventListener("keydown", function (e) {
            if (e.key === "Enter" && !$("setup-confirm-create").disabled) $("setup-confirm-create").click();
        });
        cell.appendChild(input);
        row.appendChild(lbl);
        row.appendChild(cell);
        rows.appendChild(row);
    });
    $("setup-confirm-create").disabled = true;
}

function updateConfirm() {
    var ok = true;
    $("setup-confirm-rows").querySelectorAll("input").forEach(function (input) {
        var idx = Number(input.dataset.idx);
        var v = normWord(input.value);
        var match = v !== "" && (v === phraseWords("ru")[idx] || v === phraseWords("en")[idx]);
        input.parentNode.classList.toggle("ok", match);
        if (!match) ok = false;
    });
    $("setup-confirm-create").disabled = !ok;
}

$("setup-confirm-show").addEventListener("click", function () { showStep("setup", "phrase"); });
$("setup-confirm-back").addEventListener("click", function () { showStep("setup", "phrase"); });

$("setup-confirm-create").addEventListener("click", function () {
    if (!phrase) return;
    var value = phrase.en;
    var fromRecovery = setupDiscardBackup;
    applyPhrase(value, fromRecovery, function (e) {
        $("setup-confirm-error").textContent = e.text || t("setup.err.generic");
    });
});

// --- Восстановление по фразе ---
var restoreGrid = buildInputGrid($("setup-restore-grid"), function () {
    $("setup-restore-error").textContent = "";
    $("setup-restore-submit").disabled = !restoreGrid.filled();
});

$("setup-restore-back").addEventListener("click", function () {
    restoreGrid.clear();
    showStep("setup", "welcome");
});

$("setup-restore-submit").addEventListener("click", function () {
    applyPhrase(restoreGrid.value(), false, function (e) {
        $("setup-restore-error").textContent = e.mismatch ? t("recovery.mismatch") : (e.text || t("setup.err.generic"));
        if (e.word !== undefined) restoreGrid.markBad(e.word);
    });
});

// --- «Ключ базы утерян» ---
var setupDiscardBackup = false;

var recoveryGrid = buildInputGrid($("recovery-grid"), function () {
    $("recovery-error").textContent = "";
    $("recovery-restore-btn").disabled = !recoveryGrid.filled();
});

$("recovery-restore-btn").addEventListener("click", function () {
    applyPhrase(recoveryGrid.value(), false, function (e) {
        if (e.mismatch) {
            showStep("recovery", "mismatch");
            return;
        }
        $("recovery-error").textContent = e.text || t("setup.err.generic");
        if (e.word !== undefined) recoveryGrid.markBad(e.word);
    });
});

$("recovery-other-btn").addEventListener("click", function () {
    recoveryGrid.clear();
    $("recovery-restore-btn").disabled = true;
    showStep("recovery", "phrase");
    recoveryGrid.inputs[0].focus();
});

$("recovery-discard-btn").addEventListener("click", function () {
    applyPhrase(recoveryGrid.value(), true, function (e) {
        showStep("recovery", "phrase");
        $("recovery-error").textContent = e.text || t("setup.err.generic");
    });
});

// Новый профиль вместо утерянного: обычный поток создания, старая история удаляется.
$("recovery-fresh-btn").addEventListener("click", function () {
    setupDiscardBackup = true;
    showScreen("setup");
    showStep("setup", "welcome");
    $("setup-create-btn").click();
});
