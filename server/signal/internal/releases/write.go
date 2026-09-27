package releases

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrInvalid — манифест не прошёл проверку (ошибка запроса, не сервера).
var ErrInvalid = errors.New("releases: invalid manifest")

// Validate разбирает манифест и проверяет, что latest есть среди releases,
// а файлы каждой версии реально лежат в каталоге релизов.
func (s *Store) Validate(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: json: %v", ErrInvalid, err)
	}
	if m.Version != 0 && m.Version != ManifestFormatVersion {
		return nil, fmt.Errorf("%w: format version %d", ErrInvalid, m.Version)
	}
	if m.Latest == "" {
		return nil, fmt.Errorf("%w: latest пуст", ErrInvalid)
	}
	found := false
	seen := make(map[string]bool, len(m.Releases))
	for _, r := range m.Releases {
		if r.Version == "" || seen[r.Version] {
			return nil, fmt.Errorf("%w: пустая или повторная версия %q", ErrInvalid, r.Version)
		}
		seen[r.Version] = true
		if r.Version == m.Latest {
			found = true
			if r.Revoked {
				return nil, fmt.Errorf("%w: latest %q отозвана", ErrInvalid, m.Latest)
			}
		}
		// Откат только на то, что есть на диске.
		for plat, b := range r.Platforms {
			for _, f := range b.Files {
				abs, err := s.ArtifactPath(plat, r.Version, f.Path)
				if err != nil {
					return nil, fmt.Errorf("%w: нет файла %s/%s/%s", ErrInvalid, plat, r.Version, f.Path)
				}
				st, err := os.Stat(abs)
				if err != nil || !st.Mode().IsRegular() {
					return nil, fmt.Errorf("%w: нет файла %s/%s/%s", ErrInvalid, plat, r.Version, f.Path)
				}
				if f.Size > 0 && st.Size() != f.Size {
					return nil, fmt.Errorf("%w: размер %s/%s/%s: на диске %d, в манифесте %d",
						ErrInvalid, plat, r.Version, f.Path, st.Size(), f.Size)
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: latest %q нет среди releases", ErrInvalid, m.Latest)
	}
	return &m, nil
}

// Replace атомарно пишет manifest.json и manifest.json.sig (tmp + rename)
// и сразу перечитывает кеш — /api/update/manifest отдаёт новое без ожидания
// смены mtime. Подпись проверяет вызывающий.
func (s *Store) Replace(raw []byte, sigB64 string) (*Manifest, error) {
	if s.dir == "" {
		return nil, fmt.Errorf("releases: dir not configured")
	}
	m, err := s.Validate(raw)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Сначала подпись, потом манифест: кеш перечитывается по mtime манифеста,
	// так что читатель со стороны увидит новую пару не позже смены manifest.json.
	sig := []byte(strings.TrimSpace(sigB64) + "\n")
	if err := writeAtomic(filepath.Join(s.dir, "manifest.json.sig"), sig); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(s.dir, "manifest.json")
	if err := writeAtomic(manifestPath, raw); err != nil {
		return nil, err
	}
	st, err := os.Stat(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("releases: stat manifest: %w", err)
	}

	body := make([]byte, len(raw))
	copy(body, raw)
	s.m = m
	s.rawBytes = body
	s.sig = sig
	s.mtime = st.ModTime()
	return m, nil
}

// writeAtomic — запись во временный файл рядом и rename поверх.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("releases: tmp: %w", err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("releases: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("releases: sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("releases: close tmp: %w", err)
	}
	if err := os.Chmod(tmp, 0644); err != nil {
		return fmt.Errorf("releases: chmod tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("releases: rename: %w", err)
	}
	ok = true
	return nil
}
