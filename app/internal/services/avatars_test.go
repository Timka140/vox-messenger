package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func testAvatarJPEG(t *testing.T, side, quality int, noise bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	var px [3]byte
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			c := color.RGBA{uint8(x), uint8(y), 128, 255}
			if noise {
				_, _ = rand.Read(px[:])
				c = color.RGBA{px[0], px[1], px[2], 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestValidateAvatarJPEG(t *testing.T) {
	if err := validateAvatarJPEG(testAvatarJPEG(t, 256, 80, false)); err != nil {
		t.Fatalf("валидное фото отклонено: %v", err)
	}
	if err := validateAvatarJPEG(testAvatarJPEG(t, 128, 80, false)); err == nil {
		t.Fatal("128×128 принято")
	}
	big := testAvatarJPEG(t, 256, 100, true)
	if len(big) <= maxAvatarBytes {
		t.Fatalf("фикстура шума слишком мала: %d байт", len(big))
	}
	if err := validateAvatarJPEG(big); err == nil {
		t.Fatal("фото больше 40 КБ принято")
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 256, 256))); err != nil {
		t.Fatal(err)
	}
	if err := validateAvatarJPEG(pngBuf.Bytes()); err == nil {
		t.Fatal("PNG принят")
	}
	if err := validateAvatarJPEG(nil); err == nil {
		t.Fatal("пустое фото принято")
	}
}

// Фото уходит одним DC.Send без нарезки: сообщение целиком должно быть
// меньше SCTP max message size (65536 у pion).
func TestAvatarMessagesFitOneSCTPMessage(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(make([]byte, maxAvatarBytes))
	uuid := "0f8fad5b-d9cb-469f-a165-70867728950e"
	for _, m := range []P2PMessage{
		{Type: "avatar_data", Content: strings.Repeat("f", 64), Data: b64, Time: 1789999999},
		{Type: "group_avatar", ID: uuid, GroupID: uuid, Content: b64, Time: 1789999999},
	} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) >= 65536 {
			t.Fatalf("%s: %d байт не влезает в одно сообщение SCTP", m.Type, len(b))
		}
	}
}

func newAvatarPeer(t *testing.T, name string) (*AvatarService, string) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), name+".db"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.New()
	if err := sess.Set(priv, pub); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	network := NewNetworkService(log, sess, store)
	groups := NewGroupsService(log, sess, store, network)
	id, _ := sess.PeerID()
	return NewAvatarService(log, sess, store, network, groups), id
}

// TestAvatarExchange — hash → get → data между двумя пирами с отдельными БД;
// повторный обмен тем же хешем не гоняет фото заново; удаление доходит.
func TestAvatarExchange(t *testing.T) {
	alice, aliceID := newAvatarPeer(t, "alice")
	bob, bobID := newAvatarPeer(t, "bob")

	sent := map[string]int{}
	link := func(from *AvatarService, fromID string, to *AvatarService) {
		from.network.SetTestSender(func(_ string, data []byte) error {
			var m P2PMessage
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("json: %v", err)
			}
			sent[m.Type]++
			to.handleP2P(fromID, &m)
			return nil
		})
	}
	link(alice, aliceID, bob)
	link(bob, bobID, alice)

	photo := testAvatarJPEG(t, 256, 85, false)
	hash, err := alice.SetMyAvatar(base64.StdEncoding.EncodeToString(photo))
	if err != nil {
		t.Fatalf("SetMyAvatar: %v", err)
	}

	alice.syncToPeer(bobID) // DataChannel открылся
	gotHash, gotData := bob.profileAvatar(aliceID)
	if gotHash != hash || !bytes.Equal(gotData, photo) {
		t.Fatalf("у Боба фото Алисы не то: hash=%q (want %q), %d байт", gotHash, hash, len(gotData))
	}
	if sent["avatar_get"] != 1 || sent["avatar_data"] != 1 {
		t.Fatalf("ожидался один запрос и один ответ, было %v", sent)
	}

	alice.syncToPeer(bobID) // переподключение: хеш тот же
	if sent["avatar_get"] != 1 {
		t.Fatalf("фото запрошено повторно при том же хеше: %v", sent)
	}

	if err := alice.ClearMyAvatar(); err != nil {
		t.Fatalf("ClearMyAvatar: %v", err)
	}
	alice.syncToPeer(bobID)
	if h, data := bob.profileAvatar(aliceID); h != "" || len(data) != 0 {
		t.Fatalf("удаление не дошло: hash=%q, %d байт", h, len(data))
	}

	hashes, err := bob.GetAvatarHashes()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hashes[aliceID]; ok {
		t.Fatal("в GetAvatarHashes остался удалённый хеш")
	}
}
