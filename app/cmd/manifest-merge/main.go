// manifest-merge — утиль для сборки manifest.json v2.
//
// Работа со structure:
//
//	<artifacts>/
//	  <platform>/
//	    nexus[.exe]            ← основной бинарь (entry)
//	    models/
//	      DeepFilterNet3.tar.gz
//	      silero_vad.onnx
//	    runtime/
//	      onnxruntime.dll
//
// Рекурсивно обходит каждую платформу, считает sha256 каждого файла,
// формирует ReleaseEntry с полным списком files[]. Определяет entry по
// наличию vox/vox.exe в корне platform-папки.
//
// Подпись манифеста создаётся отдельным шагом в release.sh через sign-release
// (sha256 файла manifest.json + Ed25519 signature → manifest.json.sig).
//
// Usage:
//
//	manifest-merge -existing <path> -out <path> \
//	               -version vX.YY.ZZZ.WWWW -changelog "msg" -force=false \
//	               -artifacts <release-dir> [-keep N] [-pruned-out pruned.txt]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- Типы манифеста v2 (синхронизированы с server/signal/internal/releases) ---

const ManifestFormatVersion = 2

type Manifest struct {
	Version  int            `json:"version"`
	Latest   string         `json:"latest"`
	Releases []ReleaseEntry `json:"releases"`
}

type ReleaseEntry struct {
	Version    string    `json:"version"`
	ReleasedAt time.Time `json:"released_at"`
	Changelog  string    `json:"changelog"`
	Force      bool      `json:"force"`
	// Revoked — версия отозвана в админке; merge обязан сохранить флаг.
	Revoked   bool                      `json:"revoked,omitempty"`
	Platforms map[string]PlatformBundle `json:"platforms"`
}

type PlatformBundle struct {
	Entry string         `json:"entry"`
	Files []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func main() {
	var (
		existingPath = flag.String("existing", "", "path to existing manifest.json (optional)")
		outPath      = flag.String("out", "", "output manifest.json")
		version      = flag.String("version", "", "release version, e.g. v0.00.000.0001")
		changelog    = flag.String("changelog", "", "release changelog")
		force        = flag.Bool("force", false, "force update (UI blocks until installed)")
		artifactsDir = flag.String("artifacts", "", "directory containing <platform>/... subdirs")
		keep         = flag.Int("keep", 0, "keep only N latest releases (0 = keep all)")
		prunedOut    = flag.String("pruned-out", "", "if -keep>0, write list of pruned versions to this file")
	)
	flag.Parse()

	if *outPath == "" || *version == "" || *artifactsDir == "" {
		fmt.Fprintln(os.Stderr, "usage: manifest-merge -out <path> -version <vX.YY.ZZZ.WWWW> -artifacts <dir> [...]")
		os.Exit(2)
	}

	// Load existing
	var m Manifest
	if *existingPath != "" {
		if data, err := os.ReadFile(*existingPath); err == nil {
			if err := json.Unmarshal(data, &m); err != nil {
				fmt.Fprintln(os.Stderr, "warning: failed to parse existing manifest, starting fresh:", err)
				m = Manifest{}
			}
			if m.Version != 0 && m.Version != ManifestFormatVersion {
				fmt.Fprintf(os.Stderr, "warning: existing manifest is v%d, upgrading to v%d\n",
					m.Version, ManifestFormatVersion)
				m = Manifest{}
			}
		}
	}

	// Scan artifacts dir for <platform>/... recursively
	entry := ReleaseEntry{
		Version:    *version,
		ReleasedAt: time.Now().UTC(),
		Changelog:  *changelog,
		Force:      *force,
		Platforms:  map[string]PlatformBundle{},
	}

	platformDirs, err := os.ReadDir(*artifactsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read artifacts dir:", err)
		os.Exit(1)
	}
	for _, e := range platformDirs {
		if !e.IsDir() {
			continue
		}
		platform := e.Name()
		if !strings.Contains(platform, "-") {
			continue // не платформа (напр. existing.json, pruned.txt)
		}
		platDir := filepath.Join(*artifactsDir, platform)

		bundle, err := scanPlatformBundle(platDir, platform)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: scan %s: %v\n", platform, err)
			continue
		}
		if len(bundle.Files) == 0 {
			continue
		}
		if bundle.Entry == "" {
			fmt.Fprintf(os.Stderr, "warning: %s has no entry (vox/vox.exe), skipping\n", platform)
			continue
		}
		entry.Platforms[platform] = bundle

		totalSize := int64(0)
		for _, f := range bundle.Files {
			totalSize += f.Size
		}
		fmt.Printf("added: %s entry=%s files=%d total=%d bytes\n",
			platform, bundle.Entry, len(bundle.Files), totalSize)
		for _, f := range bundle.Files {
			fmt.Printf("       %s (%d bytes, sha256=%s)\n", f.Path, f.Size, f.SHA256)
		}
	}

	if len(entry.Platforms) == 0 {
		fmt.Fprintln(os.Stderr, "error: no platform bundles found in", *artifactsDir)
		os.Exit(1)
	}

	// Replace или append
	filtered := make([]ReleaseEntry, 0, len(m.Releases))
	for _, r := range m.Releases {
		if r.Version != *version {
			filtered = append(filtered, r)
		}
	}
	filtered = append(filtered, entry)

	sort.Slice(filtered, func(i, j int) bool {
		return compareVersions(filtered[i].Version, filtered[j].Version) > 0
	})

	var pruned []string
	if *keep > 0 && len(filtered) > *keep {
		for _, r := range filtered[*keep:] {
			pruned = append(pruned, r.Version)
		}
		filtered = filtered[:*keep]
		fmt.Printf("pruned %d old releases (kept %d): %s\n",
			len(pruned), *keep, strings.Join(pruned, ", "))
	}
	for _, p := range pruned {
		if p == *version {
			fmt.Fprintf(os.Stderr,
				"warning: released version %s is older than newest in manifest and was pruned\n",
				*version)
			break
		}
	}

	m.Version = ManifestFormatVersion
	m.Releases = filtered
	if len(filtered) > 0 {
		m.Latest = filtered[0].Version
	} else {
		m.Latest = *version
	}

	if *prunedOut != "" {
		_ = os.WriteFile(*prunedOut, []byte(strings.Join(pruned, "\n")), 0644)
	}

	outBytes, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(*outPath, outBytes, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write manifest:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote manifest: %s (latest=%s, total=%d)\n", *outPath, m.Latest, len(m.Releases))
}

// scanPlatformBundle рекурсивно обходит platDir и собирает PlatformBundle.
// Игнорирует скрытые файлы/папки (начинающиеся с ".") и .sig файлы (legacy).
// Entry определяется по наличию vox или vox.exe в корне platDir.
func scanPlatformBundle(platDir, platform string) (PlatformBundle, error) {
	var bundle PlatformBundle

	// Определяем entry
	entryName := "vox"
	if strings.HasPrefix(platform, "windows") {
		entryName = "vox.exe"
	}

	err := filepath.WalkDir(platDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		// Пропускаем скрытые (.vox-update, .DS_Store и т.п.)
		if strings.HasPrefix(name, ".") && p != platDir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Пропускаем legacy .sig файлы
		if strings.HasSuffix(name, ".sig") {
			return nil
		}

		rel, err := filepath.Rel(platDir, p)
		if err != nil {
			return err
		}
		// POSIX-style path в манифесте
		posixPath := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		sum, err := sha256File(p)
		if err != nil {
			return fmt.Errorf("hash %s: %w", posixPath, err)
		}

		bundle.Files = append(bundle.Files, ManifestFile{
			Path:   posixPath,
			Size:   info.Size(),
			SHA256: sum,
		})

		if posixPath == entryName {
			bundle.Entry = entryName
		}
		return nil
	})
	if err != nil {
		return PlatformBundle{}, err
	}

	// Sort для детерминированного манифеста
	sort.Slice(bundle.Files, func(i, j int) bool {
		return bundle.Files[i].Path < bundle.Files[j].Path
	})
	return bundle, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func compareVersions(a, b string) int {
	pa := parseV(a)
	pb := parseV(b)
	for i := 0; i < 4; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseV(s string) [4]int {
	var r [4]int
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	for i := 0; i < 4 && i < len(parts); i++ {
		n, _ := strconv.Atoi(parts[i])
		r[i] = n
	}
	return r
}
