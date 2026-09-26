package releases

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeManifest(t *testing.T, dir string, m *Manifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGetEmpty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, err := s.Get(); err == nil {
		t.Error("expected error for missing manifest")
	}
	if info := s.LatestReleaseInfo(); info != nil {
		t.Errorf("expected nil LatestReleaseInfo, got %+v", info)
	}
}

func TestGetAndLatest(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Version: ManifestFormatVersion,
		Latest:  "v0.00.000.0001",
		Releases: []ReleaseEntry{
			{
				Version:    "v0.00.000.0001",
				ReleasedAt: time.Now(),
				Changelog:  "initial",
				Force:      false,
				Platforms: map[string]PlatformBundle{
					"windows-amd64": {
						Entry: "nexus.exe",
						Files: []ManifestFile{
							{Path: "nexus.exe", Size: 100, SHA256: "abc"},
							{Path: "models/DeepFilterNet3.tar.gz", Size: 7978218, SHA256: "def"},
						},
					},
				},
			},
		},
	}
	writeManifest(t, dir, m)

	s := NewStore(dir)
	got, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got.Latest != "v0.00.000.0001" {
		t.Errorf("Latest = %q", got.Latest)
	}
	bundle, ok := got.Releases[0].Platforms["windows-amd64"]
	if !ok {
		t.Fatal("windows-amd64 bundle missing")
	}
	if bundle.Entry != "nexus.exe" {
		t.Errorf("Entry = %q", bundle.Entry)
	}
	if len(bundle.Files) != 2 {
		t.Errorf("Files count = %d, want 2", len(bundle.Files))
	}
	info := s.LatestReleaseInfo()
	if info == nil || info.Version != "v0.00.000.0001" {
		t.Errorf("LatestReleaseInfo = %+v", info)
	}
}

func TestGetBytes(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Version:  ManifestFormatVersion,
		Latest:   "v0.00.000.0001",
		Releases: []ReleaseEntry{{Version: "v0.00.000.0001", Changelog: "x"}},
	}
	writeManifest(t, dir, m)
	// Пишем фейковую подпись
	if err := os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte("ZmFrZS1zaWc=\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	body, sig, err := s.GetBytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Error("empty body")
	}
	if string(sig) != "ZmFrZS1zaWc=\n" {
		t.Errorf("sig = %q", string(sig))
	}
}

func TestReloadOnMtime(t *testing.T) {
	dir := t.TempDir()
	m1 := &Manifest{Version: ManifestFormatVersion, Latest: "v0.00.000.0001", Releases: []ReleaseEntry{{Version: "v0.00.000.0001", Changelog: "first"}}}
	writeManifest(t, dir, m1)
	s := NewStore(dir)
	got1, _ := s.Get()
	if got1.Latest != "v0.00.000.0001" {
		t.Fatal("first get")
	}

	time.Sleep(10 * time.Millisecond)
	m2 := &Manifest{Version: ManifestFormatVersion, Latest: "v0.00.000.0002", Releases: []ReleaseEntry{{Version: "v0.00.000.0002", Changelog: "second"}}}
	writeManifest(t, dir, m2)
	past := time.Now().Add(time.Second)
	_ = os.Chtimes(filepath.Join(dir, "manifest.json"), past, past)

	got2, err := s.Get()
	if err != nil {
		t.Fatal(err)
	}
	if got2.Latest != "v0.00.000.0002" {
		t.Errorf("after reload Latest = %q, want v0.00.000.0002", got2.Latest)
	}
}

func TestArtifactPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// Создаём легальный файл для первых проверок
	platformDir := filepath.Join(dir, "windows-amd64", "v0.00.000.0001")
	_ = os.MkdirAll(platformDir, 0755)
	legalFile := filepath.Join(platformDir, "nexus.exe")
	_ = os.WriteFile(legalFile, []byte("fake"), 0644)
	// Файл во вложенной папке models
	modelsDir := filepath.Join(platformDir, "models")
	_ = os.MkdirAll(modelsDir, 0755)
	modelFile := filepath.Join(modelsDir, "DeepFilterNet3.tar.gz")
	_ = os.WriteFile(modelFile, []byte("fake-model"), 0644)

	s := NewStore(dir)

	// Легальные запросы
	if _, err := s.ArtifactPath("windows-amd64", "v0.00.000.0001", "nexus.exe"); err != nil {
		t.Errorf("legal path (root): %v", err)
	}
	if _, err := s.ArtifactPath("windows-amd64", "v0.00.000.0001", "models/DeepFilterNet3.tar.gz"); err != nil {
		t.Errorf("legal path (nested): %v", err)
	}

	// Нелегальные
	badCases := [][3]string{
		{"..", "v0.00.000.0001", "nexus.exe"},
		{"windows-amd64", "..", "nexus.exe"},
		{"windows-amd64", "v0.00.000.0001", ".."},
		{"windows-amd64", "v0.00.000.0001", "../etc/passwd"},
		{"windows-amd64", "v0.00.000.0001", "models/../../../etc/passwd"},
		{"", "v0.00.000.0001", "nexus.exe"},
		{"windows-amd64", "", "nexus.exe"},
		{"windows-amd64", "v0.00.000.0001", ""},
		{"windows-amd64", "v0.00.000.0001", "/etc/passwd"},
		{"windows-amd64", "v0.00.000.0001", "models//bad"},
		{"windows-amd64", "v0.00.000.0001", "models/./bad"},
	}
	if runtime.GOOS != "windows" {
		badCases = append(badCases, [3]string{"windows-amd64", "v0.00.000.0001", "nexus\\.exe"})
	}
	for _, c := range badCases {
		if _, err := s.ArtifactPath(c[0], c[1], c[2]); err == nil {
			t.Errorf("traversal allowed: %v", c)
		}
	}
}

func TestArtifactPathNonExistent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if _, err := s.ArtifactPath("windows-amd64", "v0.00.000.0001", "nexus.exe"); err == nil {
		t.Error("expected error for non-existent artifact")
	}
}

func TestLatestMissingFromReleases(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Version: ManifestFormatVersion,
		Latest:  "v0.00.000.0002",
		Releases: []ReleaseEntry{
			{Version: "v0.00.000.0001", Changelog: "old"},
		},
	}
	writeManifest(t, dir, m)
	s := NewStore(dir)
	if info := s.LatestReleaseInfo(); info != nil {
		t.Errorf("expected nil when Latest not in Releases, got %+v", info)
	}
}
