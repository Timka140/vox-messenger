package e2e

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/nacl/box"
)

// Групповое сообщение через сервер. Сервер хранит payload непрозрачной
// строкой; здесь он такой:
//
//		"\x1evox2:" + JSON{v, n, c, k, s}
//
//	  - K — случайный ключ сообщения (32 байта);
//	  - c = XChaCha20-Poly1305(K, n, plaintext, aad = group_id, client_msg_id, author);
//	  - k — K, обёрнутый NaCl box для каждого получателя (включая себя):
//	    prefix(8 байт ключа получателя) || nonce(24) || box(K);
//	  - s — подпись автора Ed25519 над group_id, client_msg_id, author, n, c.
//
// Получатель без своей обёртки — сообщение отправлено до его вступления
// (ErrNotForMe). Автора удостоверяет подпись: author_key от сервера проверяется
// ключом, которым подписано сообщение.
const (
	GroupPrefix   = "\x1evox2:"
	groupVersion  = 2
	groupSigLabel = "vox-grp-v2"
	keyPrefixLen  = 8
	wrapLen       = keyPrefixLen + 24 + chacha20poly1305.KeySize + box.Overhead
)

var (
	// ErrNotForMe — среди получателей нас нет (сообщение старше вступления).
	ErrNotForMe = errors.New("e2e: message not encrypted for this member")
	// ErrBadMessage — формат, подпись или расшифровка не сошлись.
	ErrBadMessage = errors.New("e2e: bad group message")
)

type groupWire struct {
	V int      `json:"v"`
	N string   `json:"n"`
	C string   `json:"c"`
	K []string `json:"k"`
	S string   `json:"s"`
}

var b64 = base64.RawStdEncoding

// IsGroupPayload — payload в формате vox2.
func IsGroupPayload(payload string) bool { return strings.HasPrefix(payload, GroupPrefix) }

func groupAAD(groupID, msgID string, author ed25519.PublicKey) []byte {
	var b bytes.Buffer
	for _, f := range [][]byte{[]byte(groupID), []byte(msgID), author} {
		_ = binary.Write(&b, binary.BigEndian, uint16(len(f)))
		b.Write(f)
	}
	return b.Bytes()
}

func groupSigned(groupID, msgID string, author ed25519.PublicKey, nonce, ct []byte) []byte {
	var b bytes.Buffer
	b.WriteString(groupSigLabel)
	b.WriteByte(0)
	for _, f := range [][]byte{[]byte(groupID), []byte(msgID), author, nonce, ct} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(f)))
		b.Write(f)
	}
	return b.Bytes()
}

// SealGroup шифрует plaintext для recipients (ключи Ed25519 участников;
// себя включать — чтобы прочитать своё сообщение с другого устройства или
// после синхронизации). Получатели с битым ключом пропускаются.
func SealGroup(author ed25519.PrivateKey, recipients []ed25519.PublicKey, groupID, msgID string, plaintext []byte) (string, error) {
	authorPub := author.Public().(ed25519.PublicKey)
	authorX, err := PrivateToX25519(author)
	if err != nil {
		return "", err
	}
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nil, nonce, plaintext, groupAAD(groupID, msgID, authorPub))

	w := groupWire{V: groupVersion, N: b64.EncodeToString(nonce), C: b64.EncodeToString(ct)}
	seen := map[string]bool{}
	for _, r := range recipients {
		if len(r) != ed25519.PublicKeySize || seen[string(r)] {
			continue
		}
		seen[string(r)] = true
		rx, err := PublicToX25519(r)
		if err != nil {
			continue
		}
		var wn [24]byte
		if _, err := rand.Read(wn[:]); err != nil {
			return "", err
		}
		wrap := make([]byte, 0, wrapLen)
		wrap = append(wrap, r[:keyPrefixLen]...)
		wrap = append(wrap, wn[:]...)
		wrap = box.Seal(wrap, key, &wn, rx, authorX)
		w.K = append(w.K, b64.EncodeToString(wrap))
	}
	if len(w.K) == 0 {
		return "", fmt.Errorf("e2e: нет получателей")
	}
	w.S = b64.EncodeToString(ed25519.Sign(author, groupSigned(groupID, msgID, authorPub, nonce, ct)))
	raw, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return GroupPrefix + string(raw), nil
}

// OpenGroup расшифровывает payload vox2 от автора author для участника me.
func OpenGroup(me ed25519.PrivateKey, author ed25519.PublicKey, groupID, msgID, payload string) ([]byte, error) {
	if !IsGroupPayload(payload) || len(author) != ed25519.PublicKeySize {
		return nil, ErrBadMessage
	}
	var w groupWire
	if err := json.Unmarshal([]byte(payload[len(GroupPrefix):]), &w); err != nil || w.V != groupVersion {
		return nil, ErrBadMessage
	}
	nonce, err1 := b64.DecodeString(w.N)
	ct, err2 := b64.DecodeString(w.C)
	sig, err3 := b64.DecodeString(w.S)
	if err1 != nil || err2 != nil || err3 != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return nil, ErrBadMessage
	}
	if !ed25519.Verify(author, groupSigned(groupID, msgID, author, nonce, ct), sig) {
		return nil, fmt.Errorf("%w: подпись автора", ErrBadMessage)
	}
	mePub := me.Public().(ed25519.PublicKey)
	meX, err := PrivateToX25519(me)
	if err != nil {
		return nil, err
	}
	authorX, err := PublicToX25519(author)
	if err != nil {
		return nil, ErrBadMessage
	}
	for _, ks := range w.K {
		wrap, err := b64.DecodeString(ks)
		if err != nil || len(wrap) != wrapLen || !bytes.Equal(wrap[:keyPrefixLen], mePub[:keyPrefixLen]) {
			continue
		}
		var wn [24]byte
		copy(wn[:], wrap[keyPrefixLen:keyPrefixLen+24])
		key, ok := box.Open(nil, wrap[keyPrefixLen+24:], &wn, authorX, meX)
		if !ok {
			continue // совпал только префикс
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, ErrBadMessage
		}
		pt, err := aead.Open(nil, nonce, ct, groupAAD(groupID, msgID, author))
		if err != nil {
			return nil, fmt.Errorf("%w: расшифровка", ErrBadMessage)
		}
		return pt, nil
	}
	return nil, ErrNotForMe
}
