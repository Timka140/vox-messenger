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
)

// SourceInfo — информация об источнике логов
type SourceInfo struct {
	Name        string    `json:"name"`
	Files       int       `json:"files"`
	TotalBytes  int64     `json:"total_bytes"`
	LastPushAt  time.Time `json:"last_push_at"`
}

// Store — хранилище debug-логов на диске
type Store struct {
	baseDir string
	mu      sync.Mutex
	chunks  map[string]*chunkBuffer
	log     *slog.Logger
}

type chunkBuffer struct {
	parts     map[int][]byte
	total     int
	createdAt time.Time
}

// New создаёт новый Store
func New(baseDir string, log *slog.Logger) *Store {
	os.MkdirAll(baseDir, 0755)
	s := &Store{
		baseDir: baseDir,
		chunks:  make(map[string]*chunkBuffer),
		log:     log,
	}
	go s.cleanupLoop()
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

	// Один чанк — сразу пишем
	if chunkTotal == 1 {
		return s.writeFile(source, scrubLog(decoded))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.chunks[source]
	if !ok || buf.total != chunkTotal {
		buf = &chunkBuffer{
			parts:     make(map[int][]byte),
			total:     chunkTotal,
			createdAt: time.Now(),
		}
		s.chunks[source] = buf
	}

	buf.parts[chunkIdx] = decoded

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

	// Ротация: оставляем последние 20 файлов. Клиент пушит при каждом
	// reconnect + на shutdown, поэтому в одной сессии может осесть несколько
	// файлов с одинаковым содержимым. 20 даёт запас, чтобы ротированные
	// .log.1..log.4 предыдущих сессий не вытеснялись повторными пушами
	// текущей сессии (клиент локально хранит 4 прошлые сессии + текущую).
	s.rotateFiles(dirPath, 20)

	return true
}

// rotateFiles оставляет maxFiles самых новых файлов в директории, остальные удаляет
func (s *Store) rotateFiles(dirPath string, maxFiles int) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}

	// Только файлы
	var files []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e)
		}
	}

	if len(files) <= maxFiles {
		return
	}

	// Сортируем по имени (timestamp в имени → хронологически)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	// Удаляем самые старые
	for _, f := range files[:len(files)-maxFiles] {
		path := filepath.Join(dirPath, f.Name())
		os.Remove(path)
		s.log.Debug("logstore: ротация, удалён старый файл", slog.String("file", f.Name()))
	}
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
