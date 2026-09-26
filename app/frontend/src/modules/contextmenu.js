// contextmenu.js — universal right-click context menu
// Usage: showContextMenu(event, [{label, icon, action, danger?, separator?}])

const menu = document.getElementById("context-menu");

document.addEventListener("click", () => hide());
document.addEventListener("contextmenu", (e) => {
    // Close existing menu when opening new one elsewhere
    if (!menu.contains(e.target)) hide();
});
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") hide();
});

function hide() {
    menu.style.display = "none";
    menu.innerHTML = "";
}

/**
 * Show context menu at cursor position.
 * @param {MouseEvent} event — the contextmenu event
 * @param {Array} items — [{label, icon?, action, danger?, separator?, volume?, hint?, sub?, toggle?}]
 *   - hint: горячая клавиша справа; sub: текущее значение справа;
 *     toggle: true/false — переключатель справа (клик зовёт action)
 *   - label: display text
 *   - icon: lucide icon name (optional)
 *   - action: function to call on click
 *   - danger: if true, styled red
 *   - separator: if true, renders a separator line (other fields ignored)
 *   - volume: {value, min, max, onChange} — renders a volume slider row
 */
export function showContextMenu(event, items) {
    event.preventDefault();
    event.stopPropagation();

    menu.innerHTML = "";

    for (const item of items) {
        // header: {node} — шапка меню (меню своего аватара: фото, ник, тег)
        if (item.header) {
            item.header.node.addEventListener("click", (e) => e.stopPropagation());
            menu.appendChild(item.header.node);
            continue;
        }
        if (item.separator) {
            const sep = document.createElement("div");
            sep.className = "context-menu-sep";
            menu.appendChild(sep);
            continue;
        }

        // Volume slider row
        if (item.volume) {
            const row = document.createElement("div");
            row.className = "ctx-volume-row";

            const lbl = document.createElement("label");
            lbl.textContent = item.label || "";
            row.appendChild(lbl);

            const slider = document.createElement("input");
            slider.type = "range";
            slider.className = "ctx-volume-slider";
            slider.min = item.volume.min != null ? item.volume.min : 0;
            slider.max = item.volume.max != null ? item.volume.max : 200;
            slider.value = item.volume.value != null ? item.volume.value : 100;
            row.appendChild(slider);

            const val = document.createElement("span");
            val.className = "ctx-volume-value";
            val.textContent = slider.value + "%";
            row.appendChild(val);

            slider.addEventListener("input", () => {
                val.textContent = slider.value + "%";
                if (item.volume.onChange) item.volume.onChange(parseInt(slider.value));
            });

            // Don't close menu when interacting with slider
            row.addEventListener("click", (e) => e.stopPropagation());
            row.addEventListener("mousedown", (e) => e.stopPropagation());

            menu.appendChild(row);
            continue;
        }

        const el = document.createElement("div");
        el.className = "context-menu-item" + (item.danger ? " danger" : "");

        if (item.icon) {
            const icon = document.createElement("i");
            icon.setAttribute("data-lucide", item.icon);
            icon.className = "icon-16";
            el.appendChild(icon);
        } else if (item.swatch) {
            // swatch — кружок цвета вместо иконки («Цвет чата», фаза 7a)
            const sw = document.createElement("span");
            sw.className = "context-menu-swatch";
            sw.style.background = item.swatch;
            el.appendChild(sw);
        }

        const label = document.createElement("span");
        label.textContent = item.label;
        el.appendChild(label);

        // hint — горячая клавиша справа серым (макет ContextMenus: Ctrl+C, ↑)
        if (item.hint) {
            const hint = document.createElement("span");
            hint.className = "context-menu-hint";
            hint.textContent = item.hint;
            el.appendChild(hint);
        }
        // sub — текущее значение справа (макет HeaderMenus: «7», «мятный»)
        if (item.sub) {
            const sub = document.createElement("span");
            sub.className = "context-menu-sub";
            sub.textContent = item.sub;
            el.appendChild(sub);
        }
        // toggle — состояние пункта-переключателя (Без звука, Голосовой канал)
        if (item.toggle !== undefined) {
            const tg = document.createElement("span");
            tg.className = "context-menu-toggle" + (item.toggle ? " on" : "");
            el.appendChild(tg);
        }

        el.addEventListener("click", () => {
            hide();
            item.action();
        });

        menu.appendChild(el);
    }

    // Render lucide icons in the menu
    if (window.lucide) lucide.createIcons({ nodes: [menu] });

    // Position menu
    menu.style.display = "block";

    const rect = menu.getBoundingClientRect();
    let x = event.clientX;
    let y = event.clientY;

    // Keep within viewport
    if (x + rect.width > window.innerWidth) {
        x = window.innerWidth - rect.width - 4;
    }
    if (y + rect.height > window.innerHeight) {
        y = window.innerHeight - rect.height - 4;
    }

    menu.style.left = x + "px";
    menu.style.top = y + "px";
}
