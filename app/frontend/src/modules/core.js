"use strict";

import { Events, Call } from "/wails/runtime.js";
import { t, getLocale } from "./i18n.js";
import { showError } from "./toast.js";

export { Events };

// Method-to-service routing table.
var _routes = {
    // AppService
    LogFrontend: "AppService", Notify: "AppService", IsRecoveryMode: "AppService",
    // IdentityService
    HasIdentity: "IdentityService", LoadIdentity: "IdentityService",
    NewMnemonic: "IdentityService", ApplyMnemonic: "IdentityService", CopyMnemonic: "IdentityService",
    GetMyID: "IdentityService", GetMyShortID: "IdentityService", GetMyTag: "IdentityService",
    GetNickname: "IdentityService", SetNickname: "IdentityService", GetMyContactURI: "IdentityService",
    SaveSetting: "IdentityService", GetSetting: "IdentityService",
    // NotifyService
    ShowToast: "NotifyService",
    // NetworkService
    Connect: "NetworkService", Disconnect: "NetworkService",
    GetSignalServers: "NetworkService", AddSignalServer: "NetworkService", RemoveSignalServer: "NetworkService",
    GetCustomSeeds: "NetworkService",
    // Блокировка (фаза 7b, services/blocklist.go)
    BlockPeer: "NetworkService", UnblockPeer: "NetworkService", GetBlockedPeers: "NetworkService",
    SaveICEServers: "NetworkService", GetICEServers: "NetworkService", GetICEServersSource: "NetworkService", ResetICEServers: "NetworkService",
    // MessagingService
    SendMessage: "MessagingService", SendMessageViaMesh: "MessagingService",
    GetMessages: "MessagingService", GetConversations: "MessagingService",
    MarkMessagesRead: "MessagingService", AddContact: "MessagingService", SearchUser: "MessagingService",
    CancelMessage: "MessagingService",
    // FriendsService
    SendFriendRequest: "FriendsService", AcceptFriendRequest: "FriendsService", RejectFriendRequest: "FriendsService",
    RemoveFriend: "FriendsService", GetPendingRequests: "FriendsService", GetFriends: "FriendsService",
    GetContactSharing: "FriendsService", SetContactSharing: "FriendsService",
    DeleteConversation: "FriendsService",
    DeleteMessage: "MessagingService", DeleteMessageForAll: "MessagingService", EditMessage: "MessagingService",
    // Attachments (Phase 2-5)
    SendAttachments: "MessagingService", SendGroupAttachments: "MessagingService",
    SendAttachmentsByPath: "MessagingService", SendGroupAttachmentsByPath: "MessagingService",
    PickAttachments: "MessagingService", PrepareAttachments: "MessagingService",
    RevokePreviewTokens: "MessagingService",
    GetAttachmentBytes: "MessagingService", DeleteAttachment: "MessagingService",
    CancelAttachmentTransfer: "MessagingService", DeliverPendingMessages: "MessagingService",
    RetryAttachmentSend: "MessagingService",
    SaveAttachmentAs: "MessagingService", OpenAttachment: "MessagingService",
    SaveAllAttachmentsForMessage: "MessagingService", SaveAttachmentsTo: "MessagingService",
    ApproveAttachment: "MessagingService", DeclineAttachment: "MessagingService",
    GetChatMedia: "MessagingService", ForwardAttachment: "MessagingService",
    GetChatLinks: "MessagingService", RevealAttachment: "MessagingService",
    // AvatarService — фото профиля и групп (фаза 5a)
    SetMyAvatar: "AvatarService", ClearMyAvatar: "AvatarService",
    SetGroupAvatar: "AvatarService", ClearGroupAvatar: "AvatarService",
    GetAvatarHashes: "AvatarService",
    // Живые превью экранов в «Показать экран» (фаза 5b)
    StartScreenThumbs: "ScreenShareService", StopScreenThumbs: "ScreenShareService",
    // Звук системы при демонстрации (фаза 5e)
    ScreenAudioSupported: "ScreenShareService", SetScreenShareVolume: "ScreenShareService",
    // Ссылка-приглашение в группу (фаза 5d)
    GetGroupInviteLink: "GroupsService", ResetGroupInviteLink: "GroupsService",
    ParseGroupInviteLink: "GroupsService", JoinGroupByLink: "GroupsService",
    ClearChatMedia: "MessagingService",
    // FilesService — новый CAS pipeline (chunk-addressable, pull-модель, resume).
    SendFilesByPath: "FilesService", SendGroupFilesByPath: "FilesService",
    ApproveFileDownload: "FilesService", DeclineFileDownload: "FilesService",
    StartFileDownload: "FilesService", CancelFileDownload: "FilesService",
    // GroupsService
    CreateGroup: "GroupsService", CreateVoiceChannel: "GroupsService", GetGroups: "GroupsService", RejoinGroup: "GroupsService",
    GetGroupMessages: "GroupsService", SendGroupMessage: "GroupsService", GetGroupMembers: "GroupsService",
    AddMemberToGroup: "GroupsService", LeaveGroup: "GroupsService", DeleteGroupAsAdmin: "GroupsService",
    KickGroupMember: "GroupsService", RenameGroup: "GroupsService", MarkGroupRead: "GroupsService", DeleteLeftGroup: "GroupsService",
    SetVoiceEnabled: "GroupsService",
    GetGroupForceRelay: "GroupsService", SetGroupForceRelay: "GroupsService",
    RetryGroupRelayMessage: "GroupsService",
    GetGroupRelayThreshold: "GroupsService", SetGroupRelayThreshold: "GroupsService",
    // CallsService
    StartCall: "CallsService", AcceptCall: "CallsService", RejectCall: "CallsService",
    LeaveCall: "CallsService", EndCallForAll: "CallsService", KickFromCall: "CallsService",
    AddToCall: "CallsService", GetCallHistory: "CallsService", ClearCallHistory: "CallsService",
    GetActiveCall: "CallsService",
    // VoiceService
    JoinGroupVoice: "VoiceService", LeaveGroupVoice: "VoiceService", GetVoiceParticipants: "VoiceService",
    GetActiveVoiceGroup: "VoiceService", MuteVoice: "VoiceService", UnmuteVoice: "VoiceService",
    GetVoiceData: "VoiceService", SetPeerVolume: "VoiceService", MutePeer: "VoiceService",
    DeafenVoice: "VoiceService", UndeafenVoice: "VoiceService", IsDeafened: "VoiceService",
    SetMasterVoiceVolume: "VoiceService", GetMasterVoiceVolume: "VoiceService",
    GetSFUEnabled: "VoiceService", SetSFUEnabled: "VoiceService",
    GetSFUForce: "VoiceService", SetSFUForce: "VoiceService",
    GetSFUThreshold: "VoiceService", SetSFUThreshold: "VoiceService", GetSFUStatus: "VoiceService",
    GetOpusDTX: "VoiceService", SetOpusDTX: "VoiceService", GetVoiceBitrate: "VoiceService",
    GetVADEnabled: "VoiceService", SetVADEnabled: "VoiceService",
    GetVADThreshold: "VoiceService", SetVADThreshold: "VoiceService",
    GetNeuralMode: "VoiceService", SetNeuralMode: "VoiceService",
    GetNeuralAvailable: "VoiceService",
    GetFastEnhancerSize: "VoiceService", SetFastEnhancerSize: "VoiceService",
    GetFastEnhancerMix: "VoiceService", SetFastEnhancerMix: "VoiceService",
    GetGTCRNVariant: "VoiceService", SetGTCRNVariant: "VoiceService",
    GetGTCRNMix: "VoiceService", SetGTCRNMix: "VoiceService",
    // ScreenShareService
    StartScreenShare: "ScreenShareService", StopScreenShare: "ScreenShareService",
    IsActive: "ScreenShareService", ListDisplays: "ScreenShareService", CaptureScreenshot: "ScreenShareService",
    RequestScreenKeyframe: "ScreenShareService",
    OpenViewerWindow: "ScreenShareService", CloseViewerWindow: "ScreenShareService", ViewerState: "ScreenShareService",
    CloseAllViewerWindows: "ScreenShareService",
    // Лента сообщений (фаза 3a): подгрузка старых, поиск, вставка скриншота
    GetMessagesPage: "MessagingService", SearchMessages: "MessagingService", SavePastedImage: "MessagingService",
    GetGroupMessagesPage: "GroupsService",
    // Ответ, пересылка, «печатает…» (фаза 3b)
    SendMessageWithOptions: "MessagingService", GetMessageOffset: "MessagingService", SendTyping: "MessagingService",
    SendGroupMessageWithOptions: "GroupsService", ForwardFiles: "FilesService", ForwardFileShas: "FilesService",
    // AudioService
    GetMicrophones: "AudioService", GetSpeakers: "AudioService",
    StartMicTest: "AudioService", StopMicTest: "AudioService", GetMicLevel: "AudioService",
    SetMicGain: "AudioService", GetMicGain: "AudioService", SetMicrophone: "AudioService", GetSelectedMicrophone: "AudioService",
    SetSpeaker: "AudioService", GetSelectedSpeaker: "AudioService",
    SetNoiseSuppression: "AudioService", GetNoiseSuppression: "AudioService",
    SetDFNStrength: "AudioService", SetMicMonitor: "AudioService", ResetAudioSettings: "AudioService",
    StartRecording: "AudioService", StopRecording: "AudioService",
    SendVoiceMessage: "AudioService", SendGroupVoiceMessage: "AudioService",
    GetRecordingLevel: "AudioService", GetRecordingDuration: "AudioService", GetRecordingStatus: "AudioService",
    // UpdateService
    GetCurrentVersion: "UpdateService", GetLatestRelease: "UpdateService", CheckForUpdates: "UpdateService",
    DownloadUpdate: "UpdateService", ApplyUpdate: "UpdateService",
    CanRollback: "UpdateService", Rollback: "UpdateService",
    RepairAssets: "UpdateService",
    // MeshtasticService
    IsEnabled: "MeshtasticService", Enable: "MeshtasticService", Disable: "MeshtasticService",
    Status: "MeshtasticService", ListNodes: "MeshtasticService",
    ListSerialPorts: "MeshtasticService", TestConnection: "MeshtasticService",
    SetPortNum: "MeshtasticService", PortNum: "MeshtasticService", DefaultPortNum: "MeshtasticService",
    SendBroadcastText: "MeshtasticService", SendDirectText: "MeshtasticService", ListMeshTexts: "MeshtasticService",
    ListMeshTextsBroadcast: "MeshtasticService", ListMeshTextsForNode: "MeshtasticService",
    GetMeshUnreadCount: "MeshtasticService", MarkMeshTextsRead: "MeshtasticService",
    ClearMeshTexts: "MeshtasticService",
    SetPeerMeshNode: "MeshtasticService", GetPeerMeshNode: "MeshtasticService",
    SetWiFi: "MeshtasticService",
    ListMeshWiFiNetworks: "MeshtasticService",
    SaveMeshWiFiNetwork: "MeshtasticService",
    ForgetMeshWiFiNetwork: "MeshtasticService",
    SetBluetooth: "MeshtasticService",
    SetMeshDeviceConfig: "MeshtasticService",
};

// --- Frontend logging (must be before svc) ---
var _origLog = console.log;
var _origWarn = console.warn;
var _origError = console.error;

export function flog(level, component, msg) {
    svc("LogFrontend", level, component, String(msg)).catch(function () {});
}

// --- Debug: trace all svc calls ---
var _svcCallId = 0;

export function svc(method) {
    var args = [];
    for (var i = 1; i < arguments.length; i++) args.push(arguments[i]);
    var service = _routes[method] || "AppService";

    // Skip logging for LogFrontend to avoid recursion
    if (method === "LogFrontend") {
        return Call.ByName("vox_app/internal/services." + service + "." + method, ...args);
    }

    // Частые polling-вызовы не логируем (забивают консоль)
    var _quietMethods = {
        GetRecordingStatus: 1, GetRecordingLevel: 1, GetRecordingDuration: 1,
        GetMicLevel: 1
    };
    var quiet = _quietMethods[method];
    // Секретная фраза не должна попадать даже в консоль.
    var secret = method === "NewMnemonic" || method === "ApplyMnemonic" || method === "CopyMnemonic";

    var callId = ++_svcCallId;
    var t0 = performance.now();

    if (!quiet) {
        var argsPreview = args.map(function (a) {
            if (secret) return "***";
            if (typeof a === "string" && a.length > 60) return a.substring(0, 60) + "...";
            return a;
        });
        _origLog("[SVC #" + callId + "] → " + service + "." + method + "(" + JSON.stringify(argsPreview) + ")");
    }

    return Call.ByName("vox_app/internal/services." + service + "." + method, ...args)
        .then(function (result) {
            if (!quiet) {
                var dt = (performance.now() - t0).toFixed(1);
                var preview = "";
                try {
                    var s = secret ? "***" : JSON.stringify(result);
                    preview = s && s.length > 120 ? s.substring(0, 120) + "..." : s;
                } catch (e) { preview = String(result); }
                _origLog("[SVC #" + callId + "] ← " + method + " OK (" + dt + "ms) " + preview);
            }
            return result;
        })
        .catch(function (err) {
            var dt = (performance.now() - t0).toFixed(1);
            _origError("[SVC #" + callId + "] ✗ " + method + " FAIL (" + dt + "ms) " + err);
            throw err;
        });
}

// --- Debug: log event helper (called manually in events.js) ---
// В файл идут только поля из белого списка: ники, тексты, SDP с адресами
// в лог не попадают. id собеседника — первые 16 символов.
var _evtSafeKeys = ["id", "msgId", "callId", "groupId", "status", "type", "reason", "count", "size", "video", "busy"];
var _evtPeerKeys = ["peerId", "from", "fromId", "senderId"];
export function logEvent(eventName, data) {
    var parts = [];
    if (data && typeof data === "object") {
        _evtSafeKeys.forEach(function (k) {
            if (data[k] !== undefined && data[k] !== null && typeof data[k] !== "object") parts.push(k + "=" + data[k]);
        });
        _evtPeerKeys.forEach(function (k) {
            if (typeof data[k] === "string") parts.push(k + "=" + data[k].substring(0, 16));
        });
    }
    var preview = parts.join(" ");
    _origLog("[EVT] " + eventName + " " + preview);
    flog("DBG", "frontend.event", eventName + " " + preview);
}

var _origDebug = console.debug;
var _origInfo = console.info;
console.debug = function () {
    _origDebug.apply(console, arguments);
    flog("DBG", "frontend", Array.prototype.join.call(arguments, " "));
};
console.info = function () {
    _origInfo.apply(console, arguments);
    flog("INF", "frontend", Array.prototype.join.call(arguments, " "));
};
console.log = function () {
    _origLog.apply(console, arguments);
    flog("INF", "frontend", Array.prototype.join.call(arguments, " "));
};
console.warn = function () {
    _origWarn.apply(console, arguments);
    flog("WRN", "frontend", Array.prototype.join.call(arguments, " "));
};
console.error = function () {
    _origError.apply(console, arguments);
    flog("ERR", "frontend", Array.prototype.join.call(arguments, " "));
};

window.addEventListener("error", function (e) {
    flog("ERR", "frontend.window", e.message + " at " + e.filename + ":" + e.lineno);
});

window.addEventListener("unhandledrejection", function (e) {
    flog("ERR", "frontend.promise", String(e.reason));
});

// --- Theme ---
// applyThemePreference(pref): light | dark | system. "system" следует за
// prefers-color-scheme и переключается вслед за ОС. Хранится в localStorage
// vox-theme (и в БД ui_theme — для нативного фона окна в main.go).
var _themeMq = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
export function applyThemePreference(pref) {
    var eff = pref === "system" ? ((_themeMq && _themeMq.matches) ? "dark" : "light") : (pref || "light");
    document.documentElement.setAttribute("data-theme", eff);
    document.documentElement.dataset.themePref = pref || "light";
}
if (_themeMq && _themeMq.addEventListener) {
    _themeMq.addEventListener("change", function () {
        if ((localStorage.getItem("vox-theme") || "") === "system") applyThemePreference("system");
    });
}
(function () {
    var saved = localStorage.getItem("vox-theme");
    if (saved) applyThemePreference(saved);
    // One-time миграция: если localStorage уже содержит тему но в БД её нет,
    // синхронизируем туда (бэк прочитает при следующем старте для нативного
    // фона окна). После первого save settings.js делает это сам.
    if (saved) {
        // Лениво — Wails Call ещё не готов на момент module init.
        setTimeout(function () {
            try {
                Call.ByName("vox_app/internal/services.IdentityService.SaveSetting", "ui_theme", saved).catch(function () {});
            } catch (e) {}
        }, 500);
    }
})();

// --- Message status SVGs ---
export var CLOCK_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>';
export var SINGLE_CHECK_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 13 8 17 16 9"/></svg>';
export var DOUBLE_CHECK_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="1 13 5 17 11 9"/><polyline points="7 13 11 17 17 9"/></svg>';

// --- State ---
export var state = {
    selectedPeerID: null,
    selectedPeerInfo: null,
    selectedGroupID: null,
    myID: "",
    myShortID: "",
    myNickname: "",
    peers: [],
    groups: [],
    lastMessages: {},
    call: null,
    // Когда активен: { id, type:"dm"|"group", peerId, groupId, participants:[], creatorId, startedAt }
};

// --- DOM ---
export var dom = {
    setupScreen: document.getElementById("setup-screen"),
    recoveryScreen: document.getElementById("recovery-screen"),
    appDiv: document.getElementById("app"),
    contactsList: document.getElementById("contacts-list"),
    chatHeader: document.getElementById("chat-header"),
    chatPeerName: document.getElementById("chat-peer-name"),
    chatPeerStatus: document.getElementById("chat-peer-status"),
    messagesDiv: document.getElementById("messages"),
    messageInput: document.getElementById("message-input"),
    inputArea: document.getElementById("input-area"),
    usernameEl: document.getElementById("username"),
    statusEl: document.getElementById("status"),
    userAvatar: document.getElementById("user-avatar"),
    settingsMyTag: document.getElementById("settings-my-tag"),
    settingsScreen: document.getElementById("settings-screen"),
    settingsClose: document.getElementById("settings-close"),
    signalServersBody: document.getElementById("signal-servers-body"),
    nicknameInput: document.getElementById("nickname-input"),
    infoPanel: document.getElementById("info-panel"),
    infoToggleBtn: document.getElementById("info-toggle-btn"),
    infoCloseBtn: document.getElementById("info-close-btn"),
    shareContactBtn: document.getElementById("share-contact-btn"),
    composeBtn: document.getElementById("compose-btn"),
    // Calls
    callBtn: document.getElementById("call-btn"),
    userBarNormal: document.getElementById("user-bar-normal"),
    userBarCall: document.getElementById("user-bar-call"),
    userBarVoice: document.getElementById("user-bar-voice"),
};

// --- Screen switching (централизованно) ---
// showScreen(name) скрывает все главные экраны и показывает один.
// Раньше каждый модуль сам выставлял display для чужих screen'ов и мог забыть
// про недавно добавленный экран — это приводило к оверлапу (contactsScreen
// оставался поверх settingsScreen и т.д.).
//
// Имена:
//   "setup"    — setup-screen (первый запуск)
//   "recovery" — recovery-screen (восстановление ключа)
//   "main"     — app (основной чат + сайдбар + info panel)
//   "settings" — settings-screen (единственный оверлей; Контакты — режим
//                левой панели, страницы Звонки нет — история в профиле друга)
export function showScreen(name) {
    var map = {
        setup: dom.setupScreen,
        recovery: dom.recoveryScreen,
        main: dom.appDiv,
        settings: dom.settingsScreen,
    };
    for (var key in map) {
        var el = map[key];
        if (!el) continue;
        if (key === name) {
            el.style.display = ""; // вернуть к CSS-default
        } else {
            el.style.display = "none";
        }
    }
}

// --- Helpers ---
export function colorFromId(hexStr) {
    var h = 0;
    for (var i = 0; i < hexStr.length && i < 8; i++) {
        h = hexStr.charCodeAt(i) + ((h << 5) - h);
    }
    var hue = ((h % 360) + 360) % 360;
    return "hsl(" + hue + ", 55%, " + avatarLightness(hue) + "%)";
}

// Яркость аватара по оттенку (шаг 30°, между точками — линейно): при общих 55%
// белые инициалы на жёлтых и зелёных не читались (контраст ~2), так — не ниже 3.
// Копии функции — в notify.html и incoming-call.html.
var AVATAR_L = [55, 50, 38, 40, 43, 42, 40, 52, 55, 55, 55, 55];
function avatarLightness(hue) {
    var i = Math.floor(hue / 30), f = (hue % 30) / 30;
    return Math.round(AVATAR_L[i] + (AVATAR_L[(i + 1) % 12] - AVATAR_L[i]) * f);
}

export function formatTime(timestamp) {
    var d = new Date(timestamp);
    var now = new Date();
    var diffMs = now - d;
    var diffDays = Math.floor(diffMs / 86400000);
    var hh = d.getHours().toString().padStart(2, "0");
    var mm = d.getMinutes().toString().padStart(2, "0");
    var time = hh + ":" + mm;

    // Сравниваем календарные дни, а не getDate(): на 1-е число разница
    // «1 - 31» давала отрицательное значение и «вчера» превращалось в день недели.
    if (d.toDateString() === now.toDateString()) return time;
    var yday = new Date(now);
    yday.setDate(yday.getDate() - 1);
    if (d.toDateString() === yday.toDateString()) return t("time.yesterday");
    if (diffDays < 7) {
        var days = [t("time.sun"), t("time.mon"), t("time.tue"), t("time.wed"), t("time.thu"), t("time.fri"), t("time.sat")];
        return days[d.getDay()];
    }
    // «16.09» / «9/16» — порядок дня и месяца по языку интерфейса
    return new Intl.DateTimeFormat(getLocale(), { day: "numeric", month: "numeric" }).format(d);
}

export function formatDateLabel(timestamp) {
    var d = new Date(timestamp);
    var now = new Date();
    if (d.toDateString() === now.toDateString()) return t("time.today");
    var yesterday = new Date(now);
    yesterday.setDate(yesterday.getDate() - 1);
    if (d.toDateString() === yesterday.toDateString()) return t("time.yesterday");
    // «16 сентября 2026 г.» / «September 16, 2026» — по языку интерфейса
    return new Intl.DateTimeFormat(getLocale(), { day: "numeric", month: "long", year: "numeric" }).format(d);
}

export function formatMmSs(ms) {
    var totalSec = Math.floor(ms / 1000);
    var m = Math.floor(totalSec / 60);
    var s = totalSec % 60;
    return (m < 10 ? "0" : "") + m + ":" + (s < 10 ? "0" : "") + s;
}

export function formatBytes(bytes) {
    if (bytes < 1024) return bytes + " B";
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + " KB";
    if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + " MB";
    return (bytes / (1024 * 1024 * 1024)).toFixed(2) + " GB";
}

var maxInputLines = 10;
export function autoGrowTextarea() {
    dom.messageInput.style.height = "auto";
    var lineHeight = parseFloat(getComputedStyle(dom.messageInput).lineHeight) || 20;
    var maxH = lineHeight * maxInputLines + 20;
    // scrollHeight не включает рамку, а высота (border-box) включает: без поправки
    // поле после первого ввода становилось на 2px ниже, чем пустое
    var border = dom.messageInput.offsetHeight - dom.messageInput.clientHeight;
    var h = Math.min(dom.messageInput.scrollHeight, maxH) + border;
    dom.messageInput.style.height = h + "px";
    dom.messageInput.style.overflowY = dom.messageInput.scrollHeight > maxH ? "auto" : "hidden";
}

// cssEscape — экранирование значения для attribute-selector'а. id сообщений и
// sha приходят от удалённых пиров: кавычка/скобка в них ломала querySelector
// (throw) и прерывала рендер всей истории чата.
export function cssEscape(s) {
    s = String(s === null || s === undefined ? "" : s);
    if (window.CSS && typeof CSS.escape === "function") return CSS.escape(s);
    return s.replace(/["\\\]]/g, "\\$&");
}

// svcQuiet — svc для fire-and-forget вызовов (toggles/slider'ы настроек):
// ошибка логируется и показывается toast'ом вместо unhandled rejection
// (который глобальный обработчик выводил как «Ошибка: ...» без контекста).
export function svcQuiet(name) {
    var args = Array.prototype.slice.call(arguments);
    return svc.apply(null, args).catch(function (err) {
        var msg = String(err && err.message || err);
        flog("ERR", "svc", name + ": " + msg);
        showError(msg, { title: name });
    });
}
