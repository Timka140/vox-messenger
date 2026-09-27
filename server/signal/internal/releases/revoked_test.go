package releases

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Отзыв версии: latest отозвать нельзя, список отозванных уходит в auth_ok.
func TestRevoked(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Version: ManifestFormatVersion,
		Latest:  "v0.00.000.0081",
		Releases: []ReleaseEntry{
			{Version: "v0.00.000.0082", Revoked: true},
			{Version: "v0.00.000.0081"},
		},
	}
	writeManifest(t, dir, m)
	s := NewStore(dir)

	info := s.LatestReleaseInfo()
	if info == nil || info.Version != "v0.00.000.0081" {
		t.Fatalf("latest: %+v", info)
	}
	if !reflect.DeepEqual(info.Revoked, []string{"v0.00.000.0082"}) {
		t.Fatalf("revoked: %v", info.Revoked)
	}

	bad := *m
	bad.Latest = "v0.00.000.0082"
	raw, _ := json.Marshal(&bad)
	if _, err := s.Validate(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("отозванная latest должна отклоняться, err=%v", err)
	}

	ok, _ := json.Marshal(m)
	if _, err := s.Validate(ok); err != nil {
		t.Fatalf("отзыв не-latest допустим: %v", err)
	}
}
