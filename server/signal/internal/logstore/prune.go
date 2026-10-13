package logstore

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// storedFile — файл лога при обходе хранилища.
type storedFile struct {
	path    string
	source  string
	name    string
	size    int64
	modTime time.Time
}

// Prune держит хранилище в пределах Options: удаляет файлы старше MaxAge,
// лишние сверх KeepPerSource в каждом источнике, затем самые старые по всему
// хранилищу, пока общий размер больше MaxTotal; пустые каталоги источников
// удаляются. Диск узла маленький, поэтому чистим после каждой записи и по
// таймеру.
func (s *Store) Prune() {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()

	sources, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}
	opts := s.opts.withDefaults()
	cutoff := time.Now().Add(-opts.MaxAge)
	var kept []storedFile
	removed := 0
	remove := func(f storedFile) {
		if err := os.Remove(f.path); err == nil {
			removed++
		}
	}

	for _, src := range sources {
		if !src.IsDir() {
			continue
		}
		dir := filepath.Join(s.baseDir, src.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var files []storedFile
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			f := storedFile{
				path: filepath.Join(dir, e.Name()), source: src.Name(), name: e.Name(),
				size: info.Size(), modTime: info.ModTime(),
			}
			if f.modTime.Before(cutoff) {
				remove(f)
				continue
			}
			files = append(files, f)
		}
		// Имя — timestamp, сортировка по имени хронологическая.
		sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
		if extra := len(files) - opts.KeepPerSource; extra > 0 {
			for _, f := range files[:extra] {
				remove(f)
			}
			files = files[extra:]
		}
		if len(files) == 0 {
			_ = os.Remove(dir) // только пустой; с подкаталогами не удалится
			continue
		}
		kept = append(kept, files...)
	}

	var total int64
	for _, f := range kept {
		total += f.size
	}
	if total > opts.MaxTotal {
		sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
		emptied := map[string]bool{}
		for _, f := range kept {
			if total <= opts.MaxTotal {
				break
			}
			remove(f)
			total -= f.size
			emptied[filepath.Dir(f.path)] = true
		}
		for dir := range emptied {
			_ = os.Remove(dir)
		}
	}

	if removed > 0 {
		s.log.Info("logstore: старые логи удалены",
			slog.Int("files", removed),
			slog.Int64("total_bytes", total))
	}
}

// pruneLoop — Prune по таймеру: срок хранения истекает и без новых записей.
func (s *Store) pruneLoop() {
	s.Prune()
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.Prune()
	}
}
