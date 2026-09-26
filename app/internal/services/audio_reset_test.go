package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func newAudioTestService(t *testing.T) *AudioService {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "audio.db"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sess := session.New()
	if err := sess.Set(priv, pub); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewAudioService(log, sess, store, nil, nil, nil)
}

func TestResetAudioSettings(t *testing.T) {
	a := newAudioTestService(t)
	if err := a.SetMicrophone(3); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSpeaker(2); err != nil {
		t.Fatal(err)
	}
	a.SetMicGain(2.5)
	if err := a.SetNoiseSuppression(0); err != nil {
		t.Fatal(err)
	}
	if err := a.SetDFNStrength(40); err != nil {
		t.Fatal(err)
	}

	if err := a.ResetAudioSettings(); err != nil {
		t.Fatal(err)
	}

	if mic, _ := a.GetSelectedMicrophone(); mic != "" {
		t.Errorf("microphone = %q, want системный (пусто)", mic)
	}
	if spk, _ := a.GetSelectedSpeaker(); spk != "" {
		t.Errorf("speaker = %q, want системный (пусто)", spk)
	}
	if g, _ := a.GetMicGain(); g != 1.0 {
		t.Errorf("mic gain = %v, want 1", g)
	}
	ns, _ := a.GetNoiseSuppression()
	if ns.Level != defaultNSLevel() {
		t.Errorf("ns level = %d, want %d", ns.Level, defaultNSLevel())
	}
	if ns.DFNStrength != dfnStrengthDefault {
		t.Errorf("dfn strength = %d, want %d", ns.DFNStrength, dfnStrengthDefault)
	}
	if a.getSpeakerIndex() != -1 {
		t.Errorf("speaker index = %d, want -1", a.getSpeakerIndex())
	}
}

// Без идущей проверки «Слушать себя» только запоминается.
func TestMicMonitorWithoutTest(t *testing.T) {
	a := newAudioTestService(t)
	if err := a.SetMicMonitor(true); err != nil {
		t.Fatal(err)
	}
	if !a.monitorOn || a.monPB != nil {
		t.Fatalf("monitorOn=%v monPB=%v, want запомнен без playback", a.monitorOn, a.monPB)
	}
	if err := a.SetMicMonitor(false); err != nil {
		t.Fatal(err)
	}
	if a.monitorOn {
		t.Fatal("monitorOn после выключения")
	}
}
