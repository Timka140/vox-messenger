package services

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"vox_app/internal/network"
	"vox_app/internal/version"
)

// --- Manifest format v2 ---
//
// Манифест — список релизов. Каждый релиз для каждой платформы содержит
// bundle из entry (основного бинаря) + files[] (всех файлов включая models/
// и runtime libs). Целостность всего манифеста гарантируется единой Ed25519
// подписью над raw bytes manifest.json — сервер отдаёт её в заголовке
// X-Signature ответа на /api/update/manifest. После верификации подписи
// клиент использует sha256 в файлах как индивидуальные контрольные суммы.

// ManifestFormatVersion — поддерживаемая клиентом версия формата.
const ManifestFormatVersion = 2

// Manifest — корневая структура manifest.json v2.
type Manifest struct {
	Version  int                    `json:"version"`
	Latest   string                 `json:"latest"`
	Releases []ManifestReleaseEntry `json:"releases"`
}

// ManifestReleaseEntry — один релиз.
type ManifestReleaseEntry struct {
	Version    string    `json:"version"`
	ReleasedAt time.Time `json:"released_at"`
	Changelog  string    `json:"changelog"`
	Force      bool      `json:"force"`
	// Revoked — версия отозвана: клиент на ней ставит latest, даже если latest старше.
	Revoked   bool                              `json:"revoked,omitempty"`
	Platforms map[string]ManifestPlatformBundle `json:"platforms"`
}

// ManifestPlatformBundle — bundle файлов для одной платформы.
type ManifestPlatformBundle struct {
	Entry string         `json:"entry"` // POSIX-style путь к исполняемому файлу
	Files []ManifestFile `json:"files"`
}

// ManifestFile — один файл с sha256 и размером.
type ManifestFile struct {
	Path   string `json:"path"` // POSIX-style, относительный от <exeDir>
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// --- Local installed inventory ---

// installedInventoryFile — имя файла где хранится snapshot установленного релиза.
// Рядом с exe. Читается при Apply чтобы знать что удалять (файлы которых
// нет в новом манифесте).
const installedInventoryFile = ".nexus-installed.json"

// installedInventory — snapshot того что сейчас установлено (entry + assets).
// Пишется при успешном ApplyUpdate, читается при следующем апдейте.
type installedInventory struct {
	Version string         `json:"version"`
	Files   []ManifestFile `json:"files"`
}

// --- Public API types ---

// CheckResult — результат проверки обновлений.
type CheckResult struct {
	Available            bool   `json:"available"`
	Current              string `json:"current"`
	Latest               string `json:"latest"`
	Changelog            string `json:"changelog"`
	Force                bool   `json:"force"`
	SupportedForPlatform bool   `json:"supported_for_platform"`
	Message              string `json:"message"`
}

// DownloadResult — результат скачивания + верификации.
type DownloadResult struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	StageDir string `json:"stage_dir"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
	Message  string `json:"message"`
}

// RepairResult — результат self-heal проверки ассетов текущей версии.
// Repaired: сколько файлов было перекачано из-за отсутствия/битого sha256.
// Skipped: сколько уже было ОК (не считая entry exe — он всегда skipped).
type RepairResult struct {
	Repaired int    `json:"repaired"`
	Skipped  int    `json:"skipped"`
	Error    string `json:"error,omitempty"`
}

// --- Errors ---

var (
	ErrNoUpdateService     = errors.New("update service not configured")
	ErrPlatformUnsupported = errors.New("no build for this platform")
	ErrNoManifest          = errors.New("manifest not available")
	ErrVerifyFailed        = errors.New("verification failed")
	ErrNoPendingStage      = errors.New("no verified stage to apply")
	ErrDownloadInProgress  = errors.New("download already in progress")
)

// --- Service ---

// UpdateService — сервис автообновления клиентского приложения.
type UpdateService struct {
	log            *slog.Logger
	currentVersion version.Version
	currentRaw     string
	publicKey      ed25519.PublicKey
	getServerBase  func() string
	emit           func(string, any)
	quit           func() // завершение после применения/отката (SetQuit)

	mu            sync.RWMutex
	latestRelease *network.LatestReleaseInfo
	pendingStage  string                // абс. путь к stage dir с верифицированным bundle
	pendingEntry  *ManifestReleaseEntry // скачанный entry (нужен для Apply)

	// updateMu сериализует параллельные DownloadUpdate (двойной клик / автотриггер).
	// Пишет в .vox-update/<ver>/ — гонка тут даёт "file in use" на Windows.
	updateMu sync.Mutex

	// repairMu сериализует параллельные RepairAssets (на каждый reconnect emit).
	// Пишет в <exeDir> напрямую — другая зона от updateMu, не конфликтуют.
	repairMu sync.Mutex
}

func NewUpdateService(log *slog.Logger, appVersion, pubKeyB64 string, getServerBase func() string) *UpdateService {
	s := &UpdateService{
		log:           log,
		currentRaw:    appVersion,
		getServerBase: getServerBase,
	}
	if v, err := version.Parse(appVersion); err == nil {
		s.currentVersion = v
	} else {
		log.Info("update: dev build, auto-update disabled", slog.String("version", appVersion))
	}
	if pubKeyB64 != "" {
		pk, err := base64.StdEncoding.DecodeString(pubKeyB64)
		if err != nil || len(pk) != ed25519.PublicKeySize {
			log.Warn("update: invalid public key, verification will fail",
				slog.String("error", fmt.Sprint(err)),
				slog.Int("size", len(pk)))
		} else {
			s.publicKey = pk
		}
	}
	return s
}

func (s *UpdateService) SetEmitter(fn func(string, any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit = fn
}

func (s *UpdateService) OnLatestReleaseFromAuthOK(info *network.LatestReleaseInfo) {
	if info == nil {
		return
	}
	s.mu.Lock()
	s.latestRelease = info
	fn := s.emit
	s.mu.Unlock()

	s.log.Info("📦 latest release available",
		slog.String("current", s.currentRaw),
		slog.String("latest", info.Version),
		slog.Bool("force", info.Force))

	// Dev-сборка без UpdatePublicKey физически не может применить апдейт —
	// показывать баннер/force-модалку бессмысленно, UI только блокируется.
	if s.publicKey == nil {
		return
	}

	revoked := info.Version != s.currentRaw && slices.Contains(info.Revoked, s.currentRaw)
	if fn != nil && (s.isNewerVersion(info.Version) || revoked) {
		fn("vox:update-available", map[string]any{
			"version":   info.Version,
			"force":     info.Force || revoked,
			"changelog": info.Changelog,
			"revoked":   revoked,
		})
	}
}

// --- Wails methods ---

func (s *UpdateService) GetCurrentVersion() string {
	return s.currentRaw
}

func (s *UpdateService) GetLatestRelease() *network.LatestReleaseInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestRelease
}

// CheckForUpdates запрашивает manifest с сервера, верифицирует подпись и
// возвращает результат сравнения с текущей версией.
func (s *UpdateService) CheckForUpdates() (*CheckResult, error) {
	// Dev-сборка без UpdatePublicKey физически не может применить апдейт —
	// не дёргаем сеть и не морочим UI модалкой.
	if s.publicKey == nil {
		return &CheckResult{Available: false, Current: s.currentRaw}, nil
	}
	m, err := s.fetchManifest()
	if err != nil {
		return nil, err
	}
	if m.Latest == "" || len(m.Releases) == 0 {
		return &CheckResult{Available: false, Current: s.currentRaw}, nil
	}

	var latestEntry *ManifestReleaseEntry
	for i := range m.Releases {
		if m.Releases[i].Version == m.Latest {
			latestEntry = &m.Releases[i]
			break
		}
	}
	if latestEntry == nil {
		return &CheckResult{Available: false, Current: s.currentRaw}, nil
	}

	// Текущая версия отозвана — ставим latest, даже если он старше (откат).
	revoked := latestEntry.Version != s.currentRaw && s.isRevokedCurrent(m)
	result := &CheckResult{
		Current:   s.currentRaw,
		Latest:    latestEntry.Version,
		Changelog: latestEntry.Changelog,
		Force:     latestEntry.Force || revoked,
		Available: s.isNewerVersion(latestEntry.Version) || revoked,
	}

	platformKey := runtime.GOOS + "-" + runtime.GOARCH
	if _, ok := latestEntry.Platforms[platformKey]; ok {
		result.SupportedForPlatform = true
	} else {
		result.SupportedForPlatform = false
		result.Message = "Нет сборки для " + platformKey
	}
	return result, nil
}

// DownloadUpdate скачивает все файлы указанной версии для текущей платформы,
// верифицирует каждый по sha256 из (уже проверенного) манифеста, складывает
// в staging directory <exeDir>/.vox-update/<ver>/. Применение через ApplyUpdate.
func (s *UpdateService) DownloadUpdate(ver string) (*DownloadResult, error) {
	if s.publicKey == nil {
		return nil, fmt.Errorf("update: public key not configured (build without -X main.UpdatePublicKey)")
	}
	if !s.updateMu.TryLock() {
		return nil, ErrDownloadInProgress
	}
	defer s.updateMu.Unlock()

	m, err := s.fetchManifest()
	if err != nil {
		return nil, err
	}
	var entry *ManifestReleaseEntry
	for i := range m.Releases {
		if m.Releases[i].Version == ver {
			entry = &m.Releases[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("version %s not in manifest", ver)
	}

	platformKey := runtime.GOOS + "-" + runtime.GOARCH
	bundle, ok := entry.Platforms[platformKey]
	if !ok {
		return nil, ErrPlatformUnsupported
	}
	if len(bundle.Files) == 0 {
		return nil, fmt.Errorf("version %s has no files for %s", ver, platformKey)
	}
	if bundle.Entry == "" {
		return nil, fmt.Errorf("version %s has no entry for %s", ver, platformKey)
	}

	// Подготовка staging directory рядом с exe
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve exe path: %w", err)
	}
	exeDir := filepath.Dir(exe)
	stageDir := filepath.Join(exeDir, ".vox-update", ver)
	// Полная очистка stage — вдруг осталась недокачка с прошлой попытки
	_ = os.RemoveAll(stageDir)
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir stage: %w", err)
	}

	serverBase := ""
	if s.getServerBase != nil {
		serverBase = s.getServerBase()
	}
	if serverBase == "" {
		return nil, fmt.Errorf("no active signal server connection")
	}

	// Общий размер для прогресса
	var totalBytes int64
	for _, f := range bundle.Files {
		totalBytes += f.Size
	}

	progress := &progressWriter{
		total:   totalBytes,
		emit:    s.progressEmit,
		lastTS:  time.Now(),
		version: ver,
	}

	s.log.Info("📥 downloading update bundle",
		slog.String("version", ver),
		slog.Int("files", len(bundle.Files)),
		slog.Int64("total_bytes", totalBytes))

	// Стадия 1: для каждого файла если локальная копия уже совпадает по sha256 —
	// копируем её в stage без network IO. Экономит трафик на неизменившихся
	// ассетах (типично — модели, runtime libs, которые меняются редко).
	var bytesSaved int64
	for _, mf := range bundle.Files {
		if s.tryReuseLocal(exeDir, stageDir, mf, progress) {
			bytesSaved += mf.Size
			continue
		}
		if err := s.downloadOne(serverBase, platformKey, ver, mf, stageDir, progress); err != nil {
			_ = os.RemoveAll(stageDir)
			return nil, fmt.Errorf("download %s: %w", mf.Path, err)
		}
	}
	if bytesSaved > 0 {
		s.log.Info("💾 reused local files",
			slog.Int64("bytes_saved", bytesSaved),
			slog.Int64("total_bytes", totalBytes))
	}

	// Записываем snapshot entry в stage для ApplyUpdate
	snapPath := filepath.Join(stageDir, ".entry.json")
	snapBytes, _ := json.MarshalIndent(entry, "", "  ")
	if err := os.WriteFile(snapPath, snapBytes, 0644); err != nil {
		_ = os.RemoveAll(stageDir)
		return nil, fmt.Errorf("write stage entry snapshot: %w", err)
	}

	s.mu.Lock()
	s.pendingStage = stageDir
	entryCopy := *entry
	s.pendingEntry = &entryCopy
	s.mu.Unlock()

	s.log.Info("✅ bundle downloaded and verified",
		slog.String("version", ver),
		slog.String("stage", stageDir),
		slog.Int("files", len(bundle.Files)),
		slog.Int64("bytes", totalBytes))

	return &DownloadResult{
		OK:       true,
		Version:  ver,
		StageDir: stageDir,
		Files:    len(bundle.Files),
		Bytes:    totalBytes,
	}, nil
}

// tryReuseLocal проверяет существует ли локальная копия файла в exeDir и
// совпадает ли её sha256 с ожидаемым. Если да — копирует её в stage. Это
// позволяет пропустить HTTP-скачивание для неизменившихся ассетов (например,
// модели, которые обычно не меняются между релизами). Возвращает true если
// файл был успешно переиспользован.
func (s *UpdateService) tryReuseLocal(exeDir, stageDir string, mf ManifestFile, progress *progressWriter) bool {
	// Валидация пути — те же правила что и в downloadOne (не доверяем манифесту
	// с path-traversal). Если path невалидный — не пытаемся использовать
	// локально, вернёмся к скачиванию где произойдёт явная ошибка.
	if mf.Path == "" || strings.HasPrefix(mf.Path, "/") || strings.Contains(mf.Path, "\\") {
		return false
	}
	cleanRel := path.Clean(mf.Path)
	if cleanRel != mf.Path || strings.HasPrefix(cleanRel, "../") || cleanRel == ".." {
		return false
	}

	localSrc := filepath.Join(exeDir, filepath.FromSlash(mf.Path))
	info, err := os.Stat(localSrc)
	if err != nil || info.IsDir() || info.Size() != mf.Size {
		return false
	}

	// Проверяем sha256 локального файла
	f, err := os.Open(localSrc)
	if err != nil {
		return false
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		f.Close()
		return false
	}
	f.Close()
	if hex.EncodeToString(hasher.Sum(nil)) != mf.SHA256 {
		return false
	}

	// Локальная копия валидна — копируем в stage с сохранением иерархии
	dstPath := filepath.Join(stageDir, filepath.FromSlash(mf.Path))
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return false
	}
	src, err := os.Open(localSrc)
	if err != nil {
		return false
	}
	defer src.Close()
	dst, err := os.Create(dstPath)
	if err != nil {
		return false
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dstPath)
		return false
	}
	if err := dst.Close(); err != nil {
		os.Remove(dstPath)
		return false
	}

	// Продвигаем прогресс-бар чтобы UI показал что файл "обработан"
	if progress != nil {
		_, _ = progress.Write(make([]byte, 0)) // сброс таймера
		progress.written += mf.Size
	}

	s.log.Info("♻️  reused local file",
		slog.String("path", mf.Path),
		slog.Int64("size", mf.Size))
	return true
}

// downloadOne качает один файл из bundle, проверяет sha256 и сохраняет в stageDir
// с сохранением относительной иерархии (создаёт подпапки если нужно).
func (s *UpdateService) downloadOne(
	serverBase, platformKey, ver string,
	mf ManifestFile,
	stageDir string,
	progress *progressWriter,
) error {
	// Валидация path — должен быть POSIX-style относительный, без traversal.
	if mf.Path == "" || strings.HasPrefix(mf.Path, "/") || strings.Contains(mf.Path, "\\") {
		return fmt.Errorf("invalid manifest file path %q", mf.Path)
	}
	cleanRel := path.Clean(mf.Path)
	if cleanRel != mf.Path || strings.HasPrefix(cleanRel, "../") || cleanRel == ".." {
		return fmt.Errorf("invalid manifest file path %q (traversal)", mf.Path)
	}

	localPath := filepath.Join(stageDir, filepath.FromSlash(mf.Path))
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(localPath), err)
	}

	url := fmt.Sprintf("%s/api/update/download/%s/%s/%s",
		serverBase, platformKey, ver, mf.Path)

	s.log.Info("📥 downloading file",
		slog.String("path", mf.Path),
		slog.Int64("size", mf.Size))

	// Чистим .partial от прошлой неудачной попытки (interrupted process,
	// crash, etc.) — без этого os.Create truncates но если предыдущий
	// f.Close() не успел — могут быть file-handle race issues.
	partialPath := localPath + ".partial"
	_ = os.Remove(partialPath)

	// HTTP без общего Timeout (он бы покрыл всё скачивание включая body —
	// для 86 MB vox.exe на медленном канале легко превышается). Защита от
	// зависшего сервера через ResponseHeaderTimeout=30s: если сервер не ответил
	// заголовками за 30s, мы умираем явно. После начала тела — качаем сколько
	// надо, пока байты текут (TCP сам обнаружит обрыв через keepalive).
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}

	f, err := os.Create(partialPath)
	if err != nil {
		return fmt.Errorf("create partial: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(f, hasher, progress)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		f.Close()
		os.Remove(partialPath)
		return fmt.Errorf("copy: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(partialPath)
		return fmt.Errorf("close: %w", err)
	}

	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if gotSum != mf.SHA256 {
		os.Remove(partialPath)
		return fmt.Errorf("%w: sha256 mismatch (expected %s, got %s)",
			ErrVerifyFailed, mf.SHA256, gotSum)
	}

	if err := os.Rename(partialPath, localPath); err != nil {
		os.Remove(partialPath)
		return fmt.Errorf("rename: %w", err)
	}
	s.log.Info("✅ file downloaded", slog.String("path", mf.Path))
	return nil
}

// RepairAssets проверяет целостность ассетов текущей версии и перекачивает
// отсутствующие/битые из manifest. exe не трогает (раз процесс запущен — он
// исправен). Триггерится после первого успешного auth_ok при старте; также
// доступен из UI как Wails-метод.
//
// Использует тот же downloadOne что и DownloadUpdate, но скачивает напрямую
// в exeDir а не в stage — рестарт не нужен. Если файлы целы — no-op.
func (s *UpdateService) RepairAssets() (*RepairResult, error) {
	if s.publicKey == nil {
		return nil, fmt.Errorf("update: public key not configured (build without -X main.UpdatePublicKey)")
	}
	if s.currentVersion.IsZero() {
		return nil, fmt.Errorf("dev build: skip repair (нет manifest entry для 'dev')")
	}
	if !s.repairMu.TryLock() {
		return nil, ErrDownloadInProgress
	}
	defer s.repairMu.Unlock()

	// TopicLatestRelease может выстрелить ДО полного завершения NetworkService.Connect
	// (внутри parsing auth_ok). К этому моменту client.IsConnected() ещё false →
	// getServerBase() возвращает "". fetchManifest сам полагается на getServerBase
	// и падает первым — поэтому ждём serverBase ДО fetchManifest.
	if err := s.waitServerBase(2500 * time.Millisecond); err != nil {
		return nil, err
	}

	m, err := s.fetchManifest()
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}

	// Ищем entry для НАШЕЙ текущей версии (не latest).
	var entry *ManifestReleaseEntry
	for i := range m.Releases {
		if m.Releases[i].Version == s.currentRaw {
			entry = &m.Releases[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("current version %s not in manifest (retention или другая ветка релизов)", s.currentRaw)
	}

	platformKey := runtime.GOOS + "-" + runtime.GOARCH
	bundle, ok := entry.Platforms[platformKey]
	if !ok {
		return nil, ErrPlatformUnsupported
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve exe path: %w", err)
	}
	exeDir := filepath.Dir(exe)

	serverBase := ""
	if s.getServerBase != nil {
		serverBase = s.getServerBase()
	}
	if serverBase == "" {
		return nil, fmt.Errorf("no active signal server connection")
	}

	var repaired, skipped int
	var firstErr error

	for _, mf := range bundle.Files {
		// Entry exe не трогаем — раз процесс работает, оно на месте.
		// (Если оно битое и при этом запустилось — это вне нашей зоны.)
		if mf.Path == bundle.Entry {
			skipped++
			continue
		}

		localPath := filepath.Join(exeDir, filepath.FromSlash(mf.Path))
		if s.fileMatchesSha(localPath, mf.SHA256, mf.Size) {
			skipped++
			continue
		}

		s.log.Info("🔧 repair: missing/corrupt asset, redownloading",
			slog.String("file", mf.Path),
			slog.Int64("size", mf.Size))

		// downloadOne кладёт в <stageDir>/<mf.Path> — передаём exeDir как stageDir
		// чтобы файл лёг в финальное место сразу. Атомарность через .partial+rename.
		progress := &progressWriter{total: mf.Size, version: s.currentRaw, lastTS: time.Now()}
		if err := s.downloadOne(serverBase, platformKey, s.currentRaw, mf, exeDir, progress); err != nil {
			s.log.Warn("repair: download failed",
				slog.String("file", mf.Path),
				slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		repaired++
	}

	if repaired > 0 {
		s.log.Info("✅ repair complete",
			slog.Int("repaired", repaired),
			slog.Int("skipped", skipped))
	} else {
		s.log.Debug("repair: ничего перекачивать не нужно",
			slog.Int("checked", skipped))
	}

	res := &RepairResult{Repaired: repaired, Skipped: skipped}
	if firstErr != nil {
		res.Error = firstErr.Error()
	}
	return res, nil
}

// waitServerBase опрашивает getServerBase до тех пор пока не вернётся
// непустая строка (signal-сервер реально connected) или истечёт timeout.
// Используется в self-heal flow: TopicLatestRelease может выстрелить
// внутри auth_ok parsing, ДО того как NetworkService.Connect завершит установку
// connected состояния — тогда первая же fetchManifest упадёт.
func (s *UpdateService) waitServerBase(timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	if s.getServerBase != nil {
		if base := s.getServerBase(); base != "" {
			return nil
		}
	}
	for {
		select {
		case <-deadline.C:
			return fmt.Errorf("no active signal server connection (waited %s)", timeout)
		case <-tick.C:
			if s.getServerBase != nil {
				if base := s.getServerBase(); base != "" {
					return nil
				}
			}
		}
	}
}

// fileMatchesSha проверяет существует ли файл по path и совпадает ли его
// размер + sha256 с ожидаемыми. False при любой ошибке (отсутствие, IO, mismatch).
func (s *UpdateService) fileMatchesSha(path, expectedHash string, expectedSize int64) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if info.Size() != expectedSize {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return false
	}
	return hex.EncodeToString(hasher.Sum(nil)) == expectedHash
}

// ApplyUpdate применяет скачанный bundle: копирует файлы из stage в <exeDir>,
// делает backup текущего entry, удаляет файлы которых нет в новом manifest
// (diff со старым inventory), записывает новый inventory, эмитит событие.
func (s *UpdateService) ApplyUpdate() error {
	s.mu.RLock()
	stage := s.pendingStage
	entry := s.pendingEntry
	s.mu.RUnlock()
	if stage == "" || entry == nil {
		return ErrNoPendingStage
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve exe: %w", err)
	}
	exeDir := filepath.Dir(exe)
	prevVersion := s.currentRaw
	if prevVersion == "" {
		prevVersion = "unknown"
	}

	platformKey := runtime.GOOS + "-" + runtime.GOARCH
	bundle, ok := entry.Platforms[platformKey]
	if !ok {
		return ErrPlatformUnsupported
	}

	// Читаем старый inventory (если есть)
	oldInv := readInventory(exeDir)

	// Вычисляем diff: какие файлы удалять (есть в old, нет в new)
	newPaths := make(map[string]struct{}, len(bundle.Files))
	for _, f := range bundle.Files {
		newPaths[f.Path] = struct{}{}
	}
	var toDelete []string
	if oldInv != nil {
		for _, f := range oldInv.Files {
			if _, keep := newPaths[f.Path]; !keep {
				toDelete = append(toDelete, f.Path)
			}
		}
	}

	// Копируем все новые файлы кроме entry (entry обрабатываем специально
	// через platform-specific replace с backup и последующим relaunch).
	for _, f := range bundle.Files {
		if f.Path == bundle.Entry {
			continue
		}
		src := filepath.Join(stage, filepath.FromSlash(f.Path))
		dst := filepath.Join(exeDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		// Перезаписываем атомарно через rename (с промежуточным remove если cross-device)
		if err := replaceFile(src, dst); err != nil {
			return fmt.Errorf("replace %s: %w", f.Path, err)
		}
		s.log.Info("installed asset", slog.String("path", f.Path))
	}

	// Удаляем obsolete файлы + чистим пустые подкаталоги
	for _, p := range toDelete {
		full := filepath.Join(exeDir, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("failed to remove obsolete file",
				slog.String("path", p), slog.String("error", err.Error()))
		} else {
			s.log.Info("removed obsolete file", slog.String("path", p))
		}
		// Best-effort cleanup пустых подкаталогов
		removeEmptyParents(filepath.Dir(full), exeDir)
	}

	// Записываем новый inventory ДО replace entry (если replace упадёт,
	// хотим иметь актуальный snapshot assets что уже установлены)
	newInv := installedInventory{
		Version: entry.Version,
		Files:   bundle.Files,
	}
	if err := writeInventory(exeDir, &newInv); err != nil {
		s.log.Warn("failed to write inventory",
			slog.String("error", err.Error()))
	}

	// Replace entry binary (platform-specific — на Windows через .old-<ver> backup)
	stagedEntry := filepath.Join(stage, filepath.FromSlash(bundle.Entry))
	if err := applyUpdatePlatform(s.log, exe, stagedEntry, prevVersion); err != nil {
		return fmt.Errorf("replace entry: %w", err)
	}

	// Single-slot policy для entry backup: удаляем все .old-* кроме только что созданного
	keepBackup := exe + ".old-" + prevVersion
	s.pruneOldBackups(exe, keepBackup)

	// Clean stage
	_ = os.RemoveAll(stage)

	s.quitAfterApply()
	return nil
}

// --- Rollback ---

func (s *UpdateService) CanRollback() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return s.findNewestBackup(exe) != ""
}

func (s *UpdateService) Rollback() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve exe: %w", err)
	}
	newest := s.findNewestBackup(exe)
	if newest == "" {
		return errors.New("no backup to rollback")
	}
	if err := rollbackUpdatePlatform(s.log, exe, newest, s.currentRaw); err != nil {
		return err
	}
	s.quitAfterApply()
	return nil
}

// SetQuit — чем завершить процесс, когда новый уже запущен (main.go: app.Quit).
func (s *UpdateService) SetQuit(fn func()) {
	s.mu.Lock()
	s.quit = fn
	s.mu.Unlock()
}

// quitAfterApply завершает процесс прямым вызовом, не событием: app.Event.Emit
// одновременно с app.Quit взаимно блокируются в Wails v3 alpha (рассылка окнам
// держит windowsLock и ждёт главный поток, Quit на главном потоке ждёт windowsLock).
func (s *UpdateService) quitAfterApply() {
	s.mu.RLock()
	fn := s.quit
	s.mu.RUnlock()
	if fn != nil {
		go fn()
	}
}

// CleanupOldBackups удаляет stale файлы автообновления при старте приложения.
func (s *UpdateService) CleanupOldBackups() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	base := filepath.Base(exe)

	_ = os.RemoveAll(filepath.Join(dir, ".vox-update"))

	newest := s.findNewestBackup(exe)
	s.pruneOldBackups(exe, newest)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	failedPrefix := base + ".failed-"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, failedPrefix) {
			if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}

	// Asset .old-<ts> backups (от replaceFile fallback при locked DLL).
	// Имя формата `<basename>.old-<unixnano>` рядом с заменяемым файлом —
	// рекурсивно по подпапкам (models/, и т.п.). Best-effort удаление —
	// если файл всё ещё loaded в другом процессе, Remove fails, не страшно.
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		// Только .old-<digits> чтобы не зацепить .old-<version> (это exe backup).
		name := d.Name()
		idx := strings.LastIndex(name, ".old-")
		if idx < 0 {
			return nil
		}
		suffix := name[idx+len(".old-"):]
		if _, err := strconv.ParseInt(suffix, 10, 64); err != nil {
			return nil // не наш timestamp-формат (например vox.exe.old-v0.00.000.0046)
		}
		_ = os.Remove(p)
		return nil
	})
}

func (s *UpdateService) findNewestBackup(exe string) string {
	dir := filepath.Dir(exe)
	base := filepath.Base(exe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := base + ".old-"
	var newest string
	var newestTime time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestTime) {
			newest = filepath.Join(dir, name)
			newestTime = info.ModTime()
		}
	}
	return newest
}

func (s *UpdateService) pruneOldBackups(exe, keepPath string) {
	dir := filepath.Dir(exe)
	base := filepath.Base(exe)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + ".old-"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		full := filepath.Join(dir, name)
		if full == keepPath {
			continue
		}
		if err := os.Remove(full); err != nil {
			s.log.Debug("prune old backup failed",
				slog.String("path", full), slog.String("error", err.Error()))
		} else {
			s.log.Info("pruned old backup", slog.String("path", full))
		}
	}
}

// --- Manifest fetch + signature verification ---

func (s *UpdateService) fetchManifest() (*Manifest, error) {
	serverBase := ""
	if s.getServerBase != nil {
		serverBase = s.getServerBase()
	}
	if serverBase == "" {
		return nil, fmt.Errorf("no active signal server connection")
	}
	url := serverBase + "/api/update/manifest"
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("manifest get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable {
		return nil, ErrNoManifest
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest status %d", resp.StatusCode)
	}

	// Читаем raw bytes — подпись должна быть проверена над ними до парсинга.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("manifest read: %w", err)
	}

	// Верификация подписи если ключ настроен (release build)
	if s.publicKey != nil {
		sigB64 := strings.TrimSpace(resp.Header.Get("X-Signature"))
		if sigB64 == "" {
			return nil, fmt.Errorf("%w: server did not provide X-Signature", ErrVerifyFailed)
		}
		sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
		if err != nil {
			return nil, fmt.Errorf("%w: bad signature encoding: %v", ErrVerifyFailed, err)
		}
		// Подпись считалась над sha256(body) — это же делает sign-release tool
		sum := sha256.Sum256(body)
		if !ed25519.Verify(s.publicKey, sum[:], sigBytes) {
			return nil, fmt.Errorf("%w: ed25519 verify over manifest bytes", ErrVerifyFailed)
		}
	}

	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest parse: %w", err)
	}
	if m.Version != 0 && m.Version != ManifestFormatVersion {
		return nil, fmt.Errorf("unsupported manifest format version %d (want %d)",
			m.Version, ManifestFormatVersion)
	}
	return &m, nil
}

// isRevokedCurrent — текущая версия помечена в манифесте как отозванная.
func (s *UpdateService) isRevokedCurrent(m *Manifest) bool {
	for _, r := range m.Releases {
		if r.Version == s.currentRaw {
			return r.Revoked
		}
	}
	return false
}

func (s *UpdateService) isNewerVersion(ver string) bool {
	if s.currentVersion.IsZero() {
		return ver != "dev" && ver != ""
	}
	candidate, err := version.Parse(ver)
	if err != nil {
		return false
	}
	return s.currentVersion.Less(candidate)
}

func (s *UpdateService) progressEmit(data map[string]any) {
	s.mu.RLock()
	fn := s.emit
	s.mu.RUnlock()
	if fn != nil {
		fn("vox:update-progress", data)
	}
}

// --- Helpers ---

type progressWriter struct {
	total   int64
	written int64
	lastTS  time.Time
	emit    func(map[string]any)
	version string
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.written += int64(n)
	if time.Since(p.lastTS) > 200*time.Millisecond && p.emit != nil {
		p.lastTS = time.Now()
		percent := 0
		if p.total > 0 {
			percent = int(p.written * 100 / p.total)
		}
		p.emit(map[string]any{
			"version":    p.version,
			"downloaded": p.written,
			"total":      p.total,
			"percent":    percent,
		})
	}
	return n, nil
}

// replaceFile — атомарная замена dst файлом src через rename.
// На Windows если dst — загруженная DLL/exe, прямой rename падает с
// "Access is denied". Тогда переименовываем dst в <dst>.old-<timestamp>
// (Windows позволяет rename loaded file, но не remove) и кладём src на
// место dst. Stale .old-* подчистит CleanupOldBackups при следующем старте.
func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Попытка #2 для locked dst (loaded DLL): переименовать в backup.
	backup := dst + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(dst, backup); err == nil {
		if err2 := os.Rename(src, dst); err2 != nil {
			// Откатываем — вернём оригинал на место.
			_ = os.Rename(backup, dst)
			return err2
		}
		// Best-effort: попытаемся удалить backup (на Windows для loaded DLL не удалится — не страшно)
		_ = os.Remove(backup)
		return nil
	}
	// Попытка #3 (cross-device или dst отсутствует): remove+rename как раньше.
	_ = os.Remove(dst)
	return os.Rename(src, dst)
}

// removeEmptyParents удаляет пустые подкаталоги от path к stopAt включительно.
// Останавливается на первом непустом каталоге или при достижении stopAt.
func removeEmptyParents(dir, stopAt string) {
	stopAbs, _ := filepath.Abs(stopAt)
	for {
		absDir, err := filepath.Abs(dir)
		if err != nil || absDir == stopAbs {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// readInventory читает .nexus-installed.json рядом с exe.
// Возвращает nil если файла нет или он повреждён — это нормально для
// свежеустановленного экземпляра, diff просто будет пустым.
func readInventory(exeDir string) *installedInventory {
	p := filepath.Join(exeDir, installedInventoryFile)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var inv installedInventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil
	}
	return &inv
}

func writeInventory(exeDir string, inv *installedInventory) error {
	p := filepath.Join(exeDir, installedInventoryFile)
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}
