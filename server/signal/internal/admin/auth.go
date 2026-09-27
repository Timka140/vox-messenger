package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	maxSkew      = 60 * time.Second
	replayWindow = 120 * time.Second
	maxBody      = 4 << 20
	// maxReplay — потолок кеша подписей; записать в него может только
	// владелец токена, так что это защита от собственной ошибки, не от атаки.
	maxReplay = 100_000
)

// replayCache — подписи, уже принятые за последние replayWindow.
type replayCache struct {
	mu        sync.Mutex
	seen      map[string]time.Time // подпись → когда истекает
	lastSweep time.Time
}

// add — true, если подписи ещё не было.
func (c *replayCache) add(sig string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastSweep) > time.Second || len(c.seen) >= maxReplay {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
		c.lastSweep = now
	}
	if exp, ok := c.seen[sig]; ok && now.Before(exp) {
		return false
	}
	if len(c.seen) >= maxReplay {
		return false
	}
	c.seen[sig] = now.Add(replayWindow)
	return true
}

// Canonical — строка, над которой считается HMAC (контракт §1).
func Canonical(method, requestURI, ts string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + requestURI + "\n" + ts + "\n" + hex.EncodeToString(sum[:])
}

// Sign — hex(HMAC-SHA256(token, canonical)). Нужна тестам и утилитам.
func Sign(token, method, requestURI, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(Canonical(method, requestURI, ts, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// auth — проверка X-NW-Ts / X-NW-Sig; тело читается целиком (≤ 4 МБ) и
// подкладывается обратно для обработчика, ответ обработчика шифруется.
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tsStr := r.Header.Get("X-NW-Ts")
		sigHex := r.Header.Get("X-NW-Sig")
		if tsStr == "" || sigHex == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		tsMs, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		now := a.now()
		d := now.Sub(time.UnixMilli(tsMs))
		if d > maxSkew || d < -maxSkew {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeErr(w, http.StatusRequestEntityTooLarge, "body too large")
				return
			}
			writeErr(w, http.StatusBadRequest, "read body")
			return
		}

		got, err := hex.DecodeString(sigHex)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mac := hmac.New(sha256.New, a.token)
		mac.Write([]byte(Canonical(r.Method, r.URL.RequestURI(), tsStr, body)))
		if !hmac.Equal(got, mac.Sum(nil)) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Повтор проверяем только после верной подписи: чужой мусор кеш не засоряет.
		if !a.replay.add(hex.EncodeToString(got), now) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// После проверки подписи открытым текстом не уходит ничего: запрос
		// без X-NW-Enc: 1 отвергается, остальные ответы шифруются (seal.go).
		if r.Header.Get(encHeader) != "1" {
			writeErr(w, http.StatusBadRequest, "encryption required")
			return
		}
		fs, err := newFrameSealer(w, a.key, hex.EncodeToString(got), a.nonce)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		sw := &sealWriter{rw: w, fs: fs}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(sw, r)
		if err := sw.finish(); err != nil {
			a.log.Debug("ответ прерван", "error", err.Error())
		}
	})
}
