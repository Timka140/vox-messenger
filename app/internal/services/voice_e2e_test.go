package services

import (
	"bytes"
	"testing"
)

func TestVoiceE2EFrames(t *testing.T) {
	alice, bob := newVoiceE2E(), newVoiceE2E()
	const aliceID, aliceHash = "alice", uint32(0xA11CE)
	frame := []byte("VOX1\x01\x00opus-frame")

	hdr := wrapVOXS(nil, aliceHash)
	if body := alice.seal(hdr, aliceHash, frame); body != nil {
		t.Fatal("без ключа кадр не должен шифроваться (и уходить)")
	}
	alice.ensure()
	body := alice.seal(hdr, aliceHash, frame)
	if bytes.Contains(body, []byte("opus-frame")) {
		t.Fatal("кадр открытым текстом")
	}
	if got, need := bob.open(aliceID, hdr, aliceHash, body); got != nil || !need {
		t.Fatal("без ключа Алисы Боб должен запросить ключ")
	}

	epoch, key := alice.current()
	bob.setPeer(aliceID, epoch, key)
	got, _ := bob.open(aliceID, hdr, aliceHash, body)
	if !bytes.Equal(got, frame) {
		t.Fatalf("расшифровка: %q", got)
	}

	// Подделка отправителя: тот же кадр под чужим sender_hash не открывается.
	if got, _ := bob.open(aliceID, wrapVOXS(nil, 0xBAD), 0xBAD, body); got != nil {
		t.Fatal("кадр с подменённым sender_hash принят")
	}
	// Порча шифротекста.
	bad := append([]byte(nil), body...)
	bad[len(bad)-1] ^= 1
	if got, _ := bob.open(aliceID, hdr, aliceHash, bad); got != nil {
		t.Fatal("испорченный кадр принят")
	}

	// Смена ключа (кто-то ушёл): старый ключ новые кадры не открывает.
	alice.rotate()
	body2 := alice.seal(hdr, aliceHash, frame)
	if got, need := bob.open(aliceID, hdr, aliceHash, body2); got != nil || !need {
		t.Fatal("кадр новой эпохи без нового ключа")
	}
	e2, k2 := alice.current()
	bob.setPeer(aliceID, e2, k2)
	if got, _ := bob.open(aliceID, hdr, aliceHash, body2); !bytes.Equal(got, frame) {
		t.Fatal("новая эпоха не открылась")
	}
	// Предыдущая эпоха ещё открывается (кадры в пути).
	if got, _ := bob.open(aliceID, hdr, aliceHash, body); !bytes.Equal(got, frame) {
		t.Fatal("кадр прошлой эпохи")
	}

	alice.reset()
	if alice.seal(hdr, aliceHash, frame) != nil {
		t.Fatal("после выхода из канала кадры не шифруются")
	}
}

func TestVoiceKeyPlainBindsGroup(t *testing.T) {
	p := voiceKeyPlain(7, bytes.Repeat([]byte{1}, 32), "group-1")
	if len(p) != 4+32+len("group-1") || string(p[36:]) != "group-1" {
		t.Fatalf("формат voice_key: %x", p)
	}
}
