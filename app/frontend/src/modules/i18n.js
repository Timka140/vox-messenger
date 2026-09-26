// i18n — lightweight internationalization module.
// Usage: import { t, setLocale } from "./i18n.js";
//        t("chat.empty.title") → "Select a conversation"

var strings = {};
var currentLocale = "en";

// t returns the translated string for a key, or the key itself as fallback.
export function t(key) {
    return strings[key] || key;
}

// getLocale returns the current locale code.
export function getLocale() {
    return currentLocale;
}

// setLocale loads a locale JSON file and applies translations to [data-i18n] elements.
export async function setLocale(lang) {
    try {
        var resp = await fetch("/src/i18n/" + lang + ".json");
        if (!resp.ok) return;
        strings = await resp.json();
        currentLocale = lang;
        localStorage.setItem("vox-locale", lang);
        applyDOM();
    } catch (err) {
        console.error("i18n: failed to load locale", lang, err);
    }
}

// applyDOM sets textContent/placeholder/title on all elements with data-i18n attributes.
export function applyDOM() {
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
        var key = el.getAttribute("data-i18n");
        var val = strings[key];
        if (!val) return;
        el.textContent = val;
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach(function (el) {
        var key = el.getAttribute("data-i18n-placeholder");
        var val = strings[key];
        if (val) el.placeholder = val;
    });
    document.querySelectorAll("[data-i18n-title]").forEach(function (el) {
        var key = el.getAttribute("data-i18n-title");
        var val = strings[key];
        if (val) el.title = val;
    });
}

// initLocale loads the saved locale or falls back to "en".
export async function initLocale() {
    var saved = localStorage.getItem("vox-locale") || "en";
    await setLocale(saved);
}
