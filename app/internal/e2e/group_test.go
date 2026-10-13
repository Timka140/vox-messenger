package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func keys(t *testing.T, n int) ([]ed25519.PrivateKey, []ed25519.PublicKey) {
	t.Helper()
	var privs []ed25519.PrivateKey
	var pubs []ed25519.PublicKey
	for i := 0; i < n; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		privs, pubs = append(privs, priv), append(pubs, pub)
	}
	return privs, pubs
}

func TestGroupRoundTripAndOutsider(t *testing.T) {
	privs, pubs := keys(t, 4) // 0 — автор, 1,2 — участники, 3 — вступил позже
	msg := []byte("привет, группа")
	payload, err := SealGroup(privs[0], pubs[:3], "g1", "m1", msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "привет") {
		t.Fatal("открытый текст в payload")
	}
	for i := 0; i < 3; i++ {
		got, err := OpenGroup(privs[i], pubs[0], "g1", "m1", payload)
		if err != nil || string(got) != string(msg) {
			t.Fatalf("участник %d: %v %q", i, err, got)
		}
	}
	if _, err := OpenGroup(privs[3], pubs[0], "g1", "m1", payload); !errors.Is(err, ErrNotForMe) {
		t.Fatalf("не-получатель: %v", err)
	}
}

func TestGroupRejectsForgery(t *testing.T) {
	privs, pubs := keys(t, 3)
	payload, _ := SealGroup(privs[0], pubs, "g1", "m1", []byte("x"))
	// Сервер выдаёт сообщение за другого автора.
	if _, err := OpenGroup(privs[1], pubs[2], "g1", "m1", payload); !errors.Is(err, ErrBadMessage) {
		t.Fatalf("чужой автор: %v", err)
	}
	// Перенос в другую группу или под другой id.
	if _, err := OpenGroup(privs[1], pubs[0], "g2", "m1", payload); !errors.Is(err, ErrBadMessage) {
		t.Fatalf("другая группа: %v", err)
	}
	if _, err := OpenGroup(privs[1], pubs[0], "g1", "m2", payload); !errors.Is(err, ErrBadMessage) {
		t.Fatalf("другой id: %v", err)
	}
	// Порча шифротекста.
	bad := strings.Replace(payload, `"c":"`, `"c":"A`, 1)
	if _, err := OpenGroup(privs[1], pubs[0], "g1", "m1", bad); err == nil {
		t.Fatal("испорченный payload принят")
	}
	if _, err := OpenGroup(privs[1], pubs[0], "g1", "m1", "plain text"); !errors.Is(err, ErrBadMessage) {
		t.Fatalf("открытый текст: %v", err)
	}
}

// 100 участников (предел группы) укладываются в 32 КБ payload relay.
func TestGroupSizeAt100(t *testing.T) {
	privs, pubs := keys(t, 100)
	payload, err := SealGroup(privs[0], pubs, "550e8400-e29b-41d4-a716-446655440000", "m", make([]byte, 4000))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > 32*1024 {
		t.Fatalf("payload %d байт > 32 КБ", len(payload))
	}
	t.Logf("100 участников, 4000 байт текста: %d байт", len(payload))
}
