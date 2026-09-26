package services

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vox_app/internal/logsafe"
	"vox_app/internal/storage"
)

// AssetBridge раздаёт большие/локальные файлы в WebView2 без копирования
// в JS heap — ставится Middleware'ом перед основным asset-server'ом.
//
//	GET /vox-attach/<sha256>   — стрим из BlobStore (Range-aware)
//	GET /vox-local/<token>     — стрим локального файла по token'у (для превью
//	                             в picker-модалке; token живёт ttl)
//
// Всё остальное проксируется next.ServeHTTP — основной BundledAssetFileServer.
type AssetBridge struct {
	blobs *storage.BlobStore
	db    *sql.DB
	log   *slog.Logger

	mu     sync.Mutex
	tokens map[string]*localToken
}

type localToken struct {
	path      string
	mime      string
	expiresAt time.Time
}

const (
	defaultLocalTokenTTL = 1 * time.Hour
	tokenLen             = 16 // 32 hex chars
)

// NewAssetBridge — конструктор. db нужна для resolve sha → size/mime.
func NewAssetBridge(blobs *storage.BlobStore, db *sql.DB, log *slog.Logger) *AssetBridge {
	b := &AssetBridge{
		blobs:  blobs,
		db:     db,
		log:    log,
		tokens: make(map[string]*localToken),
	}
	go b.gcLoop()
	return b
}

// Middleware возвращает http.Handler-обёртку. Если URL начинается с
// "/vox-attach/" или "/vox-local/" — обрабатываем сами; иначе пропускаем дальше.
func (b *AssetBridge) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/vox-attach/"):
			b.serveAttach(w, r, strings.TrimPrefix(r.URL.Path, "/vox-attach/"))
		case strings.HasPrefix(r.URL.Path, "/vox-local/"):
			b.serveLocal(w, r, strings.TrimPrefix(r.URL.Path, "/vox-local/"))
		case strings.HasPrefix(r.URL.Path, "/vox-avatar/"):
			b.serveAvatar(w, strings.TrimPrefix(r.URL.Path, "/vox-avatar/"))
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// --- /vox-avatar/<peerId> или /vox-avatar/g:<groupId> ---

// serveAvatar отдаёт фото профиля (peer_profiles) или группы (groups) из БД.
// Фото ≤ 40 КБ — читаем целиком. Фронт добавляет ?v=<hash>, поэтому
// кешировать на стороне WebView не нужно: новый хеш = новый URL.
func (b *AssetBridge) serveAvatar(w http.ResponseWriter, id string) {
	if id == "" || len(id) > 128 {
		http.NotFound(w, nil)
		return
	}
	var data []byte
	var err error
	if gid, ok := strings.CutPrefix(id, "g:"); ok {
		err = b.db.QueryRow("SELECT avatar FROM groups WHERE group_id = ?", gid).Scan(&data)
	} else {
		err = b.db.QueryRow("SELECT avatar FROM peer_profiles WHERE peer_id = ?", id).Scan(&data)
	}
	if err != nil || len(data) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// --- /vox-attach/<sha256> ---

var sha256RE = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func (b *AssetBridge) serveAttach(w http.ResponseWriter, r *http.Request, sha string) {
	if !sha256RE.MatchString(sha) {
		http.Error(w, "bad sha", http.StatusBadRequest)
		return
	}
	sha = strings.ToLower(sha)

	// Single-file модель: если есть row в files и local_path указывает на
	// существующий файл — отдаём через ServeFile (Range/Cache из коробки).
	// Если local_path пуст или файл удалён — 404 (загрузка ещё не завершена
	// или юзер удалил файл руками).
	{
		var mimeType string
		var localPath string
		err := b.db.QueryRow(
			`SELECT mime, local_path FROM files WHERE sha = ? LIMIT 1`, sha,
		).Scan(&mimeType, &localPath)
		if err == nil {
			if localPath != "" {
				if f, ferr := os.Open(localPath); ferr == nil {
					defer f.Close()
					if mimeType != "" {
						w.Header().Set("Content-Type", mimeType)
					}
					info, _ := f.Stat()
					http.ServeContent(w, r, filepath.Base(localPath), info.ModTime(), f)
					return
				}
				// Файл удалён извне (юзер через Проводник стёр). Сбрасываем
				// local_path → bubble увидит !available → Download-кнопку.
				b.log.Info("vox-attach: local file missing, reset path",
					slog.String("sha", safeShort8(sha)))
				_, _ = b.db.Exec(`UPDATE files SET local_path = '' WHERE sha = ?`, sha)
			}
			// local_path пуст или файл утерян — 404, фронт показывает Download.
			http.Error(w, "file not yet downloaded", http.StatusNotFound)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			b.log.Warn("vox-attach: db lookup files",
				slog.String("sha", safeShort8(sha)), logsafe.Err("err", err))
		}
	}

	// Fallback на legacy BlobStore.
	var size int64
	var mimeType string
	err := b.db.QueryRow(
		`SELECT size, mime FROM attachments WHERE sha256 = ? LIMIT 1`, sha,
	).Scan(&size, &mimeType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		b.log.Warn("vox-attach: db lookup", slog.String("sha", safeShort8(sha)), logsafe.Err("err", err))
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, max-age=3600")

	start, end, isRange, ok := parseRange(r.Header.Get("Range"), size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if !isRange {
		start, end = 0, size-1
	}
	length := end - start + 1

	rdr, err := b.blobs.OpenReaderAt(sha, start)
	if err != nil {
		// blob отсутствует физически — считаем 404 (не 500). Это нормальное
		// состояние когда blob был удалён (например старым багом GC, утерянная
		// retention), а row в attachments осталась. Фронт получает placeholder
		// и не пугается красным в DevTools.
		if errors.Is(err, storage.ErrBlobNotFound) {
			b.log.Debug("vox-attach: blob missing (file gone but row remains)",
				slog.String("sha", safeShort8(sha)))
			http.Error(w, "blob missing", http.StatusNotFound)
			return
		}
		b.log.Warn("vox-attach: open blob", slog.String("sha", safeShort8(sha)), logsafe.Err("err", err))
		http.Error(w, "blob open failed", http.StatusInternalServerError)
		return
	}
	defer rdr.Close()

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if isRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	}

	if r.Method == http.MethodHead {
		return
	}

	if _, err := io.CopyN(w, rdr, length); err != nil && err != io.EOF {
		// Клиент мог оборвать загрузку — это нормально для <video>.
		b.log.Debug("vox-attach: copy interrupted",
			slog.String("sha", safeShort8(sha)),
			slog.Int64("start", start),
			slog.Int64("len", length),
			logsafe.Err("err", err))
	}
}


// --- /vox-local/<token> ---

// RegisterLocalToken создаёт новый token для path. Используется PrepareAttachments
// для превью локальных файлов (фото, выбранное в picker'е).
//   - mimeType=="" — определяется по расширению.
//   - ttl<=0 — defaultLocalTokenTTL.
//
// Path должен быть абсолютным; существование файла НЕ проверяется здесь —
// caller гарантирует что путь валидный.
func (b *AssetBridge) RegisterLocalToken(path, mimeType string, ttl time.Duration) (string, error) {
	if path == "" {
		return "", fmt.Errorf("RegisterLocalToken: empty path")
	}
	if ttl <= 0 {
		ttl = defaultLocalTokenTTL
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}

	raw := make([]byte, tokenLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("RegisterLocalToken: rand: %w", err)
	}
	token := hex.EncodeToString(raw)

	b.mu.Lock()
	b.tokens[token] = &localToken{
		path:      path,
		mime:      mimeType,
		expiresAt: time.Now().Add(ttl),
	}
	b.mu.Unlock()
	return token, nil
}

// RevokeLocalToken — явный отзыв (когда модалка закрылась).
func (b *AssetBridge) RevokeLocalToken(token string) {
	b.mu.Lock()
	delete(b.tokens, token)
	b.mu.Unlock()
}

// LookupLocalToken — для тестов и debug.
func (b *AssetBridge) LookupLocalToken(token string) (path, mimeType string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tok, ok := b.tokens[token]
	if !ok {
		return "", "", false
	}
	if time.Now().After(tok.expiresAt) {
		delete(b.tokens, token)
		return "", "", false
	}
	return tok.path, tok.mime, true
}

func (b *AssetBridge) serveLocal(w http.ResponseWriter, r *http.Request, token string) {
	path, mimeType, ok := b.LookupLocalToken(token)
	if !ok {
		http.Error(w, "bad or expired token", http.StatusForbidden)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "open failed", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, max-age=300")
	// http.ServeContent умеет Range, ETag, ModTime.
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// gcLoop периодически чистит протухшие tokens.
func (b *AssetBridge) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		b.mu.Lock()
		for k, v := range b.tokens {
			if now.After(v.expiresAt) {
				delete(b.tokens, k)
			}
		}
		b.mu.Unlock()
	}
}

// --- Range header parsing ---
//
// Поддерживается одна форма: "bytes=START-END" и "bytes=START-".
// "bytes=-N" (last N bytes) и multi-range не нужны для media-плеера WebView2.
//
// ok=false означает синтактически невалидный header (вернуть 416).
// isRange=false при отсутствии или пустом header — отдаём всё.

func parseRange(h string, size int64) (start, end int64, isRange, ok bool) {
	if h == "" {
		return 0, 0, false, true
	}
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if strings.Contains(spec, ",") {
		// Multi-range мы не поддерживаем — вернём весь файл.
		return 0, 0, false, true
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, false
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	if startStr == "" {
		// "bytes=-N" — последние N байт.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	}
	s, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false, false
	}
	if endStr == "" {
		return s, size - 1, true, true
	}
	e, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil || e < s {
		return 0, 0, false, false
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true, true
}
