package network

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// Подпись SDP ключом профиля. Offer/answer идут через сигнальный сервер, а
// DTLS-отпечаток в SDP — единственное, что связывает P2P-соединение с
// собеседником. Без подписи сервер (или посредник до него) мог подменить
// отпечаток и встать посередине. Подписывается SHA-256 всего SDP (отпечаток,
// ice-ufrag, кандидаты), тип, pc_id, отправитель, получатель и время.
//
// Подпись лежит внутри payload (vox_ts, vox_sig): поля верхнего уровня
// Message сервер пересобирает и неизвестные отбрасывает.
const (
	sdpSignLabel = "vox-sdp-v1"
	// sdpMaxSkew — допуск по времени подписи: часы собеседников и очередь
	// сигналки для офлайн-адресата.
	sdpMaxSkew = 10 * time.Minute
)

// ErrSDPSignature — offer/answer без верной подписи собеседника.
var ErrSDPSignature = errors.New("SDP signature invalid")

// SetSigner задаёт ключ профиля для подписи offer/answer.
func (pm *PeerManager) SetSigner(priv ed25519.PrivateKey) {
	pm.mu.Lock()
	pm.signKey = priv
	pm.mu.Unlock()
}

func sdpSignedBytes(from, to ed25519.PublicKey, pcId int, sdType, sdp string, tsMs int64) []byte {
	sum := sha256.Sum256([]byte(sdp))
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(tsMs))
	return authMessage(sdpSignLabel, from, to, []byte{byte(pcId)}, []byte(sdType), sum[:], ts[:])
}

// signedSDP — payload offer/answer: SessionDescription + pc_id + подпись.
func (pm *PeerManager) signedSDP(sd webrtc.SessionDescription, to string, pcId int) (json.RawMessage, error) {
	pm.mu.RLock()
	key := pm.signKey
	pm.mu.RUnlock()
	toPub, err := hex.DecodeString(to)
	if err != nil || len(toPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signedSDP: invalid peer key")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signedSDP: нет ключа подписи")
	}
	ts := time.Now().UnixMilli()
	sig := ed25519.Sign(key, sdpSignedBytes(key.Public().(ed25519.PublicKey), toPub, pcId, sd.Type.String(), sd.SDP, ts))
	m := map[string]any{"type": sd.Type.String(), "sdp": sd.SDP, "vox_ts": ts, "vox_sig": hex.EncodeToString(sig)}
	if pcId != 0 {
		m["pc_id"] = pcId // дубль Message.PcId (см. injectPcID)
	}
	return json.Marshal(m)
}

// verifySDP разбирает payload offer/answer и проверяет подпись отправителя
// from (hex ключа профиля). Ошибка — payload отвергается целиком.
func (pm *PeerManager) verifySDP(from string, payload json.RawMessage, pcId int) (webrtc.SessionDescription, error) {
	var p struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
		TS   int64  `json:"vox_ts"`
		Sig  string `json:"vox_sig"`
	}
	var sd webrtc.SessionDescription
	if err := json.Unmarshal(payload, &p); err != nil {
		return sd, fmt.Errorf("%w: %v", ErrSDPSignature, err)
	}
	fromPub, err := hex.DecodeString(from)
	sig, err2 := hex.DecodeString(p.Sig)
	if err != nil || err2 != nil || len(fromPub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return sd, fmt.Errorf("%w: нет подписи", ErrSDPSignature)
	}
	if d := time.Since(time.UnixMilli(p.TS)); d > sdpMaxSkew || d < -sdpMaxSkew {
		return sd, fmt.Errorf("%w: время подписи вне допуска (%s)", ErrSDPSignature, d.Round(time.Second))
	}
	me, err := hex.DecodeString(pm.myPubKey)
	if err != nil || len(me) != ed25519.PublicKeySize {
		return sd, fmt.Errorf("%w: свой ключ", ErrSDPSignature)
	}
	if !ed25519.Verify(ed25519.PublicKey(fromPub), sdpSignedBytes(fromPub, me, pcId, p.Type, p.SDP, p.TS), sig) {
		return sd, ErrSDPSignature
	}
	if n := countFingerprints(p.SDP); n != 1 {
		return sd, fmt.Errorf("%w: в SDP %d разных DTLS-отпечатков", ErrSDPSignature, n)
	}
	sd.Type = webrtc.NewSDPType(p.Type)
	sd.SDP = p.SDP
	return sd, nil
}

// countFingerprints — сколько разных a=fingerprint в SDP. pion сверяет
// сертификат с одним отпечатком; несколько разных — не наш случай.
func countFingerprints(sdp string) int {
	seen := map[string]bool{}
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if f := strings.Fields(strings.TrimPrefix(line, "a=fingerprint:")); strings.HasPrefix(line, "a=fingerprint:") && len(f) >= 2 {
			seen[strings.ToLower(f[0])+" "+strings.ToUpper(f[1])] = true
		}
	}
	return len(seen)
}
