"use strict";

// voice-state.js — Единый владелец mute/deafen состояния с Observer pattern.
// Все изменения состояния проходят через этот класс, UI обновляется автоматически через подписки.

import { svc, flog } from "./core.js";

var DEFAULT_STATUS = { muted: false, deafened: false };

class VoiceState {
    constructor() {
        this._statuses = {};   // peerId → {muted, deafened}
        this._myId = "";
        this._mutedBeforeDeafen = false;
        this._listeners = {};  // event → [fn]
        this._batchDepth = 0;
        this._batchDirty = {};  // event → data (last)
    }

    // --- Init ---

    init(myId) {
        this._myId = myId;
    }

    // --- Getters ---

    getMyStatus() {
        if (!this._myId) return { muted: false, deafened: false };
        return this._statuses[this._myId] || { muted: false, deafened: false };
    }

    getPeerStatus(peerId) {
        return this._statuses[peerId] || { muted: false, deafened: false };
    }

    // --- Self mutations ---

    toggleMute() {
        if (!this._myId) return;
        var prev = this.getMyStatus();
        var newMuted = !prev.muted;

        // Optimistic UI update
        this._setSelf(newMuted, prev.deafened);

        svc(newMuted ? "MuteVoice" : "UnmuteVoice").catch(function (err) {
            // Rollback
            flog("ERR", "voice-state", "toggleMute failed, rollback: " + err);
            this._setSelf(prev.muted, prev.deafened);
        }.bind(this));

        flog("INF", "voice-state", "toggleMute: " + newMuted);
    }

    toggleDeafen() {
        if (!this._myId) return;
        var prev = this.getMyStatus();
        var next = !prev.deafened;
        var self = this;

        if (next) {
            this._mutedBeforeDeafen = prev.muted;
        }

        // Deafen ждёт подтверждения от бекенда (не optimistic)
        svc(next ? "DeafenVoice" : "UndeafenVoice").then(function () {
            var muted = next ? true : self._mutedBeforeDeafen;
            self.batch(function () {
                self._setSelf(muted, next);
            });
            flog("INF", "voice-state", "toggleDeafen: " + next + " muted: " + muted);
        }).catch(function (err) {
            flog("ERR", "voice-state", "toggleDeafen failed: " + err);
        });
    }

    resetSelf() {
        if (!this._myId) return;
        this._setSelf(false, false);
    }

    // --- Peer mutations ---

    setPeerStatus(peerId, status) {
        this._statuses[peerId] = { muted: !!status.muted, deafened: !!status.deafened };
        if (peerId === this._myId) {
            this._emit("self-changed", this._statuses[peerId]);
        } else {
            this._emit("peer-changed", { peerId: peerId, muted: !!status.muted, deafened: !!status.deafened });
        }
    }

    removePeer(peerId) {
        delete this._statuses[peerId];
        if (peerId === this._myId) {
            this._emit("self-changed", DEFAULT_STATUS);
        } else {
            this._emit("peer-removed", { peerId: peerId });
        }
    }

    // --- Observer ---

    on(event, fn) {
        if (!this._listeners[event]) this._listeners[event] = [];
        this._listeners[event].push(fn);
    }

    off(event, fn) {
        if (!this._listeners[event]) return;
        this._listeners[event] = this._listeners[event].filter(function (f) { return f !== fn; });
    }

    // --- Batch ---

    batch(fn) {
        this._batchDepth++;
        try {
            fn();
        } finally {
            this._batchDepth--;
            if (this._batchDepth === 0) {
                this._flushBatch();
            }
        }
    }

    // --- Internal ---

    _setSelf(muted, deafened) {
        if (!this._myId) return;
        var prev = this._statuses[this._myId];
        // Дедупликация: не шлём если не изменилось
        if (prev && prev.muted === muted && prev.deafened === deafened) return;
        this._statuses[this._myId] = { muted: muted, deafened: deafened };
        this._emit("self-changed", { muted: muted, deafened: deafened });
    }

    _emit(event, data) {
        if (this._batchDepth > 0) {
            this._batchDirty[event] = data;
            return;
        }
        var cbs = this._listeners[event];
        if (!cbs) return;
        for (var i = 0; i < cbs.length; i++) {
            try { cbs[i](data); } catch (e) {
                flog("ERR", "voice-state", "listener error on " + event + ": " + e);
            }
        }
    }

    _flushBatch() {
        var dirty = this._batchDirty;
        this._batchDirty = {};
        for (var event in dirty) {
            var cbs = this._listeners[event];
            if (!cbs) continue;
            for (var i = 0; i < cbs.length; i++) {
                try { cbs[i](dirty[event]); } catch (e) {
                    flog("ERR", "voice-state", "listener error on " + event + ": " + e);
                }
            }
        }
    }
}

export var voiceState = new VoiceState();
