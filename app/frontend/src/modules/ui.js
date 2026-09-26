"use strict";

// ui.js — Shared UI builder module.
// Provides reusable DOM construction functions for dialogs, friend lists,
// and group context menus. Eliminates duplication across chat/offcanvas/contacts.

import { colorFromId } from "./core.js";
import { applyAvatar } from "./avatar.js";
import { t } from "./i18n.js";

// escapeHtml экранирует недоверенный текст перед вставкой в innerHTML.
export function escapeHtml(s) {
    if (s === null || s === undefined) return "";
    return String(s).replace(/[&<>"']/g, function (c) {
        return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
}

// --- Dialog overlay ---

// buildDialogOverlay creates a modal dialog with title, body HTML, and action buttons.
// Markup: .backup-overlay > .backup-dialog > [.dialog-header, .dialog-body, .backup-actions].
// Returns { overlay, dialog, body, confirmBtn, cancelBtn } — body is the content slot.
// Backwards compat: callers that insert before .backup-actions still work; new callers
// should append into `body` directly.
//
// opts (каркас макета Dialogs, фаза 5b): wide — ширина 560 вместо 420;
// danger — основная кнопка красная; noHeader — без шапки (подтверждение);
// className — доп. класс диалога; onClose — вызывается один раз при любом
// закрытии (Esc, фон, крестик, отмена, подтверждение с удалением overlay).
export function buildDialogOverlay(title, bodyHTML, cancelLabel, confirmLabel, onConfirm, opts) {
    opts = opts || {};
    var overlay = document.createElement("div");
    overlay.className = "backup-overlay";

    var dialog = document.createElement("div");
    dialog.className = "backup-dialog" + (opts.wide ? " dialog-wide" : "") + (opts.className ? " " + opts.className : "");

    var actionsHTML = '<div class="backup-actions">' +
        '<button class="backup-copy-btn" type="button">' + cancelLabel + "</button>";
    if (confirmLabel) {
        actionsHTML += '<button class="backup-done-btn' + (opts.danger ? " danger" : "") + '" type="button">' + confirmLabel + "</button>";
    }
    actionsHTML += "</div>";

    // title экранируем: часть вызывающих (mesh longName, имена групп) передают
    // недоверенный ввод. body — на ответственности вызывающего (многие уже
    // экранируют или строят из createElement).
    dialog.innerHTML =
        '<div class="dialog-header">' +
            '<h3>' + escapeHtml(title) + '</h3>' +
            '<button class="dialog-close-btn" type="button" aria-label="Close">' +
                '<i data-lucide="x" class="icon-18"></i>' +
            '</button>' +
        '</div>' +
        '<div class="dialog-body">' + (bodyHTML || "") + '</div>' +
        actionsHTML;

    overlay.appendChild(dialog);
    document.body.appendChild(overlay);

    if (window.lucide) lucide.createIcons({ nodes: [dialog.querySelector(".dialog-header")] });

    var cancelBtn = dialog.querySelector(".backup-copy-btn");
    var confirmBtn = dialog.querySelector(".backup-done-btn");
    var closeBtn = dialog.querySelector(".dialog-close-btn");
    var body = dialog.querySelector(".dialog-body");
    var actions = dialog.querySelector(".backup-actions");
    // Подтверждение по макету — без шапки: вопрос жирным в теле.
    if (opts.noHeader) {
        dialog.querySelector(".dialog-header").remove();
        closeBtn = null;
    }

    var finished = false;
    function finish() {
        if (finished) return;
        finished = true;
        if (opts.onClose) opts.onClose();
    }

    function close() {
        document.removeEventListener("keydown", onKeydown);
        overlay.remove();
        finish();
    }

    function onKeydown(e) {
        // Вызывающие часто убирают overlay напрямую (overlay.remove() в
        // onConfirm) — без этой проверки слушатель оставался на document
        // навсегда и копился с каждым открытым диалогом.
        if (!overlay.isConnected) {
            document.removeEventListener("keydown", onKeydown);
            finish();
            return;
        }
        if (e.key === "Escape") {
            e.stopPropagation();
            close();
        }
    }
    document.addEventListener("keydown", onKeydown);

    // Начальный фокус: первое текстовое поле, иначе кнопка подтверждения/
    // отмены — чтобы Tab/Enter/Escape работали сразу после открытия.
    setTimeout(function () {
        if (!overlay.isConnected) return;
        var active = document.activeElement;
        if (active && active !== document.body && overlay.contains(active)) return;
        var first = dialog.querySelector(
            "input:not([type=hidden]):not([type=checkbox]):not([type=radio]), textarea, select");
        try { (first || confirmBtn || cancelBtn).focus(); } catch (_) {}
    }, 30);

    overlay.addEventListener("click", function (e) {
        if (e.target === overlay) close();
    });
    if (closeBtn) closeBtn.addEventListener("click", close);
    cancelBtn.addEventListener("click", close);

    if (confirmBtn && onConfirm) {
        confirmBtn.addEventListener("click", function () {
            onConfirm(overlay);
            if (!overlay.isConnected) {
                document.removeEventListener("keydown", onKeydown);
                finish();
            }
        });
    }

    return { overlay: overlay, dialog: dialog, body: body, actions: actions, confirmBtn: confirmBtn, cancelBtn: cancelBtn };
}

// confirmDialog — один шаблон для деструктивного (макет Dialogs
// «Подтверждение»): без шапки, заголовок-вопрос жирным, пояснение, красная
// кнопка справа. opts.danger=false — обычная кнопка.
export function confirmDialog(title, text, confirmLabel, onConfirm, opts) {
    opts = opts || {};
    var dlg = buildDialogOverlay(title, "", t("dialog.cancel"), confirmLabel, function (overlay) {
        overlay.remove();
        onConfirm();
    }, { noHeader: true, danger: opts.danger !== false, className: "dialog-confirm" });
    var h = document.createElement("div");
    h.className = "dialog-confirm-title";
    h.textContent = title;
    var p = document.createElement("p");
    p.className = "dialog-confirm-text";
    p.textContent = text;
    dlg.body.appendChild(h);
    dlg.body.appendChild(p);
    return dlg;
}

// emptyState — пустое состояние (макет EmptyStates): иконка в цвете рамки,
// заголовок, одна строка объяснения и не больше одного действия.
//   action: { label, icon?, onClick }
export function emptyState(icon, title, text, action) {
    var box = document.createElement("div");
    box.className = "empty-state";
    box.innerHTML = '<i data-lucide="' + escapeHtml(icon) + '" class="es-icon"></i><div class="es-title"></div>' +
        (text ? '<div class="es-text"></div>' : "");
    box.querySelector(".es-title").textContent = title;
    if (text) box.querySelector(".es-text").textContent = text;
    if (action) {
        var b = document.createElement("button");
        b.type = "button";
        b.className = "es-btn";
        if (action.icon) b.innerHTML = '<i data-lucide="' + escapeHtml(action.icon) + '" class="icon-16"></i>';
        var s = document.createElement("span");
        s.textContent = action.label;
        b.appendChild(s);
        b.addEventListener("click", action.onClick);
        box.appendChild(b);
    }
    // Lucide не рисует в узле вне документа — иконки после вставки.
    requestAnimationFrame(function () {
        if (window.lucide && box.isConnected) lucide.createIcons({ nodes: [box] });
    });
    return box;
}

// --- Friend checkbox list (with search + chips) ---

// buildFriendCheckboxList creates a searchable list of friend cards with checkboxes.
// options.onSelectionChange(count) is called when selection changes.
// Returns a container; getSelectedIds() works on it unchanged.
// Supports lazy rendering for 1000+ friends.
export function buildFriendCheckboxList(friends, options) {
    var opts = options || {};
    var PAGE_SIZE = 50;
    var wrapper = document.createElement("div");
    wrapper.className = "dialog-member-list-container";

    // Search input + leading icon
    var searchWrapper = document.createElement("div");
    searchWrapper.className = "dialog-search-wrapper";
    var searchIcon = document.createElement("i");
    searchIcon.setAttribute("data-lucide", "search");
    searchIcon.className = "icon-16";
    var searchInput = document.createElement("input");
    searchInput.type = "text";
    searchInput.className = "dialog-search-input";
    searchInput.placeholder = t("dialog.search_placeholder");
    searchInput.autocomplete = "off";
    searchWrapper.appendChild(searchIcon);
    searchWrapper.appendChild(searchInput);
    wrapper.appendChild(searchWrapper);

    // Chips area
    var chipsArea = document.createElement("div");
    chipsArea.className = "dialog-chips-area";
    wrapper.appendChild(chipsArea);

    // Сводка выбора «N выбрано · Сбросить»: при большом списке чипы уезжают
    // за max-height, а счётчик и сброс должны быть всегда на виду.
    var metaRow = document.createElement("div");
    metaRow.className = "dialog-select-meta";
    metaRow.hidden = true;
    var metaCount = document.createElement("span");
    metaCount.className = "dialog-select-count";
    var metaClear = document.createElement("button");
    metaClear.type = "button";
    metaClear.className = "dialog-select-clear";
    metaClear.textContent = t("dialog.clear_selection");
    metaRow.appendChild(metaCount);
    metaRow.appendChild(metaClear);
    wrapper.appendChild(metaRow);

    // Scrollable list
    var list = document.createElement("div");
    list.className = "dialog-member-list";
    wrapper.appendChild(list);

    // Empty state
    var emptyState = document.createElement("div");
    emptyState.className = "dialog-empty-state";
    emptyState.innerHTML = '<i data-lucide="user-x" class="icon-empty" style="width:32px;height:32px;"></i><span>' + t("dialog.no_results") + '</span>';
    emptyState.style.display = "none";
    list.appendChild(emptyState);

    // Prepare data with precomputed display info
    // Детектим дубликаты ника — чтобы юзер не путал разные устройства одного контакта.
    var nickCounts = {};
    for (var n = 0; n < friends.length; n++) {
        var nkey = (friends[n].nickname || "").toLowerCase();
        if (nkey) nickCounts[nkey] = (nickCounts[nkey] || 0) + 1;
    }

    var allItems = [];
    for (var i = 0; i < friends.length; i++) {
        var f = friends[i];
        var dn = f.nickname || f.peerId.substring(0, 16);
        var nkeyI = (f.nickname || "").toLowerCase();
        var duplicate = nkeyI && nickCounts[nkeyI] > 1;
        allItems.push({
            friend: f,
            displayName: dn,
            subtitle: duplicate ? f.peerId.substring(0, 16) : "",
            online: !!f.online,
            initials: dn.substring(0, 2).toUpperCase(),
            color: colorFromId(f.peerId),
            searchName: dn.toLowerCase(),
            label: null // DOM element, created lazily
        });
    }

    // Онлайн сверху, при равенстве — по нику; дубликат-офлайн не маскирует онлайн-версию.
    allItems.sort(function (a, b) {
        if (a.online !== b.online) return a.online ? -1 : 1;
        return a.searchName.localeCompare(b.searchName);
    });

    var filtered = allItems; // current filtered view
    var rendered = 0; // how many of filtered are rendered

    function createLabel(item) {
        var f = item.friend;
        var label = document.createElement("label");
        label.className = "friend-checkbox-label" + (item.online ? "" : " is-offline");
        label.setAttribute("data-name", item.searchName);
        label.setAttribute("data-peer-id", f.peerId);

        var avatar = document.createElement("div");
        avatar.className = "friend-checkbox-avatar";
        applyAvatar(avatar, f.peerId, item.displayName);
        // Статус — бейджем на аватаре, как в списке контактов, а не точкой
        // перед именем.
        var statusDot = document.createElement("span");
        statusDot.className = "status-dot friend-avatar-status " + (item.online ? "online" : "offline");
        avatar.appendChild(statusDot);

        var info = document.createElement("div");
        info.className = "friend-checkbox-info";

        var nameRow = document.createElement("div");
        nameRow.className = "friend-checkbox-name-row";
        var nameSpan = document.createElement("span");
        nameSpan.className = "friend-checkbox-name";
        nameSpan.textContent = item.displayName;
        nameRow.appendChild(nameSpan);
        info.appendChild(nameRow);

        // Подзаголовок: у дубликата ника — peerId (различить устройства),
        // иначе текстовый статус.
        var subSpan = document.createElement("span");
        subSpan.className = "friend-checkbox-subtitle" + (item.subtitle ? " is-mono" : "");
        subSpan.textContent = item.subtitle || (item.online ? t("chat.online") : t("chat.offline")).toLowerCase();
        info.appendChild(subSpan);

        var cb = document.createElement("input");
        cb.type = "checkbox";
        cb.value = f.peerId;
        cb.className = "friend-checkbox-input";

        // Визуальный чекбокс (нативный скрыт, но остаётся для клика по label
        // и клавиатуры): кружок → залитый accent с галочкой.
        var check = document.createElement("span");
        check.className = "friend-check";
        check.setAttribute("aria-hidden", "true");

        // Restore checked state if previously selected
        if (selectedSet[f.peerId]) {
            cb.checked = true;
            label.classList.add("selected");
        }

        label.appendChild(avatar);
        label.appendChild(info);
        label.appendChild(cb);
        label.appendChild(check);

        cb.addEventListener("change", function () {
            if (cb.checked) {
                label.classList.add("selected");
                selectedSet[f.peerId] = true;
                addChip(f.peerId, item.displayName, item.initials, item.color);
            } else {
                label.classList.remove("selected");
                delete selectedSet[f.peerId];
                removeChip(f.peerId);
            }
            fireChange();
        });

        item.label = label;
        return label;
    }

    // Заголовки секций «online · N» / «offline · N» — только когда в выборке
    // есть и те и другие; список отсортирован online-first, так что граница
    // одна.
    function sectionHeader(online) {
        var count = 0;
        for (var c = 0; c < filtered.length; c++) if (filtered[c].online === online) count++;
        var h = document.createElement("div");
        h.className = "dialog-list-section";
        h.textContent = (online ? t("chat.online") : t("chat.offline")).toLowerCase() + " \u00b7 " + count;
        return h;
    }

    function renderPage() {
        var end = Math.min(rendered + PAGE_SIZE, filtered.length);
        var mixed = filtered.length > 0 && filtered[0].online && !filtered[filtered.length - 1].online;
        for (var j = rendered; j < end; j++) {
            if (mixed && (j === 0 || filtered[j].online !== filtered[j - 1].online)) {
                list.insertBefore(sectionHeader(filtered[j].online), emptyState);
            }
            var label = filtered[j].label || createLabel(filtered[j]);
            label.style.display = "";
            list.insertBefore(label, emptyState);
        }
        rendered = end;
    }

    function rebuildList() {
        // Remove all rows and section headers from DOM (keep emptyState)
        var children = list.querySelectorAll(".friend-checkbox-label, .dialog-list-section");
        for (var j = 0; j < children.length; j++) {
            children[j].remove();
        }
        rendered = 0;
        renderPage();
        emptyState.style.display = filtered.length === 0 ? "" : "none";
        list.scrollTop = 0;
    }

    // Track selected peers by ID (survives re-renders)
    var selectedSet = {};

    function addChip(peerId, name, initials, color) {
        var chip = document.createElement("button");
        chip.type = "button";
        chip.className = "dialog-chip";
        chip.setAttribute("data-peer-id", peerId);

        var chipAvatar = document.createElement("span");
        chipAvatar.className = "dialog-chip-avatar";
        chipAvatar.style.backgroundColor = color;
        chipAvatar.textContent = initials;

        var chipName = document.createTextNode(name);

        var chipRemove = document.createElement("i");
        chipRemove.className = "dialog-chip-remove";
        chipRemove.textContent = "×";

        chip.appendChild(chipAvatar);
        chip.appendChild(chipName);
        chip.appendChild(chipRemove);
        chipsArea.appendChild(chip);

        chip.addEventListener("click", function (e) {
            e.preventDefault();
            delete selectedSet[peerId];
            var lbl = list.querySelector('label[data-peer-id="' + peerId + '"]');
            if (lbl) {
                var box = lbl.querySelector("input[type=checkbox]");
                if (box) box.checked = false;
                lbl.classList.remove("selected");
            }
            chip.remove();
            fireChange();
        });
    }

    function removeChip(peerId) {
        var chip = chipsArea.querySelector('.dialog-chip[data-peer-id="' + peerId + '"]');
        if (chip) chip.remove();
    }

    function fireChange() {
        var count = Object.keys(selectedSet).length;
        metaRow.hidden = count === 0;
        metaCount.textContent = t("dialog.selected_count").replace("{n}", count);
        if (opts.onSelectionChange) opts.onSelectionChange(count);
    }

    function clearSelection() {
        var ids = Object.keys(selectedSet);
        for (var k = 0; k < ids.length; k++) {
            delete selectedSet[ids[k]];
            var lbl = list.querySelector('label[data-peer-id="' + ids[k] + '"]');
            if (lbl) {
                var box = lbl.querySelector("input[type=checkbox]");
                if (box) box.checked = false;
                lbl.classList.remove("selected");
            }
            removeChip(ids[k]);
        }
        fireChange();
    }
    metaClear.addEventListener("click", clearSelection);

    function applyFilter() {
        var query = searchInput.value.trim().toLowerCase();
        if (!query) {
            filtered = allItems;
        } else {
            filtered = allItems.filter(function (item) {
                return item.searchName.indexOf(query) !== -1;
            });
        }
        rebuildList();
    }

    // Search filtering
    searchInput.addEventListener("input", applyFilter);

    // Клавиатура в поиске: Enter — переключить первого найденного (набрал
    // имя → Enter → следующий), Escape при непустом запросе — очистить
    // запрос, а не закрыть диалог.
    searchInput.addEventListener("keydown", function (e) {
        if (e.key === "Enter") {
            e.preventDefault();
            if (filtered.length === 0) return;
            var first = filtered[0].label || createLabel(filtered[0]);
            var box = first.querySelector("input[type=checkbox]");
            if (box) box.click();
            if (searchInput.value) { searchInput.value = ""; applyFilter(); }
        } else if (e.key === "Escape" && searchInput.value) {
            e.preventDefault();
            e.stopPropagation();
            searchInput.value = "";
            applyFilter();
        }
    });

    // Infinite scroll
    list.addEventListener("scroll", function () {
        if (rendered < filtered.length && list.scrollTop + list.clientHeight >= list.scrollHeight - 40) {
            renderPage();
        }
    });

    // Initial render
    renderPage();
    emptyState.style.display = filtered.length === 0 ? "" : "none";

    // Expose selectedSet for getSelectedIds
    wrapper._selectedSet = selectedSet;
    wrapper._total = allItems.length;
    wrapper._searchInput = searchInput;

    return wrapper;
}

// buildFriendClickList creates a searchable list of clickable friend rows (no checkboxes).
// Used for Add Member dialog. options.onClick(peerId) is called on click.
// Supports lazy rendering for 1000+ friends.
export function buildFriendClickList(friends, options) {
    var opts = options || {};
    var PAGE_SIZE = 50;
    var wrapper = document.createElement("div");
    wrapper.className = "dialog-member-list-container";

    // Search input + leading icon
    var searchWrapper = document.createElement("div");
    searchWrapper.className = "dialog-search-wrapper";
    var searchIcon = document.createElement("i");
    searchIcon.setAttribute("data-lucide", "search");
    searchIcon.className = "icon-16";
    var searchInput = document.createElement("input");
    searchInput.type = "text";
    searchInput.className = "dialog-search-input";
    searchInput.placeholder = t("dialog.search_placeholder");
    searchInput.autocomplete = "off";
    searchWrapper.appendChild(searchIcon);
    searchWrapper.appendChild(searchInput);
    wrapper.appendChild(searchWrapper);

    // Scrollable list
    var list = document.createElement("div");
    list.className = "dialog-member-list";
    wrapper.appendChild(list);

    // Empty state
    var emptyState = document.createElement("div");
    emptyState.className = "dialog-empty-state";
    emptyState.innerHTML = '<i data-lucide="user-x" class="icon-empty" style="width:32px;height:32px;"></i><span>' + t("dialog.no_results") + '</span>';
    emptyState.style.display = "none";
    list.appendChild(emptyState);

    // Prepare data — детектим дубликаты ника (разные устройства одного контакта)
    var nickCounts = {};
    for (var n = 0; n < friends.length; n++) {
        var nkey = (friends[n].nickname || "").toLowerCase();
        if (nkey) nickCounts[nkey] = (nickCounts[nkey] || 0) + 1;
    }

    var allItems = [];
    for (var i = 0; i < friends.length; i++) {
        var f = friends[i];
        var dn = f.nickname || f.peerId.substring(0, 16);
        var nkeyI = (f.nickname || "").toLowerCase();
        var duplicate = nkeyI && nickCounts[nkeyI] > 1;
        allItems.push({
            friend: f,
            displayName: dn,
            subtitle: duplicate ? f.peerId.substring(0, 16) : "",
            online: !!f.online,
            searchName: dn.toLowerCase(),
            row: null
        });
    }

    allItems.sort(function (a, b) {
        if (a.online !== b.online) return a.online ? -1 : 1;
        return a.searchName.localeCompare(b.searchName);
    });

    var filtered = allItems;
    var rendered = 0;

    function createRow(item) {
        var f = item.friend;
        var row = document.createElement("div");
        row.className = "friend-checkbox-label" + (item.online ? "" : " is-offline");
        row.setAttribute("data-name", item.searchName);
        row.style.cursor = "pointer";

        var avatar = document.createElement("div");
        avatar.className = "friend-checkbox-avatar";
        applyAvatar(avatar, f.peerId, item.displayName);

        var info = document.createElement("div");
        info.className = "friend-checkbox-info";

        var nameRow = document.createElement("div");
        nameRow.className = "friend-checkbox-name-row";

        var statusDot = document.createElement("span");
        statusDot.className = "status-dot " + (item.online ? "online" : "offline");
        nameRow.appendChild(statusDot);

        var nameSpan = document.createElement("span");
        nameSpan.className = "friend-checkbox-name";
        nameSpan.textContent = item.displayName;
        nameRow.appendChild(nameSpan);

        info.appendChild(nameRow);

        if (item.subtitle) {
            var subSpan = document.createElement("span");
            subSpan.className = "friend-checkbox-subtitle";
            subSpan.textContent = item.subtitle;
            info.appendChild(subSpan);
        }

        var icon = document.createElement("i");
        icon.setAttribute("data-lucide", "user-plus");
        icon.className = "icon-16";

        row.appendChild(avatar);
        row.appendChild(info);
        row.appendChild(icon);

        row.addEventListener("click", function () {
            if (opts.onClick) opts.onClick(f.peerId);
        });

        item.row = row;
        return row;
    }

    function renderPage() {
        var end = Math.min(rendered + PAGE_SIZE, filtered.length);
        var needIcons = false;
        for (var j = rendered; j < end; j++) {
            var row = filtered[j].row || createRow(filtered[j]);
            list.insertBefore(row, emptyState);
            needIcons = true;
        }
        rendered = end;
        if (needIcons && window.lucide) lucide.createIcons({ nodes: [list] });
    }

    function rebuildList() {
        var children = list.querySelectorAll(".friend-checkbox-label");
        for (var j = 0; j < children.length; j++) {
            children[j].remove();
        }
        rendered = 0;
        renderPage();
        emptyState.style.display = filtered.length === 0 ? "" : "none";
    }

    // Search filtering
    searchInput.addEventListener("input", function () {
        var query = searchInput.value.trim().toLowerCase();
        if (!query) {
            filtered = allItems;
        } else {
            filtered = allItems.filter(function (item) {
                return item.searchName.indexOf(query) !== -1;
            });
        }
        rebuildList();
    });

    // Infinite scroll
    list.addEventListener("scroll", function () {
        if (rendered < filtered.length && list.scrollTop + list.clientHeight >= list.scrollHeight - 40) {
            renderPage();
        }
    });

    // Initial render
    renderPage();
    emptyState.style.display = filtered.length === 0 ? "" : "none";

    return wrapper;
}

// getSelectedIds extracts selected peer IDs from a checkbox list container.
// Uses _selectedSet (lazy rendering) if available, falls back to DOM query.
export function getSelectedIds(container) {
    if (container._selectedSet) {
        return Object.keys(container._selectedSet);
    }
    var boxes = container.querySelectorAll("input[type=checkbox]:checked");
    var ids = [];
    for (var i = 0; i < boxes.length; i++) {
        ids.push(boxes[i].value);
    }
    return ids;
}

// --- Group context menu ---

// buildGroupContextMenu returns a context menu items array for a group.
// Creator sees: rename + copy ID + separator + delete.
// Non-creator sees: copy ID + separator + leave.
// callbacks: { onRename(group), onDelete(group), onLeave(group), refreshPeers() }
export function buildGroupContextMenu(group, myID, callbacks) {
    var isCreator = group.creatorId === myID;
    var items = [];

    if (isCreator) {
        items.push({
            label: t("ctx.rename_group"), icon: "pencil", action: function () {
                if (callbacks.onRename) callbacks.onRename(group);
            }
        });
    }

    items.push({
        label: t("ctx.copy_group_id"), icon: "copy", action: function () {
            navigator.clipboard.writeText(group.groupId);
        }
    });

    items.push({ separator: true });

    if (group.left) {
        items.push({
            label: t("ctx.rejoin_group"), icon: "log-in", action: function () {
                if (callbacks.onRejoin) callbacks.onRejoin(group);
            }
        });
        items.push({
            label: t("ctx.delete_left_group"), icon: "trash-2", danger: true, action: function () {
                if (callbacks.onDeleteLeft) callbacks.onDeleteLeft(group);
            }
        });
    } else if (isCreator) {
        items.push({
            label: t("ctx.delete_group"), icon: "trash-2", danger: true, action: function () {
                if (callbacks.onDelete) callbacks.onDelete(group);
            }
        });
    } else {
        items.push({
            label: t("ctx.leave_group"), icon: "log-out", danger: true, action: function () {
                if (callbacks.onLeave) callbacks.onLeave(group);
            }
        });
    }

    return items;
}
