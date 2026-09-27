package services

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vox_app/internal/network"
)

var thisPlatform = runtime.GOOS + "-" + runtime.GOARCH

// newRevokedTestService — сервис с сервером, отдающим подписанный манифест m.
// События vox:update-available приходят в канал (auth_ok сверяется асинхронно).
func newRevokedTestService(t *testing.T, current string, m *Manifest) (*UpdateService, chan map[string]any) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := ""
	if m != nil {
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sum[:]))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Signature", sig)
			w.Write(body)
		}))
		t.Cleanup(srv.Close)
		base = srv.URL
	}
	s := NewUpdateService(slog.New(slog.NewTextHandler(io.Discard, nil)), current,
		base64.StdEncoding.EncodeToString(pub), func() string { return base })
	s.retryDelays = nil
	got := make(chan map[string]any, 4)
	s.SetEmitter(func(name string, data any) {
		if name == "vox:update-available" {
			got <- data.(map[string]any)
		}
	})
	return s, got
}

func withBuild() map[string]ManifestPlatformBundle {
	return map[string]ManifestPlatformBundle{thisPlatform: {Entry: "vox", Files: []ManifestFile{{}}}}
}

func expectEvent(t *testing.T, got chan map[string]any) map[string]any {
	t.Helper()
	select {
	case ev := <-got:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("событие vox:update-available не пришло")
		return nil
	}
}

func expectNoEvent(t *testing.T, got chan map[string]any) {
	t.Helper()
	select {
	case ev := <-got:
		t.Fatalf("событие не ожидалось: %v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// Откат из админки: клиент на отозванной версии получает latest, даже если он старше.
func TestAuthOKRevokedCurrentOffersDowngrade(t *testing.T) {
	s, got := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0082", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: withBuild()},
	}})
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	ev := expectEvent(t, got)
	if ev["version"] != "v0.00.000.0082" || ev["force"] != true || ev["revoked"] != true {
		t.Fatalf("ожидалось принудительное обновление до 0082, получено %v", ev)
	}
}

// Неподписанный revoked в auth_ok без подтверждения манифестом не откатывает.
func TestAuthOKRevokedNotInManifestIgnored(t *testing.T) {
	s, got := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: withBuild()},
	}})
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	expectNoEvent(t, got)
}

// Откат на релиз без сборки под эту платформу не показывает force-модалку.
func TestAuthOKRevokedNoBuildForPlatformIgnored(t *testing.T) {
	other := map[string]ManifestPlatformBundle{"plan9-mips": {Entry: "vox", Files: []ManifestFile{{}}}}
	s, got := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0082", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: other},
	}})
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	expectNoEvent(t, got)
}

func TestAuthOKOlderNotRevokedIgnored(t *testing.T) {
	s, got := newRevokedTestService(t, "v0.00.000.0083", nil)
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0081"},
	})
	expectNoEvent(t, got)
}

// DownloadUpdate не ставит старшую версию, если манифест не отзывал текущую.
func TestDownloadRefusesDowngradeWithoutRevoke(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: withBuild()},
	}})
	_, err := s.DownloadUpdate("v0.00.000.0082")
	if err == nil || !strings.Contains(err.Error(), "not an update") {
		t.Fatalf("ожидался отказ, получено %v", err)
	}
}

func TestManifestRevokedCurrent(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083", nil)
	m := &Manifest{Latest: "v0.00.000.0082", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true},
		{Version: "v0.00.000.0082"},
	}}
	if !s.isRevokedCurrent(m) {
		t.Fatal("текущая 0083 отозвана")
	}
	m.Releases[0].Revoked = false
	if s.isRevokedCurrent(m) {
		t.Fatal("без флага — не отозвана")
	}
}
// Отозванная текущая версия, которую манифест же назначил latest, — не
// повод переустанавливать её саму (и затирать слот отката).
func TestDownloadUpdateRefusesRevokedCurrentAsLatest(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true, Platforms: withBuild()},
	}})
	_, err := s.DownloadUpdate("v0.00.000.0083")
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("ожидали отказ, получили %v", err)
	}
}

// Манифест не получен с первого раза (рестарт узла) — уведомление об отзыве
// всё равно доходит после повтора.
func TestAuthOKRetriesManifestFetch(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(&Manifest{Latest: "v0.00.000.0082", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: withBuild()},
	}})
	sum := sha256.Sum256(body)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sum[:]))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Signature", sig)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	s := NewUpdateService(slog.New(slog.NewTextHandler(io.Discard, nil)), "v0.00.000.0083",
		base64.StdEncoding.EncodeToString(pub), func() string { return srv.URL })
	s.retryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	got := make(chan map[string]any, 4)
	s.SetEmitter(func(name string, data any) {
		if name == "vox:update-available" {
			got <- data.(map[string]any)
		}
	})
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	if ev := expectEvent(t, got); ev["version"] != "v0.00.000.0082" || ev["revoked"] != true {
		t.Fatalf("ожидался откат до 0082, получено %v", ev)
	}
	if lr := s.GetLatestRelease(); lr == nil || lr.Version != "v0.00.000.0082" {
		t.Fatalf("GetLatestRelease: %+v", lr)
	}
}

// Баннер остался от прошлой проверки, а предложенную версию отозвали —
// DownloadUpdate её не ставит.
func TestDownloadRefusesRevokedTarget(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0084", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0083", Platforms: withBuild()},
	}})
	_, err := s.DownloadUpdate("v0.00.000.0084")
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("ожидали отказ из-за отзыва, получили %v", err)
	}
}

// Предложенную 0084 отозвали, latest снова 0083: подсказка без «новее»
// всё равно обновляет latest для Настроек.
func TestAuthOKLatestChangedRefreshesSettings(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083", &Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0084", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0083", Platforms: withBuild()},
	}})
	s.latestRelease = &network.LatestReleaseInfo{Version: "v0.00.000.0084"}
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0083", Revoked: []string{"v0.00.000.0084"},
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if lr := s.GetLatestRelease(); lr != nil && lr.Version == "v0.00.000.0083" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetLatestRelease: %+v", s.GetLatestRelease())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Подсказка, пришедшая во время идущей проверки, не теряется: проверка
// делает ещё один проход.
func TestAuthOKHintDuringCheckRechecked(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(m *Manifest) ([]byte, string) {
		body, _ := json.Marshal(m)
		sum := sha256.Sum256(body)
		return body, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sum[:]))
	}
	oldBody, oldSig := sign(&Manifest{Latest: "v0.00.000.0083", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Platforms: withBuild()},
	}})
	newBody, newSig := sign(&Manifest{Latest: "v0.00.000.0082", Releases: []ManifestReleaseEntry{
		{Version: "v0.00.000.0083", Revoked: true, Platforms: withBuild()},
		{Version: "v0.00.000.0082", Platforms: withBuild()},
	}})
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release // первая проверка висит, пока не придёт вторая подсказка
			w.Header().Set("X-Signature", oldSig)
			w.Write(oldBody)
			return
		}
		w.Header().Set("X-Signature", newSig)
		w.Write(newBody)
	}))
	t.Cleanup(srv.Close)

	s := NewUpdateService(slog.New(slog.NewTextHandler(io.Discard, nil)), "v0.00.000.0083",
		base64.StdEncoding.EncodeToString(pub), func() string { return srv.URL })
	s.retryDelays = nil
	got := make(chan map[string]any, 4)
	s.SetEmitter(func(name string, data any) {
		if name == "vox:update-available" {
			got <- data.(map[string]any)
		}
	})
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{Version: "v0.00.000.0084"})
	for calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	close(release)
	if ev := expectEvent(t, got); ev["version"] != "v0.00.000.0082" || ev["revoked"] != true {
		t.Fatalf("ожидался откат до 0082, получено %v", ev)
	}
}
