package services

import (
	"archive/zip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteLogsZip(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "vox-app.log")
	files := map[string]string{
		"vox-app.log":          "current session",
		"vox-app.log.1":        "previous 1",
		"vox-app.log.2":        "previous 2",
		"vox-app.log.1.unsent": "",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(dir, "out.zip")
	n, err := writeLogsZip(dst, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("files = %d, want 3", n)
	}

	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(b)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %v, want 3 без маркера .unsent", got)
	}
	for _, name := range []string{"vox-app.log", "vox-app.log.1", "vox-app.log.2"} {
		if got[name] != files[name] {
			t.Errorf("%s = %q, want %q", name, got[name], files[name])
		}
	}
}

func TestWriteLogsZipNoLogs(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.zip")
	if _, err := writeLogsZip(dst, filepath.Join(dir, "vox-app.log")); err == nil {
		t.Fatal("без логов — ожидалась ошибка")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("архив создан без логов")
	}
}

func TestVerboseLogging(t *testing.T) {
	a := NewAppService(slog.New(slog.NewTextHandler(io.Discard, nil)))

	lv := new(slog.LevelVar)
	lv.Set(slog.LevelInfo)
	a.SetLogControl(lv, slog.LevelInfo, "vox-app.log")
	if v := a.GetVerboseLogging(); v.On || v.Always {
		t.Fatalf("prod до включения: %+v", v)
	}
	if err := a.SetVerboseLogging(true); err != nil {
		t.Fatal(err)
	}
	if lv.Level() != slog.LevelDebug || !a.GetVerboseLogging().On {
		t.Fatalf("после включения level = %v", lv.Level())
	}
	if err := a.SetVerboseLogging(false); err != nil {
		t.Fatal(err)
	}
	if lv.Level() != slog.LevelInfo {
		t.Fatalf("после выключения level = %v, want Info", lv.Level())
	}

	// local/dev: базовый уровень уже Debug — выключение его не понижает.
	lv2 := new(slog.LevelVar)
	lv2.Set(slog.LevelDebug)
	a.SetLogControl(lv2, slog.LevelDebug, "vox-app.log")
	if v := a.GetVerboseLogging(); !v.On || !v.Always {
		t.Fatalf("local: %+v, want on+always", v)
	}
	_ = a.SetVerboseLogging(false)
	if lv2.Level() != slog.LevelDebug {
		t.Fatalf("local после выключения level = %v, want Debug", lv2.Level())
	}
}
