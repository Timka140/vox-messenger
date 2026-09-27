package services

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"

	"vox_app/internal/network"
)

func newRevokedTestService(t *testing.T, current string) (*UpdateService, *[]map[string]any) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := NewUpdateService(slog.New(slog.NewTextHandler(io.Discard, nil)), current,
		base64.StdEncoding.EncodeToString(pub), func() string { return "" })
	var got []map[string]any
	s.SetEmitter(func(name string, data any) {
		if name == "vox:update-available" {
			got = append(got, data.(map[string]any))
		}
	})
	return s, &got
}

// Откат из админки: клиент на отозванной версии получает latest, даже если он старше.
func TestAuthOKRevokedCurrentOffersDowngrade(t *testing.T) {
	s, got := newRevokedTestService(t, "v0.00.000.0083")
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0083"},
	})
	if len(*got) != 1 || (*got)[0]["version"] != "v0.00.000.0082" || (*got)[0]["force"] != true {
		t.Fatalf("ожидалось принудительное обновление до 0082, получено %v", *got)
	}
}

func TestAuthOKOlderNotRevokedIgnored(t *testing.T) {
	s, got := newRevokedTestService(t, "v0.00.000.0083")
	s.OnLatestReleaseFromAuthOK(&network.LatestReleaseInfo{
		Version: "v0.00.000.0082", Revoked: []string{"v0.00.000.0081"},
	})
	if len(*got) != 0 {
		t.Fatalf("старшая версия без отзыва не предлагается: %v", *got)
	}
}

func TestManifestRevokedCurrent(t *testing.T) {
	s, _ := newRevokedTestService(t, "v0.00.000.0083")
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
