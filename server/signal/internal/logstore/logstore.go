package logstore

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	chunkTimeout = 30 * time.Second // таймаут сборки чанков

	// MaxPushBytes — предел одного лога после base64 decode. Больше — пуш
	// отбрасывается целиком: защита памяти (сборка чанков) и диска.
	MaxPushBytes = 16 << 20
	// clientChunkB64 — размер чанка клиента в символах base64 (SendLogPush).
	clientChunkB64 = 48000
	maxChunkTotal  = MaxPushBytes*4/3/clientChunkB64 + 2
	// maxChunkBuffers — одновременных недособранных логов.
	maxChunkBuffers = 64
)

// Options — пределы хранения. Нулевое поле — значение по умолчанию.
type Options struct {
	MaxTotal      int64         // общий размер каталога, байт
	MaxAge        time.Duration // файлы старше удаляются
	KeepPerSource int           // файлов на источник
}

const (
	defaultMaxTotal      = 1 << 30
	defaultMaxAge        = 14 * 24 * time.Hour
	defaultKeepPerSource = 10
	pruneInterval        = 10 * time.Minute
)

func (o Options) withDefaults() Options {
	if o.MaxTotal <= 0 {
		o.MaxTotal = defaultMaxTotal
	}
	if o.MaxAge <= 0 {
		o.MaxAge = defaultMaxAge
	}
	if o.KeepPerSource <= 0 {
		o.KeepPerSource = defaultKeepPerSource
	}
	return o
}

// clientSourceRe — имя источника, которое шлёт клиент: анонимный id
// установки, не ник (настройка «Отправлять анонимные логи»).
var clientSourceRe = regexp.MustCompile(`^anon-[0-9a-f]{16}$`)

// ValidClientSource — source из log_push допустим.
func ValidClientSource(source string) bool {
	return clientSourceRe.MatchString(source)
}

// SourceInfo — информация об источнике логов
type SourceInfo struct {
	Name       string    `json:"name"`
	Files      int       `json:"files"`
	TotalBytes int64     `json:"total_bytes"`
	LastPushAt time.Time `json:"last_push_at"`
}

// Store — хранилище debug-логов на диске
type Store struct {
	baseDir string
	opts    Options
	pruneMu sync.Mutex
	mu      sync.Mutex
	chunks  map[string]*chunkBuffer
	log     *slog.Logger
}

type chunkBuffer struct {
	parts     map[int][]byte
	total     int
	size      int
	createdAt time.Time
}

// New создаёт новый Store
func New(baseDir string, opts Options, log *slog.Logger) *Store {
	os.MkdirAll(baseDir, 0755)
	s := &Store{
		baseDir: baseDir,
		opts:    opts.withDefaults(),
		chunks:  make(map[string]*chunkBuffer),
		log:     log,
	}
	go s.cleanupLoop()
	go s.pruneLoop()
	return s
}

// SaveChunk сохраняет чанк логов клиента. Возвращает true если все чанки получены и файл записан.
// Собранный лог проходит scrubLog: чанки режут строки посередине, поэтому чистим целиком.
func (s *Store) SaveChunk(source string, chunkIdx, chunkTotal int, data string) bool {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		s.log.Warn("logstore: ошибка base64 decode", slog.String("source", source), slog.String("error", err.Error()))
		return false
	}

	if chunkTotal < 1 || chunkTotal > maxChunkTotal || chunkIdx < 0 || chunkIdx >= chunkTotal {
		s.log.Warn("logstore: неверный номер чанка", slog.Int("idx", chunkIdx), slog.Int("total", chunkTotal))
		return false
	}

	if len(decoded) > MaxPushBytes {
		s.log.Warn("logstore: лог больше предела, отброшен", slog.Int("limit", MaxPushBytes))
		return false
	}

	// Один чанк — сразу пишем
	if chunkTotal == 1 {
		return s.writeFile(source, scrubLog(decoded))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.chunks[source]
	if !ok || buf.total != chunkTotal {
		if !ok && len(s.chunks) >= maxChunkBuffers {
			s.log.Warn("logstore: слишком много недособранных логов, пуш отброшен")
			return false
		}
		buf = &chunkBuffer{
			parts:     make(map[int][]byte),
			total:     chunkTotal,
			createdAt: time.Now(),
		}
		s.chunks[source] = buf
	}

	if old, dup := buf.parts[chunkIdx]; dup {
		buf.size -= len(old)
	}
	buf.parts[chunkIdx] = decoded
	buf.size += len(decoded)
	if buf.size > MaxPushBytes {
		s.log.Warn("logstore: лог больше предела, отброшен", slog.Int("limit", MaxPushBytes))
		delete(s.chunks, source)
		return false
	}

	if len(buf.parts) < chunkTotal {
		return false
	}

	// Все чанки получены — собираем
	var full []byte
	for i := 0; i < chunkTotal; i++ {
		full = append(full, buf.parts[i]...)
	}
	delete(s.chunks, source)

	return s.writeFile(source, scrubLog(full))
}

// SaveFromFile копирует лог-файл в хранилище
func (s *Store) SaveFromFile(source, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("logstore: read %s: %w", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxPushBytes {
		data = data[len(data)-MaxPushBytes:]
	}
	s.writeFile(source, data)
	return nil
}

// ListSources возвращает список источников логов
func (s *Store) ListSources() []SourceInfo {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil
	}

	var sources []SourceInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		si := SourceInfo{Name: e.Name()}
		dirPath := filepath.Join(s.baseDir, e.Name())
		files, _ := os.ReadDir(dirPath)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			si.Files++
			info, err := f.Info()
			if err == nil {
				si.TotalBytes += info.Size()
				if info.ModTime().After(si.LastPushAt) {
					si.LastPushAt = info.ModTime()
				}
			}
		}
		sources = append(sources, si)
	}
	return sources
}

// ReadLogs читает все логи источника (опционально с фильтром по времени)
func (s *Store) ReadLogs(source string, since time.Time) ([]byte, error) {
	safeName := sanitize(source)
	dirPath := filepath.Join(s.baseDir, safeName)

	files, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("logstore: read dir %s: %w", safeName, err)
	}

	// Сортируем по имени (хронологически, т.к. timestamp в имени)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	var result []byte
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if !since.IsZero() {
			info, err := f.Info()
			if err == nil && info.ModTime().Before(since) {
				continue
			}
		}
		data, err := os.ReadFile(filepath.Join(dirPath, f.Name()))
		if err != nil {
			continue
		}
		result = append(result, data...)
	}
	return result, nil
}

// ReadAllLogs читает логи всех источников
func (s *Store) ReadAllLogs(since time.Time) map[string][]byte {
	sources := s.ListSources()
	result := make(map[string][]byte, len(sources))
	for _, src := range sources {
		data, err := s.ReadLogs(src.Name, since)
		if err == nil && len(data) > 0 {
			result[src.Name] = data
		}
	}
	return result
}

// writeFile записывает данные в новый файл с timestamp
func (s *Store) writeFile(source string, data []byte) bool {
	safeName := sanitize(source)
	dirPath := filepath.Join(s.baseDir, safeName)
	os.MkdirAll(dirPath, 0755)

	ts := time.Now().Format("2006-01-02T15-04-05")
	filePath := filepath.Join(dirPath, ts+".jsonl")

	// Если файл с таким именем уже есть — добавляем суффикс
	if _, err := os.Stat(filePath); err == nil {
		filePath = filepath.Join(dirPath, ts+"_"+fmt.Sprintf("%d", time.Now().UnixMilli()%1000)+".jsonl")
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		s.log.Warn("logstore: ошибка записи", slog.String("path", filePath), slog.String("error", err.Error()))
		return false
	}

	s.log.Info("📁 логи сохранены",
		slog.String("source", source),
		slog.String("file", filepath.Base(filePath)),
		slog.Int("bytes", len(data)))

	// Пределы хранения (срок, файлов на источник, общий размер) — prune.go.
	s.Prune()

	return true
}

// cleanupLoop чистит просроченные chunk-буферы
func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for key, buf := range s.chunks {
			if time.Since(buf.createdAt) > chunkTimeout {
				s.log.Warn("logstore: chunk timeout, дропаю неполный",
					slog.String("source", key),
					slog.Int("received", len(buf.parts)),
					slog.Int("expected", buf.total))
				delete(s.chunks, key)
			}
		}
		s.mu.Unlock()
	}
}

var safeRe = regexp.MustCompile(`[^a-zA-Z0-9#_\-]`)

// SourceName — имя каталога (источника), под которым сохраняются логи source.
func SourceName(source string) string {
	return sanitize(source)
}

func sanitize(name string) string {
	s := safeRe.ReplaceAllString(name, "_")
	s = strings.TrimRight(s, "_.")
	if s == "" {
		s = "unknown"
	}
	return s
}

// CopyReader — вспомогательная функция для копирования содержимого reader
func CopyReader(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
