// Package admin — admin API узла /admin/v1/* для шлюза northwindcdn.com/admin.
// Контракт — design/northwind-site/ADMIN-CONTRACT.md §1: запросы подписаны
// HMAC-SHA256 общим ADMIN_TOKEN, ответы JSON в потоке кадров AES-256-GCM (seal.go).
package admin

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"messenger_signal/internal/logstore"
	"messenger_signal/internal/releases"
)

// MinTokenLen — ADMIN_TOKEN короче — API выключен.
const MinTokenLen = 32

// processStart — время старта процесса (инициализация пакета).
var processStart = time.Now().UTC()

// ClusterPeer — сосед по кластеру для /status.
type ClusterPeer struct {
	Addr      string `json:"addr"`
	ID        string `json:"id"`
	Connected bool   `json:"connected"`
}

// ClusterInfo — состояние кластера для /status.
type ClusterInfo struct {
	Enabled bool          `json:"enabled"`
	Self    string        `json:"self,omitempty"`
	Peers   []ClusterPeer `json:"peers,omitempty"`
}

// Options — зависимости API. nil-функции и nil-хранилища — поле /status
// опускается, соответствующие ручки отвечают 503.
type Options struct {
	Token         string
	ReleasePubKey ed25519.PublicKey // nil — PUT /releases отвечает 503
	NodeID        string
	Clients       func() int
	// ClientVersions — онлайн-клиенты по версии приложения из auth
	// ("unknown" — не прислали).
	ClientVersions func() map[string]int
	// CollectLogs — разослать log_request (nil — все онлайн-клиенты узла),
	// вернуть число клиентов, кому ушёл запрос. nil — POST /logs/collect 503.
	CollectLogs func(sources []string) int
	// OnlineSources — имена источников логов, чьи клиенты сейчас подключены
	// к узлу. nil — поле online в /logs/sources опускается.
	OnlineSources func() map[string]bool
	Cluster       func() ClusterInfo
	Groups        func() (int, bool)
	Releases      *releases.Store
	Logs          *logstore.Store
	Log           *slog.Logger
	Now           func() time.Time // для тестов; nil — time.Now
	Started       time.Time        // для тестов; ноль — момент New
}

// API — обработчики /admin/v1/*.
type API struct {
	opt    Options
	token  []byte
	key    []byte    // ключ шифрования ответов (seal.go)
	nonce  io.Reader // источник nonce; nil — crypto/rand (тесты подменяют)
	replay *replayCache
	log    *slog.Logger
	// started — запуск процесса: кеш повторов живёт в памяти, поэтому подписи
	// старше него отвергаются — после рестарта их не с чем сверить.
	started time.Time

	statsMu sync.Mutex
	stats   map[string]fileStats // путь файла → счётчики (по mtime+size)
}

// New создаёт API. Token должен быть не короче MinTokenLen — проверяет вызывающий.
func New(opt Options) *API {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	key, err := responseKey([]byte(opt.Token))
	if err != nil {
		// HKDF-SHA256 на 32 байта не ошибается; на всякий случай — не молча.
		panic("admin: response key: " + err.Error())
	}
	a := &API{
		opt:    opt,
		token:  []byte(opt.Token),
		key:    key,
		replay: &replayCache{seen: make(map[string]time.Time)},
		log:    log.With(slog.String("component", "admin")),
		stats:  make(map[string]fileStats),
	}
	a.started = opt.Started
	if a.started.IsZero() {
		a.started = a.now()
	}
	return a
}

func (a *API) now() time.Time {
	if a.opt.Now != nil {
		return a.opt.Now()
	}
	return time.Now()
}

// Handler — все маршруты под подписью.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/status", a.handleStatus)
	mux.HandleFunc("GET /admin/v1/releases", a.handleReleasesGet)
	mux.HandleFunc("PUT /admin/v1/releases", a.handleReleasesPut)
	mux.HandleFunc("GET /admin/v1/logs/sources", a.handleLogSources)
	mux.HandleFunc("POST /admin/v1/logs/collect", a.handleLogsCollect)
	mux.HandleFunc("GET /admin/v1/logs/{source}", a.handleLogs)
	mux.HandleFunc("GET /admin/v1/logs/{source}/download", a.handleLogsDownload)
	mux.HandleFunc("/admin/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	return a.auth(mux)
}

// Disabled — ответ, когда ADMIN_TOKEN не задан.
func Disabled() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// buildVersion — версия сигналки из build info: версия модуля либо vcs-ревизия.
// Ничего нет — пусто, поле опускается.
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

type statusReleases struct {
	Latest string `json:"latest"`
	Count  int    `json:"count"`
}

type statusGo struct {
	Goroutines int     `json:"goroutines"`
	MemAllocMB float64 `json:"mem_alloc_mb"`
}

type statusResp struct {
	NodeID         string          `json:"node_id,omitempty"`
	Version        string          `json:"version,omitempty"`
	StartedAt      string          `json:"started_at"`
	ClientsOnline  *int            `json:"clients_online,omitempty"`
	ClientVersions *map[string]int `json:"client_versions,omitempty"`
	Cluster        *ClusterInfo    `json:"cluster,omitempty"`
	Groups         *int            `json:"groups,omitempty"`
	Releases       *statusReleases `json:"releases,omitempty"`
	LogSources     *int            `json:"log_sources,omitempty"`
	Go             statusGo        `json:"go"`
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := statusResp{
		NodeID:    a.opt.NodeID,
		Version:   buildVersion(),
		StartedAt: processStart.Format(time.RFC3339),
	}
	if a.opt.Clients != nil {
		n := a.opt.Clients()
		resp.ClientsOnline = &n
	}
	if a.opt.ClientVersions != nil {
		v := a.opt.ClientVersions()
		if v == nil {
			v = map[string]int{}
		}
		resp.ClientVersions = &v
	}
	if a.opt.Cluster != nil {
		c := a.opt.Cluster()
		resp.Cluster = &c
	}
	if a.opt.Groups != nil {
		if n, ok := a.opt.Groups(); ok {
			resp.Groups = &n
		}
	}
	if a.opt.Releases != nil {
		if m, err := a.opt.Releases.Get(); err == nil && m != nil {
			resp.Releases = &statusReleases{Latest: m.Latest, Count: len(m.Releases)}
		}
	}
	if a.opt.Logs != nil {
		n := len(a.opt.Logs.ListSources())
		resp.LogSources = &n
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	resp.Go = statusGo{
		Goroutines: runtime.NumGoroutine(),
		MemAllocMB: float64(int64(float64(ms.Alloc)/(1<<20)*10)) / 10,
	}
	writeJSON(w, http.StatusOK, resp)
}

type releasesBody struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
}

func (a *API) handleReleasesGet(w http.ResponseWriter, r *http.Request) {
	if a.opt.Releases == nil {
		writeErr(w, http.StatusServiceUnavailable, "releases not configured")
		return
	}
	body, sig, err := a.opt.Releases.GetBytes()
	if err != nil {
		writeErr(w, http.StatusNotFound, "manifest not available")
		return
	}
	writeJSON(w, http.StatusOK, releasesBody{
		Manifest:  string(body),
		Signature: strings.TrimSpace(string(sig)),
	})
}

func (a *API) handleReleasesPut(w http.ResponseWriter, r *http.Request) {
	if a.opt.Releases == nil {
		writeErr(w, http.StatusServiceUnavailable, "releases not configured")
		return
	}
	if len(a.opt.ReleasePubKey) != ed25519.PublicKeySize {
		writeErr(w, http.StatusServiceUnavailable, "RELEASE_PUBKEY not configured")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	var req releasesBody
	if err := json.Unmarshal(raw, &req); err != nil || req.Manifest == "" {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	sigB64 := strings.TrimSpace(req.Signature)
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	manifest := []byte(req.Manifest)
	sum := sha256.Sum256(manifest)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(a.opt.ReleasePubKey, sum[:], sig) {
		writeErr(w, http.StatusBadRequest, "signature")
		return
	}
	m, err := a.opt.Releases.Replace(manifest, sigB64)
	if err != nil {
		if errors.Is(err, releases.ErrInvalid) {
			writeErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), releases.ErrInvalid.Error()+": "))
			return
		}
		a.log.Error("запись манифеста", slog.String("error", err.Error()))
		writeErr(w, http.StatusInternalServerError, "write failed")
		return
	}
	a.log.Info("манифест релизов заменён через admin API", slog.String("latest", m.Latest),
		slog.Int("releases", len(m.Releases)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latest": m.Latest})
}
