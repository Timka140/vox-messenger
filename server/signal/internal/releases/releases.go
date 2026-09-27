// Package releases отвечает за хранение и выдачу клиентских релизов
// (бинарей + манифеста + assets) через signal-server.
//
// Manifest format v2: корневой JSON содержит список релизов, каждый релиз
// для каждой платформы содержит `files[]` — список всех файлов релиза
// (основной бинарь + модели + runtime libs + другие assets). Подпись Ed25519
// считается над raw bytes manifest.json и хранится в manifest.json.sig рядом.
// Поэтому отдельных .sig файлов для каждого бинаря больше нет — целостность
// всех файлов гарантируется единой подписью манифеста + sha256 каждого файла
// внутри него.
//
// Manifest перезагружается с диска при изменении mtime.
package releases

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"messenger_signal/internal/version"
	"messenger_signal/pkg/protocol"
)

// ManifestFormatVersion — текущая версия формата манифеста. Клиенты
// проверяют при парсинге: если version > поддерживаемой, отказываются
// использовать для верификации (требуется обновление клиента).
const ManifestFormatVersion = 2

// Manifest — root структура manifest.json (формат v2).
type Manifest struct {
	Version  int            `json:"version"`  // = ManifestFormatVersion
	Latest   string         `json:"latest"`   // версия последнего релиза
	Releases []ReleaseEntry `json:"releases"` // все известные релизы (desc по версии)
}

// ReleaseEntry — один релиз с платформенными bundles.
type ReleaseEntry struct {
	Version    string    `json:"version"`
	ReleasedAt time.Time `json:"released_at"`
	Changelog  string    `json:"changelog"`
	Force      bool      `json:"force"`
	// Revoked — версия отозвана: клиенты на ней переходят на latest, даже если
	// latest старше (откат в админке). Отозвать можно только не-latest.
	Revoked   bool                      `json:"revoked,omitempty"`
	Platforms map[string]PlatformBundle `json:"platforms"` // ключ: "windows-amd64" и т.п.
}

// PlatformBundle — набор файлов релиза для одной платформы.
// Все пути в Files относительны от корня bundle (то есть от <platform>/<version>/
// на сервере и от <exeDir> на клиенте).
type PlatformBundle struct {
	// Entry — относительный путь к основному исполняемому файлу внутри Files.
	// Клиент использует для определения какой файл переименовать в .old-* при apply.
	// Пример: "nexus.exe" для windows, "nexus" для linux.
	Entry string `json:"entry"`

	// Files — полный список файлов этого релиза для этой платформы.
	// Все файлы с path, относительным от <platform>/<version>/ на сервере.
	// Включает основной бинарь (entry) + все assets (модели, runtime libs).
	Files []ManifestFile `json:"files"`
}

// ManifestFile — один файл с целостностью.
type ManifestFile struct {
	// Path — POSIX-style относительный путь (forward slashes).
	// Примеры: "nexus.exe", "models/DeepFilterNet3.tar.gz", "runtime/onnxruntime.dll".
	Path string `json:"path"`

	// Size — размер в байтах, для UI прогресса и sanity check.
	Size int64 `json:"size"`

	// SHA256 — hex-encoded sha256 файла.
	SHA256 string `json:"sha256"`
}

// Store загружает manifest.json с диска и отдаёт артефакты.
// Также держит в памяти raw bytes manifest.json + manifest.json.sig для
// одноразовой отдачи клиенту через HTTP.
type Store struct {
	dir string

	mu       sync.RWMutex
	m        *Manifest
	rawBytes []byte // raw manifest.json — для подписи и для верификации клиентом
	sig      []byte // raw manifest.json.sig (base64 text) — тоже передаётся клиенту
	mtime    time.Time
}

// NewStore создаёт store указывающий на директорию с manifest.json и manifest.json.sig.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Get возвращает актуальный manifest. Перезагружает с диска если файл изменился.
func (s *Store) Get() (*Manifest, error) {
	if _, _, err := s.loadIfChanged(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m, nil
}

// GetBytes возвращает raw JSON bytes manifest.json и base64-encoded подпись
// manifest.json.sig. Используется HTTP handler'ом для отдачи клиенту вместе
// с X-Signature заголовком — клиент верифицирует подпись над raw bytes до
// парсинга, избегая race между parse и verify.
func (s *Store) GetBytes() ([]byte, []byte, error) {
	if _, _, err := s.loadIfChanged(); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Копии чтобы вызывающий не мог мутировать кеш.
	bodyCopy := make([]byte, len(s.rawBytes))
	copy(bodyCopy, s.rawBytes)
	sigCopy := make([]byte, len(s.sig))
	copy(sigCopy, s.sig)
	return bodyCopy, sigCopy, nil
}

// loadIfChanged подгружает manifest.json + manifest.json.sig если их mtime
// изменился с прошлого чтения. Возвращает (raw, sig, error).
func (s *Store) loadIfChanged() ([]byte, []byte, error) {
	if s.dir == "" {
		return nil, nil, fmt.Errorf("releases: dir not configured")
	}
	manifestPath := filepath.Join(s.dir, "manifest.json")
	st, err := os.Stat(manifestPath)
	if err != nil {
		return nil, nil, err
	}

	s.mu.RLock()
	cached := s.m
	cachedMtime := s.mtime
	cachedRaw := s.rawBytes
	cachedSig := s.sig
	s.mu.RUnlock()

	if cached != nil && st.ModTime().Equal(cachedMtime) {
		return cachedRaw, cachedSig, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m != nil && st.ModTime().Equal(s.mtime) {
		return s.rawBytes, s.sig, nil
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("releases: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("releases: parse manifest: %w", err)
	}
	if m.Version != 0 && m.Version != ManifestFormatVersion {
		return nil, nil, fmt.Errorf("releases: unsupported manifest format version %d (want %d)",
			m.Version, ManifestFormatVersion)
	}

	// Подпись читаем best-effort — если её нет, клиент откажется верифицировать.
	// Это нормально для dev-окружения без ключей.
	var sigBytes []byte
	sigPath := filepath.Join(s.dir, "manifest.json.sig")
	if b, err := os.ReadFile(sigPath); err == nil {
		sigBytes = b
	}

	s.m = &m
	s.rawBytes = data
	s.sig = sigBytes
	s.mtime = st.ModTime()
	return data, sigBytes, nil
}

// LatestReleaseInfo возвращает информацию о последнем релизе для auth_ok payload.
func (s *Store) LatestReleaseInfo() *protocol.LatestReleaseInfo {
	m, err := s.Get()
	if err != nil || m == nil || m.Latest == "" {
		return nil
	}
	for _, r := range m.Releases {
		if r.Version == m.Latest {
			return &protocol.LatestReleaseInfo{
				Version:   r.Version,
				Force:     r.Force,
				Changelog: r.Changelog,
				Revoked:   revokedVersions(m),
			}
		}
	}
	return nil
}

// revokedVersions — отозванные версии манифеста (для auth_ok).
func revokedVersions(m *Manifest) []string {
	var out []string
	for _, r := range m.Releases {
		if r.Revoked {
			out = append(out, r.Version)
		}
	}
	return out
}

// safeSegment — разрешены буквы/цифры/точки/дефисы/подчёркивания.
// Всё что может представлять traversal (..), разделители, пробелы — запрещено.
var safeSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ArtifactPath возвращает абсолютный путь к файлу артефакта с traversal guard.
// filename — POSIX-style путь относительно <platform>/<version>/, может
// содержать подкаталоги (`/`), но каждый сегмент проверяется safeSegment.
// Итоговый путь должен находиться внутри releases dir.
func (s *Store) ArtifactPath(platform, ver, filename string) (string, error) {
	if s.dir == "" {
		return "", fmt.Errorf("releases: dir not configured")
	}
	// platform и ver — одиночные segments.
	for _, seg := range []string{platform, ver} {
		if seg == "" || !safeSegment.MatchString(seg) {
			return "", fmt.Errorf("releases: invalid path segment %q", seg)
		}
	}
	// filename — POSIX path, разбиваем по "/" и валидируем каждый сегмент.
	// Запрещаем абсолютные пути и пустые компоненты.
	if filename == "" || strings.HasPrefix(filename, "/") || strings.Contains(filename, "\\") {
		return "", fmt.Errorf("releases: invalid filename %q", filename)
	}
	parts := strings.Split(filename, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("releases: empty filename")
	}
	for _, p := range parts {
		if p == "" || !safeSegment.MatchString(p) {
			return "", fmt.Errorf("releases: invalid filename segment %q", p)
		}
		if p == "." || p == ".." {
			return "", fmt.Errorf("releases: traversal segment %q", p)
		}
	}
	// Проверка — ver можно парсить, но это soft — некоторые файлы могут иметь
	// нестандартные имена версий (теги, rc-сборки).
	if _, err := version.Parse(ver); err != nil {
		_ = err
	}

	// Собираем путь платформозависимо на диске.
	rel := filepath.Join(append([]string{platform, ver}, parts...)...)
	abs := filepath.Join(s.dir, rel)
	absClean := filepath.Clean(abs)

	// Проверяем что итоговый путь внутри dir.
	dirAbs, err := filepath.Abs(s.dir)
	if err != nil {
		return "", fmt.Errorf("releases: abs dir: %w", err)
	}
	fileAbs, err := filepath.Abs(absClean)
	if err != nil {
		return "", fmt.Errorf("releases: abs file: %w", err)
	}
	if !strings.HasPrefix(fileAbs, dirAbs+string(filepath.Separator)) && fileAbs != dirAbs {
		return "", fmt.Errorf("releases: path escapes releases dir")
	}

	if _, err := os.Stat(absClean); err != nil {
		return "", fmt.Errorf("releases: artifact not found: %w", err)
	}
	return absClean, nil
}

// PosixJoin соединяет сегменты через forward-slash — для формирования path
// в манифесте (который всегда POSIX-style независимо от OS).
func PosixJoin(parts ...string) string {
	return path.Join(parts...)
}
