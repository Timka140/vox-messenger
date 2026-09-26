"use strict";

// offcanvas.js — nav слева, кнопка голосового канала в шапке, «+» (новая
// группа / голосовой канал / найти друга) и диалоги создания. Правая панель
// переехала в info-panel.js (Инфо) и info-media.js (Медиа).

import { svc, flog, state, dom } from "./core.js";
import { t } from "./i18n.js";
import { showSettings } from "./settings.js";
import { refreshPeers, showGroupsList, showMessagesList, showVoiceChannels, showMeshList, showContactsMode } from "./contacts-list.js";
import { showContextMenu } from "./contextmenu.js";
import { buildDialogOverlay, buildFriendCheckboxList, getSelectedIds } from "./ui.js";
import { renderUserBar, endCall } from "./calls.js";
import { applyGroupVolumes } from "./peer-volume.js";
import { voiceState } from "./voice-state.js";
import { showError } from "./toast.js";
import { stopScreenShareIfActive } from "./screenshare.js";
import { selectPeer } from "./contacts-list.js";
import { applyAvatar } from "./avatar.js";
import { escapeHtml } from "./ui.js";
import "./user-menu.js"; // меню своего аватара + Ctrl+, (фаза 6a)

// --- Nav Strip (68px с подписями; Настройки — оверлей, остальное — режим левой панели) ---
var navButtons = document.querySelectorAll(".nav-item");
navButtons.forEach(function (btn) {
    btn.addEventListener("click", function () {
        var target = btn.dataset.nav;
        if (target === "settings") {
            showSettings();
            return;
        }
        navButtons.forEach(function (b) { b.classList.remove("active"); });
        btn.classList.add("active");

        if (target === "messages") {
            showMessagesList();
        } else if (target === "groups") {
            showGroupsList();
        } else if (target === "contacts") {
            showContactsMode();
        } else if (target === "mesh") {
            showMeshList();
        }
    });
});

// --- Voice header button (audio-lines в chat header; строка канала в панели жмёт её же) ---
var voiceHeaderBtn = document.getElementById("voice-join-btn");
var _voiceHeaderBusy = false;
if (voiceHeaderBtn) {
    voiceHeaderBtn.addEventListener("click", async function () {
        // Guard от двойного клика (Join+Leave вперемешку) и фиксируем группу:
        // за время await пользователь мог переключить чат.
        if (_voiceHeaderBusy) return;
        var gid = state.selectedGroupID;
        if (!gid) return;
        _voiceHeaderBusy = true;
        voiceHeaderBtn.disabled = true;
        try {
            var ag = await svc("GetActiveVoiceGroup").catch(function () { return ""; });
            if (ag === gid) {
                // Уже в канале → выйти (свою демонстрацию гасим сами:
                // бэкенд её при выходе не останавливает)
                stopScreenShareIfActive();
                await svc("LeaveGroupVoice");
                voiceHeaderBtn.classList.remove("active");
                voiceState.resetSelf(); // observer обновит кнопки
                document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
                renderUserBar();
            } else {
                // Если активен звонок → завершить перед переходом в канал
                if (state.call) {
                    await svc("LeaveCall").catch(function () {});
                    state.call = null; // prevent endCall from calling LeaveCall again
                    endCall();
                }
                // Не в канале → войти
                await svc("JoinGroupVoice", gid);
                voiceHeaderBtn.classList.add("active");
                voiceState.resetSelf(); // при join всегда unmuted/undeafened
                // Восстановить сохранённые громкости/mute для пиров
                applyGroupVolumes(gid);
                document.dispatchEvent(new CustomEvent("vox:ui-stage-refresh"));
                renderUserBar();
            }
            document.dispatchEvent(new CustomEvent("vox:ui-voice-changed")); // info-panel: строка канала
        } catch (err) {
            console.error("voice header btn:", err);
            showError(String(err && err.message || err), { title: t("voice.join_failed") });
        } finally {
            _voiceHeaderBusy = false;
            voiceHeaderBtn.disabled = false;
        }
    });
}

// --- Share Contact ---
dom.shareContactBtn.addEventListener("click", async function () {
    try {
        var uri = await svc("GetMyContactURI");
        if (uri) {
            await navigator.clipboard.writeText(uri);
            flog("INF", "frontend", "contact URI copied to clipboard");
        }
    } catch (err) {
        console.error("share contact:", err);
    }
});

// --- Compose: меню создания (гамбургер-меню удалено) ---
dom.composeBtn.addEventListener("click", function (e) {
    e.stopPropagation();
    var r = dom.composeBtn.getBoundingClientRect();
    showContextMenu({ clientX: r.left, clientY: r.bottom + 2, preventDefault: function () {}, stopPropagation: function () {} }, [
        { label: t("compose.new_message"), icon: "message-square-plus", action: function () { newMessageDialog(); } },
        { label: t("compose.new_group"), icon: "users", action: function () { createGroupDialog(); } },
        { label: t("compose.new_voice"), icon: "audio-lines", action: function () { createVoiceChannelDialog(); } },
        { separator: true },
        { label: t("compose.find_friend"), icon: "user-plus", action: function () {
            var contactsNav = document.querySelector('.nav-item[data-nav="contacts"]');
            if (contactsNav) contactsNav.click();
            var add = document.getElementById("contacts-add-btn");
            if (add) add.click();
        } },
    ]);
});

// --- «Новое сообщение» (макет NavMap): выбрать друга → открыть переписку ---

async function newMessageDialog() {
    var friends = [];
    try { friends = (await svc("GetFriends")) || []; } catch (e) { friends = []; }
    var d = buildDialogOverlay(t("compose.new_message"), "", t("dialog.close"), null, null);
    if (friends.length === 0) {
        d.body.innerHTML = '<p class="nm-empty">' + escapeHtml(t("compose.no_friends")) + '</p>';
        return;
    }
    var search = document.createElement("input");
    search.type = "text";
    search.className = "nm-search";
    search.placeholder = t("compose.search_friends");
    var list = document.createElement("div");
    list.className = "nm-list";
    d.body.append(search, list);

    function open(f) {
        d.overlay.remove();
        showMessagesList();
        var p = (state.peers || []).find(function (x) { return x.id === f.peerId; }) ||
            { id: f.peerId, nickname: f.nickname, shortId: f.peerId.substring(0, 16), online: !!f.online };
        selectPeer(f.peerId, p);
    }
    function render() {
        var q = search.value.trim().toLowerCase();
        list.innerHTML = "";
        friends.filter(function (f) { return !q || (f.nickname || "").toLowerCase().indexOf(q) >= 0; }).forEach(function (f) {
            var row = document.createElement("button");
            row.type = "button";
            row.className = "nm-row";
            var av = document.createElement("span");
            av.className = "nm-av";
            applyAvatar(av, f.peerId, f.nickname || f.peerId, { letters: 1 });
            var name = document.createElement("span");
            name.className = "nm-name";
            name.textContent = f.nickname || f.peerId.substring(0, 16);
            var st = document.createElement("span");
            st.className = "nm-status" + (f.online ? " on" : "");
            st.textContent = f.online ? t("contacts.online") : t("contacts.offline");
            row.append(av, name, st);
            row.addEventListener("click", function () { open(f); });
            list.appendChild(row);
        });
    }
    search.addEventListener("input", render);
    search.addEventListener("keydown", function (e) {
        if (e.key === "Enter") { var first = list.querySelector(".nm-row"); if (first) first.click(); }
    });
    render();
}

// --- Create Group / Voice Channel Dialog (unified) ---

async function createGroupDialog() {
    showCreateDialog(0);
}

async function createVoiceChannelDialog() {
    showCreateDialog(1);
}

async function showCreateDialog(groupType) {
    var isVoice = groupType === 1;
    var title = isVoice ? t("voice.new_channel") : t("group.new_title");
    var namePlaceholder = isVoice ? t("voice.channel_placeholder") : t("group.name_placeholder");
    var createLabel = isVoice ? t("group.create") : t("group.create");

    try {
        var friends = await svc("GetFriends") || [];

        var body =
            '<div class="settings-field"><label>' + (isVoice ? t("voice.channel_name") : t("group.group_name")) + '</label>' +
            '<input type="text" class="settings-input dialog-name-input" placeholder="' + namePlaceholder + '" maxlength="32" autocomplete="off"></div>';

        var membersList = buildFriendCheckboxList(friends, {
            onSelectionChange: function (count) {
                if (dlg.confirmBtn) {
                    dlg.confirmBtn.textContent = createLabel + (count > 0 ? " (" + count + ")" : "");
                }
            }
        });

        var dlg = buildDialogOverlay(title, body, t("group.cancel"), createLabel, async function (overlay) {
            var nameInput = overlay.querySelector(".dialog-name-input");
            var name = nameInput.value.trim();
            if (!name) return;
            var selected = getSelectedIds(membersList);
            try {
                if (isVoice) {
                    await svc("CreateVoiceChannel", name, selected);
                } else {
                    await svc("CreateGroup", name, selected, 0);
                }
                overlay.remove();
                refreshPeers();
                if (isVoice) showVoiceChannels();
            } catch (err) {
                svc("Notify", "Error", err.message || String(err)).catch(function () {});
            }
        });

        dlg.dialog.classList.add("dialog-create-group");

        var selectLabel = document.createElement("div");
        selectLabel.className = "dialog-section-label";
        selectLabel.textContent = isVoice ? t("voice.select_friends") : t("group.select_friends");
        if (friends.length > 0) {
            var totalSpan = document.createElement("span");
            totalSpan.className = "dialog-section-count";
            totalSpan.textContent = String(friends.length);
            selectLabel.appendChild(totalSpan);
        }
        dlg.body.appendChild(selectLabel);
        if (friends.length === 0) {
            var noFriends = document.createElement("div");
            noFriends.className = "dialog-hint";
            noFriends.textContent = t("group.no_friends");
            dlg.body.appendChild(noFriends);
        } else {
            dlg.body.appendChild(membersList);
        }
        if (window.lucide) lucide.createIcons({ nodes: [membersList] });

        // Create активна только с именем — иначе клик молча ничего не делал.
        var nameInput = dlg.overlay.querySelector(".dialog-name-input");
        var syncCreate = function () { dlg.confirmBtn.disabled = !nameInput.value.trim(); };
        nameInput.addEventListener("input", syncCreate);
        syncCreate();
        nameInput.focus();
    } catch (err) {
        console.error("create dialog:", err);
    }
}
