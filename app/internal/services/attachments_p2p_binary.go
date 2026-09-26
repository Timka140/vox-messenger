package services

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// attachments_p2p_binary.go — бинарный wire-формат для chunks большого
// attachment'а. Идёт через DataChannel "attachments" (см. peermgr.go), а не
// "messages" — освобождает текстовый канал и убирает base64-overhead.
//
// CHUNK-фрейм (type=0x01, Big-Endian):
//
//	0       1        type      0x01 = CHUNK
//	1      16        msgID     raw 16-байтный UUID
//	17      2        attIdx    uint16 (max 65535 файлов в одном msg)
//	19      4        chunkIdx  uint32
//	23      4        total     uint32 (chunks for this att)
//	27      N        payload   raw bytes (≤ ~256 KiB per chunk)
//
// COMPLETE-фрейм (type=0x02):
//
//	0       1        type      0x02 = COMPLETE
//	1      16        msgID     raw 16-байтный UUID
//	17      2        attIdx    uint16
//	19     32        sha256    raw 32 байта (sha256 целого att-файла)
//
// Complete ОБЯЗАН идти через тот же DC что chunks — SCTP ordered гарантирует
// что все предыдущие chunks доставятся раньше complete. Если шлём через
// "messages" DC, он обгоняет chunks (разные SCTP-стримы) — receiver вызывает
// Finalize() до того как последние chunks записались, sha сходится с пустотой
// (e3b0c442... = sha256("")).
const (
	attachFrameTypeChunk    byte = 0x01
	attachFrameTypeComplete byte = 0x02
	attachFrameHeaderLen         = 27
	attachCompleteFrameLen       = 51 // 1 + 16 + 2 + 32
)

// encodeAttachChunk пакует chunk в бинарный фрейм для отправки через
// "attachments" DC. payload включается прямо в фрейм (один write — одно
// SCTP-сообщение).
func encodeAttachChunk(msgID string, attIdx, chunkIdx, total int, payload []byte) ([]byte, error) {
	id, err := uuid.Parse(msgID)
	if err != nil {
		return nil, fmt.Errorf("encodeAttachChunk: bad msgID: %w", err)
	}
	if attIdx < 0 || attIdx > 0xFFFF {
		return nil, fmt.Errorf("encodeAttachChunk: attIdx out of range: %d", attIdx)
	}
	if chunkIdx < 0 || total < 0 {
		return nil, fmt.Errorf("encodeAttachChunk: negative chunkIdx/total")
	}
	frame := make([]byte, attachFrameHeaderLen+len(payload))
	frame[0] = attachFrameTypeChunk
	copy(frame[1:17], id[:])
	binary.BigEndian.PutUint16(frame[17:19], uint16(attIdx))
	binary.BigEndian.PutUint32(frame[19:23], uint32(chunkIdx))
	binary.BigEndian.PutUint32(frame[23:27], uint32(total))
	copy(frame[attachFrameHeaderLen:], payload)
	return frame, nil
}

// decodeAttachChunk разбирает входящий фрейм. Возвращает msgID (uuid string),
// attIdx, chunkIdx, total и срез payload (живёт пока valid frame buffer).
func decodeAttachChunk(frame []byte) (msgID string, attIdx, chunkIdx, total int, payload []byte, err error) {
	if len(frame) < attachFrameHeaderLen {
		err = fmt.Errorf("decodeAttachChunk: short frame (%d < %d)", len(frame), attachFrameHeaderLen)
		return
	}
	if frame[0] != attachFrameTypeChunk {
		err = fmt.Errorf("decodeAttachChunk: unknown type 0x%02x", frame[0])
		return
	}
	var id uuid.UUID
	copy(id[:], frame[1:17])
	msgID = id.String()
	attIdx = int(binary.BigEndian.Uint16(frame[17:19]))
	chunkIdx = int(binary.BigEndian.Uint32(frame[19:23]))
	total = int(binary.BigEndian.Uint32(frame[23:27]))
	payload = frame[attachFrameHeaderLen:]
	return
}

// encodeAttachComplete пакует attachment_complete в бинарный фрейм. Должен
// идти через тот же DC что chunks — гарантирует что все chunks дойдут раньше.
func encodeAttachComplete(msgID string, attIdx int, sha256Hex string) ([]byte, error) {
	id, err := uuid.Parse(msgID)
	if err != nil {
		return nil, fmt.Errorf("encodeAttachComplete: bad msgID: %w", err)
	}
	if attIdx < 0 || attIdx > 0xFFFF {
		return nil, fmt.Errorf("encodeAttachComplete: attIdx out of range: %d", attIdx)
	}
	if len(sha256Hex) != 64 {
		return nil, fmt.Errorf("encodeAttachComplete: bad sha256 hex len %d", len(sha256Hex))
	}
	frame := make([]byte, attachCompleteFrameLen)
	frame[0] = attachFrameTypeComplete
	copy(frame[1:17], id[:])
	binary.BigEndian.PutUint16(frame[17:19], uint16(attIdx))
	if _, err := hex.Decode(frame[19:51], []byte(sha256Hex)); err != nil {
		return nil, fmt.Errorf("encodeAttachComplete: bad sha256 hex: %w", err)
	}
	return frame, nil
}

// HandleIncomingAttachBinary — публичный entry-point для legacy-фреймов
// (type 0x01 chunk, 0x02 complete). Вызывается из роутера в main.go ПОСЛЕ
// того как files.HandleFrame вернул false (не CAS-фрейм).
//
// Диспатчит по type-байту: chunk → handleAttachmentChunkBytes,
// complete → handleAttachmentComplete (через синтетический P2PMessage).
func (s *MessagingService) HandleIncomingAttachBinary(fromPubKey string, frame []byte) {
	if len(frame) < 1 {
		return
	}
	switch frame[0] {
	case attachFrameTypeChunk:
		msgID, attIdx, _, _, payload, err := decodeAttachChunk(frame)
		if err != nil {
			s.log.Warn("attach binary chunk decode",
				slog.String("peer", safeShort(fromPubKey)),
				slog.String("err", err.Error()))
			return
		}
		// Копия payload — после возврата из callback'а DataChannel может
		// переиспользовать буфер фрейма; writer асинхронно flush'ит на диск.
		chunk := make([]byte, len(payload))
		copy(chunk, payload)
		s.handleAttachmentChunkBytes(fromPubKey, msgID, attIdx, chunk)
	case attachFrameTypeComplete:
		if len(frame) < attachCompleteFrameLen {
			s.log.Warn("attach binary complete: short frame",
				slog.String("peer", safeShort(fromPubKey)),
				slog.Int("len", len(frame)))
			return
		}
		var id uuid.UUID
		copy(id[:], frame[1:17])
		attIdx := int(binary.BigEndian.Uint16(frame[17:19]))
		shaHex := hex.EncodeToString(frame[19:51])
		// Переиспользуем legacy-обработчик через синтетический P2PMessage.
		msg := &P2PMessage{
			Type: "attachment_complete",
			ID:   id.String(),
			Att:  attIdx,
			Sha:  shaHex,
		}
		s.handleAttachmentComplete(fromPubKey, msg)
	default:
		s.log.Warn("attach binary frame: unknown type",
			slog.String("peer", safeShort(fromPubKey)),
			slog.Int("type", int(frame[0])))
	}
}
