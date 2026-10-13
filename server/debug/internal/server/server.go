package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg        Config
	log        *slog.Logger
	httpServer *http.Server
}

type Config struct {
	HTTPHost        string
	HTTPPort        int
	SignalDebugAddr string
	LogDir          string
}

type SourceInfo struct {
	Name       string `json:"name"`
	Files      int    `json:"files"`
	TotalBytes int64  `json:"total_bytes"`
	LastPushAt string `json:"last_push_at,omitempty"`
	LatestFile string `json:"latest_file,omitempty"`
}

// logEntry — одна строка лога для сортировки
type logEntry struct {
	Time   string `json:"time"`
	Source string `json:"source,omitempty"`
	raw    []byte
}

func New(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log}
}

func (s *Server) Start() error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/sources", s.handleSources)
	mux.HandleFunc("GET /api/logs/{source}/latest", s.handleLatestLog)
	mux.HandleFunc("GET /api/logs/{source}", s.handleLogsBySource)
	mux.HandleFunc("GET /api/logs", s.handleAllLogs)
	mux.HandleFunc("POST /api/collect", s.handleCollect)
	mux.HandleFunc("POST /api/collect/{peer}", s.handleCollectPeer)
	mux.HandleFunc("GET /api/status", s.handleStatus)

	addr := net.JoinHostPort(s.cfg.HTTPHost, strconv.Itoa(s.cfg.HTTPPort))
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      withCORS(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	s.log.Info("debug service запускается", slog.String("addr", addr))
	return s.httpServer.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// GET /api/sources — список источников
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.LogDir)
	if err != nil {
		s.jsonError(w, "не удалось прочитать директорию логов", http.StatusInternalServerError)
		return
	}

	var sources []SourceInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		si := SourceInfo{Name: e.Name()}
		dirPath := filepath.Join(s.cfg.LogDir, e.Name())
		files, _ := os.ReadDir(dirPath)
		var latest time.Time
		var latestName string
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			si.Files++
			info, err := f.Info()
			if err == nil {
				si.TotalBytes += info.Size()
				if info.ModTime().After(latest) {
					latest = info.ModTime()
					latestName = f.Name()
				}
			}
		}
		if !latest.IsZero() {
			si.LastPushAt = latest.Format(time.RFC3339)
			si.LatestFile = latestName
		}
		sources = append(sources, si)
	}

	s.jsonOK(w, sources)
}

// GET /api/logs/{source} — логи конкретного источника
func (s *Server) handleLogsBySource(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if source == "" {
		s.jsonError(w, "source required", http.StatusBadRequest)
		return
	}

	data, err := s.readSourceLogs(source)
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Write(data)
}

// GET /api/logs/{source}/latest — только последний лог-файл источника
func (s *Server) handleLatestLog(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if source == "" {
		s.jsonError(w, "source required", http.StatusBadRequest)
		return
	}

	dirPath := filepath.Join(s.cfg.LogDir, source)
	files, err := os.ReadDir(dirPath)
	if err != nil {
		s.jsonError(w, fmt.Sprintf("source '%s' not found", source), http.StatusNotFound)
		return
	}

	// Находим самый новый файл
	var latestName string
	var latestMod time.Time
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		info, err := f.Info()
		if err == nil && info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latestName = f.Name()
		}
	}

	if latestName == "" {
		s.jsonError(w, "no logs", http.StatusNotFound)
		return
	}

	data, err := os.ReadFile(filepath.Join(dirPath, latestName))
	if err != nil {
		s.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Log-File", latestName)
	w.Write(data)
}

// GET /api/logs — все логи, merged по timestamp
func (s *Server) handleAllLogs(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.LogDir)
	if err != nil {
		s.jsonError(w, "не удалось прочитать директорию логов", http.StatusInternalServerError)
		return
	}

	var allEntries []logEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := s.readSourceLogs(e.Name())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry logEntry
			json.Unmarshal([]byte(line), &entry)
			entry.Source = e.Name()

			// Вставляем source в JSON
			modified := addSourceField([]byte(line), e.Name())
			entry.raw = modified
			allEntries = append(allEntries, entry)
		}
	}

	// Сортируем по timestamp
	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].Time < allEntries[j].Time
	})

	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, e := range allEntries {
		w.Write(e.raw)
		w.Write([]byte("\n"))
	}
}

// POST /api/collect — триггер сбора логов с онлайн клиентов
func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	resp, err := http.Post("http://"+s.cfg.SignalDebugAddr+"/collect", "application/json", nil)
	if err != nil {
		s.jsonError(w, "не удалось подключиться к signal debug: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// POST /api/collect/{peer} — сбор от конкретного пира (через signal)
func (s *Server) handleCollectPeer(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	resp, err := http.Post("http://"+s.cfg.SignalDebugAddr+"/collect?peer="+peer, "application/json", nil)
	if err != nil {
		s.jsonError(w, "не удалось подключиться к signal debug: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// GET /api/status — статус signal server + онлайн пиры
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{
		"debug_service": "ok",
		"log_dir":       s.cfg.LogDir,
	}

	// Проверяем signal server health
	healthResp, err := http.Get("http://" + s.cfg.SignalDebugAddr + "/../health")
	if err == nil {
		defer healthResp.Body.Close()
		var health map[string]any
		if json.NewDecoder(healthResp.Body).Decode(&health) == nil {
			result["signal_health"] = health
		}
	} else {
		result["signal_health"] = "unavailable"
	}

	// Получаем список пиров
	peersResp, err := http.Get("http://" + s.cfg.SignalDebugAddr + "/peers")
	if err == nil {
		defer peersResp.Body.Close()
		var peers []any
		if json.NewDecoder(peersResp.Body).Decode(&peers) == nil {
			result["online_peers"] = peers
		}
	}

	s.jsonOK(w, result)
}

// readSourceLogs читает все файлы логов из директории источника
func (s *Server) readSourceLogs(source string) ([]byte, error) {
	dirPath := filepath.Join(s.cfg.LogDir, source)
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("source '%s' not found", source)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	var result []byte
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dirPath, f.Name()))
		if err != nil {
			continue
		}
		result = append(result, data...)
	}
	return result, nil
}

// addSourceField добавляет поле "source" в JSON строку
func addSourceField(line []byte, source string) []byte {
	// Вставляем после первого '{' чтобы source был первым полем
	if len(line) < 2 || line[0] != '{' {
		return line
	}
	prefix := fmt.Sprintf(`{"source":"%s",`, source)
	return append([]byte(prefix), line[1:]...)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) jsonOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *Server) jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
