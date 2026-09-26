package logstore

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNotFound — источника или файла нет среди того, что лежит на диске.
var ErrNotFound = errors.New("logstore: not found")

// FileInfo — один файл логов источника.
type FileInfo struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// plainName — одиночное имя без разделителей и traversal.
func plainName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, `/\:`) &&
		!strings.Contains(n, "..") && !strings.ContainsRune(n, 0)
}

// sourceDir — путь каталога источника; имя сверяется с реальным содержимым
// baseDir, а не собирается из пользовательского ввода.
func (s *Store) sourceDir(source string) (string, error) {
	if !plainName(source) {
		return "", ErrNotFound
	}
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return "", ErrNotFound
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() == source {
			return filepath.Join(s.baseDir, e.Name()), nil
		}
	}
	return "", ErrNotFound
}

// SourceFiles — обычные файлы источника, самые новые первыми (timestamp в имени).
func (s *Store) SourceFiles(source string) ([]FileInfo, error) {
	dir, err := s.sourceDir(source)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, ErrNotFound
	}
	var out []FileInfo
	for _, e := range entries {
		if !e.Type().IsRegular() || !plainName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Bytes: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// FilePath — путь к файлу источника; file пусто — самый новый.
// Имя файла должно совпасть с одним из реально лежащих в каталоге.
func (s *Store) FilePath(source, file string) (string, string, error) {
	files, err := s.SourceFiles(source)
	if err != nil {
		return "", "", err
	}
	if len(files) == 0 {
		return "", "", ErrNotFound
	}
	if file == "" {
		file = files[0].Name
	}
	if !plainName(file) {
		return "", "", ErrNotFound
	}
	for _, f := range files {
		if f.Name == file {
			dir, _ := s.sourceDir(source)
			return filepath.Join(dir, f.Name), f.Name, nil
		}
	}
	return "", "", ErrNotFound
}
