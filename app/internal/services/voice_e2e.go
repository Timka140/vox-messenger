package services

import (
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"vox_app/internal/e2e"
)

// Сквозное шифрование голоса через SFU. SFU пересылает кадры как есть и
// видел бы Opus открытым; теперь каждый кадр зашифрован ключом отправителя.
//
//   - У каждого участника канала свой ключ отправителя (32 байта) и номер
//     эпохи. Ключ уходит собеседникам по P2P (voice_key, NaCl box ключами
//     профилей — DataChannel к тому же защищён DTLS с подписанным SDP).
//   - Собеседник вошёл в канал — получает текущий ключ. Ушёл — все создают
//     новый ключ и рассылают оставшимся: ушедший новые кадры не прочтёт.
//   - Кадр в SFU: VOXS(8) || epoch(4) || counter(4) || ChaCha20-Poly1305(VOX1+opus),
//     nonce = sender_hash || epoch || counter, aad = VOXS-заголовок.
//   - Нет ключа или не сошёлся тег — кадр отбрасывается, ключ запрашивается
//     (voice_key_req). Открытый кадр в SFU не уходит никогда.
//
// Голос P2P-mesh идёт по DataChannel (DTLS) и сюда не относится.
const (
	voiceKeyMsg    = "voice_key"
	voiceKeyReqMsg = "voice_key_req"
	voiceHdrLen    = 8 // VOXS magic + sender_hash
	voiceE2EHdr    = 8 // epoch + counter
	// voiceKeyReqEvery — не чаще одного запроса ключа к пиру.
	voiceKeyReqEvery = 2 * time.Second
	// voicePeerEpochs — сколько эпох собеседника держим: кадры старой эпохи
	// ещё летят, пока новый ключ расходится.
	voicePeerEpochs = 2
)

type voicePeerKey struct {
	epoch uint32
	aead  cipher.AEAD
}

type voiceE2E struct {
	mu      sync.Mutex
	epoch   uint32
	key     []byte
	aead    cipher.AEAD
	counter uint32
	peers   map[string][]voicePeerKey // peerID → последние эпохи, новые в конце
	lastReq map[string]time.Time
}

func newVoiceE2E() *voiceE2E {
	return &voiceE2E{peers: map[string][]voicePeerKey{}, lastReq: map[string]time.Time{}}
}

// rotate — новый ключ отправителя (вход в канал, уход собеседника).
func (v *voiceE2E) rotate() {
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		return
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return
	}
	v.mu.Lock()
	v.epoch++
	v.key, v.aead, v.counter = key, aead, 0
	v.mu.Unlock()
}

// ensure — ключ отправителя есть (вход в канал).
func (v *voiceE2E) ensure() {
	v.mu.Lock()
	has := v.aead != nil
	v.mu.Unlock()
	if !has {
		v.rotate()
	}
}

// reset — выход из канала: ключи всех сторон забываются.
func (v *voiceE2E) reset() {
	v.mu.Lock()
	v.key, v.aead, v.counter = nil, nil, 0
	v.peers = map[string][]voicePeerKey{}
	v.lastReq = map[string]time.Time{}
	v.mu.Unlock()
}

func (v *voiceE2E) current() (uint32, []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.epoch, append([]byte(nil), v.key...)
}

func voiceNonce(senderHash, epoch, counter uint32) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.LittleEndian.PutUint32(n[0:4], senderHash)
	binary.BigEndian.PutUint32(n[4:8], epoch)
	binary.BigEndian.PutUint32(n[8:12], counter)
	return n
}

// seal шифрует кадр VOX1+opus; hdr — VOXS-заголовок (aad). nil — ключа нет.
func (v *voiceE2E) seal(hdr []byte, senderHash uint32, frame []byte) []byte {
	v.mu.Lock()
	aead, epoch := v.aead, v.epoch
	if aead == nil || v.counter == ^uint32(0) {
		v.mu.Unlock()
		return nil
	}
	v.counter++
	counter := v.counter
	v.mu.Unlock()
	out := make([]byte, voiceE2EHdr, voiceE2EHdr+len(frame)+aead.Overhead())
	binary.BigEndian.PutUint32(out[0:4], epoch)
	binary.BigEndian.PutUint32(out[4:8], counter)
	return aead.Seal(out, voiceNonce(senderHash, epoch, counter), frame, hdr)
}

// open расшифровывает кадр собеседника. needKey — ключа этой эпохи нет.
func (v *voiceE2E) open(peerID string, hdr []byte, senderHash uint32, body []byte) (frame []byte, needKey bool) {
	if len(body) < voiceE2EHdr+chacha20poly1305.Overhead {
		return nil, false
	}
	epoch := binary.BigEndian.Uint32(body[0:4])
	counter := binary.BigEndian.Uint32(body[4:8])
	v.mu.Lock()
	var aead cipher.AEAD
	for _, k := range v.peers[peerID] {
		if k.epoch == epoch {
			aead = k.aead
		}
	}
	v.mu.Unlock()
	if aead == nil {
		return nil, true
	}
	pt, err := aead.Open(nil, voiceNonce(senderHash, epoch, counter), body[voiceE2EHdr:], hdr)
	if err != nil {
		return nil, false
	}
	return pt, false
}

func (v *voiceE2E) setPeer(peerID string, epoch uint32, key []byte) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	keys := v.peers[peerID]
	for i, k := range keys {
		if k.epoch == epoch {
			keys[i].aead = aead
			return
		}
	}
	keys = append(keys, voicePeerKey{epoch: epoch, aead: aead})
	if len(keys) > voicePeerEpochs {
		keys = keys[len(keys)-voicePeerEpochs:]
	}
	v.peers[peerID] = keys
}

func (v *voiceE2E) dropPeer(peerID string) {
	v.mu.Lock()
	delete(v.peers, peerID)
	delete(v.lastReq, peerID)
	v.mu.Unlock()
}

// shouldRequest — пора ли снова просить ключ у пира.
func (v *voiceE2E) shouldRequest(peerID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if time.Since(v.lastReq[peerID]) < voiceKeyReqEvery {
		return false
	}
	v.lastReq[peerID] = time.Now()
	return true
}

// voiceKeyPlain — содержимое voice_key: epoch || key || group_id (чтобы ключ
// одного канала нельзя было выдать за ключ другого).
func voiceKeyPlain(epoch uint32, key []byte, groupID string) []byte {
	out := binary.BigEndian.AppendUint32(nil, epoch)
	out = append(out, key...)
	return append(out, groupID...)
}

// sendVoiceKeyLocked — свой текущий ключ пиру peerID (под s.mu).
func (s *VoiceService) sendVoiceKeyLocked(peerID, groupID string) {
	epoch, key := s.vkeys.current()
	if len(key) == 0 || groupID == "" {
		return
	}
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return
	}
	to, err := hex.DecodeString(peerID)
	if err != nil || len(to) != ed25519.PublicKeySize {
		return
	}
	blob, err := e2e.SealTo(priv, to, voiceKeyPlain(epoch, key, groupID))
	if err != nil {
		return
	}
	data, _ := json.Marshal(P2PMessage{Type: voiceKeyMsg, GroupID: groupID, Content: base64.StdEncoding.EncodeToString(blob)})
	if err := s.network.SendP2PData(peerID, data); err != nil {
		s.log.Debug("voice_key не отправлен", slog.String("peer", safeShort(peerID)), slog.String("error", err.Error()))
	}
}

// rotateVoiceKeyLocked — новый ключ и рассылка участникам канала (под s.mu).
func (s *VoiceService) rotateVoiceKeyLocked() {
	s.vkeys.rotate()
	for pid := range s.participants {
		s.sendVoiceKeyLocked(pid, s.activeGroupID)
	}
}

// handleVoiceKeyLocked — входящие voice_key / voice_key_req (под s.mu).
func (s *VoiceService) handleVoiceKeyLocked(fromPubKey string, msg *P2PMessage) {
	if msg.GroupID == "" || msg.GroupID != s.activeGroupID {
		return
	}
	if msg.Type == voiceKeyReqMsg {
		s.sendVoiceKeyLocked(fromPubKey, msg.GroupID)
		return
	}
	blob, err := base64.StdEncoding.DecodeString(msg.Content)
	if err != nil {
		return
	}
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return
	}
	from, err := hex.DecodeString(fromPubKey)
	if err != nil || len(from) != ed25519.PublicKeySize {
		return
	}
	plain, err := e2e.OpenFrom(priv, from, blob)
	if err != nil || len(plain) != 4+chacha20poly1305.KeySize+len(msg.GroupID) || string(plain[4+chacha20poly1305.KeySize:]) != msg.GroupID {
		s.log.Warn("voice_key отвергнут", slog.String("peer", safeShort(fromPubKey)))
		return
	}
	s.vkeys.setPeer(fromPubKey, binary.BigEndian.Uint32(plain[0:4]), plain[4:4+chacha20poly1305.KeySize])
	s.log.Debug("voice_key принят", slog.String("peer", safeShort(fromPubKey)), slog.Uint64("epoch", uint64(binary.BigEndian.Uint32(plain[0:4]))))
}

// requestVoiceKey — попросить ключ у пира, чьи кадры не расшифровываются.
func (s *VoiceService) requestVoiceKey(peerID string) {
	if !s.vkeys.shouldRequest(peerID) {
		return
	}
	s.mu.Lock()
	groupID := s.activeGroupID
	s.mu.Unlock()
	if groupID == "" {
		return
	}
	data, _ := json.Marshal(P2PMessage{Type: voiceKeyReqMsg, GroupID: groupID})
	_ = s.network.SendP2PData(peerID, data)
}
