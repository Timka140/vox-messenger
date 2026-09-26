package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"messenger_signal/internal/admin"
	"messenger_signal/internal/config"
	"messenger_signal/internal/hub"
	"messenger_signal/internal/identity"
	"messenger_signal/internal/releases"
)

const adminTestToken = "adm-0123456789abcdef0123456789abcdef"

func newAdminServer(t *testing.T, token string, relPub ed25519.PublicKey) (http.Handler, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := New(hub.New(log), config.WSConfig{Path: "/ws"}, config.TURNConfig{}, config.SFUConfig{}, log,
		&identity.Identity{Public: pub, Private: priv})

	dir := t.TempDir()
	// На диске — только v0.00.000.0001 и v0.00.000.0002.
	for _, v := range []string{"v0.00.000.0001", "v0.00.000.0002"} {
		p := filepath.Join(dir, "windows-amd64", v, "models")
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "windows-amd64", v, "vox.exe"), []byte("exe"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "m.bin"), []byte("model"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":2,"latest":"old","releases":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json.sig"), []byte("OLDSIG\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srv.SetReleasesStore(releases.NewStore(dir))
	srv.SetAdmin(token, relPub)
	return srv.Handler(), dir
}

func testManifest(latest string, versions ...string) []byte {
	m := releases.Manifest{Version: releases.ManifestFormatVersion, Latest: latest}
	for _, v := range versions {
		m.Releases = append(m.Releases, releases.ReleaseEntry{
			Version:    v,
			ReleasedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			Platforms: map[string]releases.PlatformBundle{"windows-amd64": {
				Entry: "vox.exe",
				Files: []releases.ManifestFile{{Path: "vox.exe", Size: 3}, {Path: "models/m.bin", Size: 5}},
			}},
		})
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return b
}

func signedReq(h http.Handler, method, uri string, body []byte) *httptest.ResponseRecorder {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	r := httptest.NewRequest(method, uri, bytes.NewReader(body))
	r.Header.Set("X-NW-Ts", ts)
	r.Header.Set("X-NW-Sig", admin.Sign(adminTestToken, method, r.URL.RequestURI(), ts, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func signManifest(priv ed25519.PrivateKey, m []byte) string {
	sum := sha256.Sum256(m)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sum[:]))
}

func TestAdminReleasesPut(t *testing.T) {
	relPub, relPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)

	good := testManifest("v0.00.000.0002", "v0.00.000.0002", "v0.00.000.0001")
	cases := []struct {
		name     string
		manifest []byte
		sig      func(m []byte) string
		want     int
		wantErr  string
	}{
		{"good", good, func(m []byte) string { return signManifest(relPriv, m) }, 200, ""},
		{"rollback to existing", testManifest("v0.00.000.0001", "v0.00.000.0001"), func(m []byte) string { return signManifest(relPriv, m) }, 200, ""},
		{"wrong key", good, func(m []byte) string { return signManifest(otherPriv, m) }, 400, "signature"},
		{"sig over other bytes", good, func(m []byte) string { return signManifest(relPriv, append([]byte(" "), m...)) }, 400, "signature"},
		{"sig over raw not sha", good, func(m []byte) string {
			return base64.StdEncoding.EncodeToString(ed25519.Sign(relPriv, m))
		}, 400, "signature"},
		{"sig not base64", good, func(m []byte) string { return "!!!" }, 400, "signature"},
		{"latest not in releases", testManifest("v0.00.000.0009", "v0.00.000.0002"), func(m []byte) string { return signManifest(relPriv, m) }, 400, ""},
		{"missing files", testManifest("v0.00.000.0003", "v0.00.000.0003", "v0.00.000.0002"), func(m []byte) string { return signManifest(relPriv, m) }, 400, ""},
		{"not json", []byte("nope"), func(m []byte) string { return signManifest(relPriv, m) }, 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, dir := newAdminServer(t, adminTestToken, relPub)
			body, _ := json.Marshal(map[string]string{"manifest": string(c.manifest), "signature": c.sig(c.manifest)})
			w := signedReq(h, "PUT", "/admin/v1/releases", body)
			if w.Code != c.want {
				t.Fatalf("code %d, want %d: %s", w.Code, c.want, w.Body)
			}
			var resp map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &resp)

			// Что отдаёт /api/update/manifest.
			mr := httptest.NewRecorder()
			h.ServeHTTP(mr, httptest.NewRequest("GET", "/api/update/manifest", nil))
			if c.want != 200 {
				if c.wantErr != "" && resp["error"] != c.wantErr {
					t.Fatalf("error %v, want %q", resp["error"], c.wantErr)
				}
				if resp["error"] == nil || resp["error"] == "" {
					t.Fatalf("нет текста ошибки: %s", w.Body)
				}
				if mr.Header().Get("X-Signature") != "OLDSIG" {
					t.Fatalf("манифест изменился после отказа: %q", mr.Header().Get("X-Signature"))
				}
				return
			}
			if resp["ok"] != true {
				t.Fatalf("resp %v", resp)
			}
			if !bytes.Equal(mr.Body.Bytes(), c.manifest) || mr.Header().Get("X-Signature") != c.sig(c.manifest) {
				t.Fatalf("/api/update/manifest отдаёт старое: %s", mr.Body)
			}
			disk, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if !bytes.Equal(disk, c.manifest) {
				t.Fatal("manifest.json на диске не заменён")
			}
			left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
			if len(left) != 0 {
				t.Fatalf("остались временные файлы %v", left)
			}
			// GET /admin/v1/releases — те же байты.
			g := signedReq(h, "GET", "/admin/v1/releases", nil)
			var gr struct{ Manifest, Signature string }
			_ = json.Unmarshal(g.Body.Bytes(), &gr)
			if gr.Manifest != string(c.manifest) || gr.Signature != c.sig(c.manifest) {
				t.Fatalf("GET releases %s", g.Body)
			}
		})
	}
}

func TestAdminReleasesNoPubKey(t *testing.T) {
	h, _ := newAdminServer(t, adminTestToken, nil)
	body, _ := json.Marshal(map[string]string{"manifest": "{}", "signature": ""})
	if w := signedReq(h, "PUT", "/admin/v1/releases", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code %d", w.Code)
	}
}

func TestAdminDisabled(t *testing.T) {
	for _, tok := range []string{"", "short-token"} {
		h, _ := newAdminServer(t, tok, nil)
		w := signedReq(h, "GET", "/admin/v1/status", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("token %q: code %d", tok, w.Code)
		}
	}
	h, _ := newAdminServer(t, adminTestToken, nil)
	w := signedReq(h, "GET", "/admin/v1/status", nil)
	if w.Code != 200 {
		t.Fatalf("enabled: code %d %s", w.Code, w.Body)
	}
	var st map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st["releases"] == nil || st["clients_online"] != 0.0 || st["cluster"].(map[string]any)["enabled"] != false {
		t.Fatalf("status %v", st)
	}
}
