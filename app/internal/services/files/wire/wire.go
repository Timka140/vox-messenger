// Package wire — бинарный протокол передачи файлов поверх DataChannel
// "attachments". Все фреймы big-endian, type-byte дискриминатор.
//
// Frame types:
//
//	0x10 FILE_MANIFEST  — sender → receiver: «вот описание файла»
//	0x11 CHUNK_REQUEST  — receiver → sender: «пришли chunks с N по N+count»
//	0x12 CHUNK_DATA     — sender → receiver: «вот один chunk»
//	0x13 CHUNK_NAK      — sender → receiver: «у меня нет этого chunk'а»
//	0x14 FILE_DECLINE   — receiver → sender: «не принимаю этот файл»
//
// Старые типы 0x01 (CHUNK) и 0x02 (COMPLETE) для legacy attachments-flow
// остаются в attachments_p2p_binary.go — они обрабатываются messaging-сервисом
// до диспатча в FilesService. Новые 0x10+ — диспатч в FilesService.
package wire

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Frame type discriminators. 0x00..0x0F зарезервированы за legacy attachments
// (см. services.attachFrameTypeChunk = 0x01).
const (
	TypeFileManifest byte = 0x10
	TypeChunkRequest byte = 0x11
	TypeChunkData    byte = 0x12
	TypeChunkNak     byte = 0x13
	TypeFileDecline  byte = 0x14
)

// MsgIDLen — UUID raw length для msgID-полей.
const MsgIDLen = 16

// FileShaLen — sha256 raw length для file_sha и chunk_sha.
const FileShaLen = 32

// FrameType извлекает type-байт из frame. Безопасно для пустого frame
// (вернёт 0). Используется диспатчером перед вызовом конкретного Decode*.
func FrameType(frame []byte) byte {
	if len(frame) == 0 {
		return 0
	}
	return frame[0]
}

// ──── 0x10 FILE_MANIFEST ─────────────────────────────────────────────────
//
// Layout:
//
//	[ 1 ] type = 0x10
//	[16 ] msgID         (UUID raw)
//	[16 ] groupID       (UUID raw, all-zero для DM)
//	[ 4 ] payloadLen    (uint32 BE)
//	[ N ] payloadJSON   (FileManifestPayload)
//
// payloadJSON содержит manifest списка файлов, имена, kind, caption — всё
// что нужно receiver'у чтобы создать message_files и начать pull.

// FileManifestPayload — JSON-тело file_manifest фрейма.
// SenderID заполняется receiver'ом из fromPeer на уровне dispatch (в JSON
// его нет — экономия; reply-path однозначный).
type FileManifestPayload struct {
	MsgID    string              `json:"msgId"`
	GroupID  string              `json:"groupId,omitempty"`
	Caption  string              `json:"caption,omitempty"`
	Files    []FileManifestEntry `json:"files"`
	SenderID string              `json:"-"` // не сериализуется; receiver ставит fromPeer
	// ForwardedFrom — «Переслано от»; старые клиенты поле игнорируют.
	ForwardedFrom *ForwardRef `json:"forwardedFrom,omitempty"`
}

// ForwardRef — автор исходного сообщения при пересылке. JSON совпадает с
// services.ForwardRef (колонка forwarded_from).
type ForwardRef struct {
	Name string `json:"name"`
}

// FileManifestEntry — описание одного файла в составе message.
type FileManifestEntry struct {
	Sha        string   `json:"sha"`
	Size       int64    `json:"size"`
	ChunkSize  int      `json:"chunkSize"`
	ChunkCount int      `json:"chunkCount"`
	Chunks     []string `json:"chunks"`
	Mime       string   `json:"mime,omitempty"`
	Width      int      `json:"width,omitempty"`
	Height     int      `json:"height,omitempty"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // "photo" | "file"
}

// EncodeFileManifest сериализует payload в frame.
func EncodeFileManifest(p *FileManifestPayload) ([]byte, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("EncodeFileManifest: marshal: %w", err)
	}
	frame := make([]byte, 1+MsgIDLen+MsgIDLen+4+len(body))
	frame[0] = TypeFileManifest
	if err := writeUUID(frame[1:1+MsgIDLen], p.MsgID); err != nil {
		return nil, fmt.Errorf("EncodeFileManifest: msgID: %w", err)
	}
	if p.GroupID != "" {
		if err := writeUUID(frame[1+MsgIDLen:1+2*MsgIDLen], p.GroupID); err != nil {
			return nil, fmt.Errorf("EncodeFileManifest: groupID: %w", err)
		}
	}
	binary.BigEndian.PutUint32(frame[1+2*MsgIDLen:1+2*MsgIDLen+4], uint32(len(body)))
	copy(frame[1+2*MsgIDLen+4:], body)
	return frame, nil
}

// DecodeFileManifest парсит frame обратно.
func DecodeFileManifest(frame []byte) (*FileManifestPayload, error) {
	const headerLen = 1 + MsgIDLen + MsgIDLen + 4
	if len(frame) < headerLen {
		return nil, fmt.Errorf("DecodeFileManifest: short frame")
	}
	if frame[0] != TypeFileManifest {
		return nil, fmt.Errorf("DecodeFileManifest: bad type 0x%02x", frame[0])
	}
	bodyLen := binary.BigEndian.Uint32(frame[1+2*MsgIDLen : headerLen])
	if int(bodyLen) > len(frame)-headerLen {
		return nil, fmt.Errorf("DecodeFileManifest: body truncated")
	}
	var p FileManifestPayload
	if err := json.Unmarshal(frame[headerLen:headerLen+int(bodyLen)], &p); err != nil {
		return nil, fmt.Errorf("DecodeFileManifest: unmarshal: %w", err)
	}
	return &p, nil
}

// ──── 0x11 CHUNK_REQUEST ─────────────────────────────────────────────────
//
// Receiver просит у sender'а chunks N..N+count-1 из файла с file_sha.
// Один request → до count CHUNK_DATA в ответ (или CHUNK_NAK для каждого
// отсутствующего).
//
// Layout:
//
//	[ 1 ] type = 0x11
//	[32 ] file_sha
//	[ 4 ] chunk_idx   (uint32 BE — стартовый)
//	[ 2 ] count       (uint16 BE — сколько chunks подряд)

// EncodeChunkRequest пакует запрос.
func EncodeChunkRequest(fileSha string, chunkIdx int, count int) ([]byte, error) {
	frame := make([]byte, 1+FileShaLen+4+2)
	frame[0] = TypeChunkRequest
	if err := writeSha(frame[1:1+FileShaLen], fileSha); err != nil {
		return nil, fmt.Errorf("EncodeChunkRequest: file_sha: %w", err)
	}
	if chunkIdx < 0 {
		return nil, fmt.Errorf("EncodeChunkRequest: negative chunkIdx")
	}
	if count <= 0 || count > 0xFFFF {
		return nil, fmt.Errorf("EncodeChunkRequest: bad count %d", count)
	}
	binary.BigEndian.PutUint32(frame[1+FileShaLen:], uint32(chunkIdx))
	binary.BigEndian.PutUint16(frame[1+FileShaLen+4:], uint16(count))
	return frame, nil
}

// DecodeChunkRequest разбирает запрос.
func DecodeChunkRequest(frame []byte) (fileSha string, chunkIdx, count int, err error) {
	want := 1 + FileShaLen + 4 + 2
	if len(frame) < want {
		err = fmt.Errorf("DecodeChunkRequest: short frame")
		return
	}
	if frame[0] != TypeChunkRequest {
		err = fmt.Errorf("DecodeChunkRequest: bad type 0x%02x", frame[0])
		return
	}
	fileSha = hex.EncodeToString(frame[1 : 1+FileShaLen])
	chunkIdx = int(binary.BigEndian.Uint32(frame[1+FileShaLen:]))
	count = int(binary.BigEndian.Uint16(frame[1+FileShaLen+4:]))
	return
}

// ──── 0x12 CHUNK_DATA ────────────────────────────────────────────────────
//
// Один chunk в ответ на CHUNK_REQUEST.
//
// Layout:
//
//	[ 1 ] type = 0x12
//	[32 ] file_sha
//	[ 4 ] chunk_idx
//	[ N ] payload (raw plaintext, sha256(payload) должна == file_chunks[idx].chunk_sha)
//
// Receiver verify'ит sha. Если mismatch — chunk отклоняется, request заново.

func EncodeChunkData(fileSha string, chunkIdx int, payload []byte) ([]byte, error) {
	frame := make([]byte, 1+FileShaLen+4+len(payload))
	frame[0] = TypeChunkData
	if err := writeSha(frame[1:1+FileShaLen], fileSha); err != nil {
		return nil, fmt.Errorf("EncodeChunkData: file_sha: %w", err)
	}
	if chunkIdx < 0 {
		return nil, fmt.Errorf("EncodeChunkData: negative chunkIdx")
	}
	binary.BigEndian.PutUint32(frame[1+FileShaLen:1+FileShaLen+4], uint32(chunkIdx))
	copy(frame[1+FileShaLen+4:], payload)
	return frame, nil
}

func DecodeChunkData(frame []byte) (fileSha string, chunkIdx int, payload []byte, err error) {
	headerLen := 1 + FileShaLen + 4
	if len(frame) < headerLen {
		err = fmt.Errorf("DecodeChunkData: short frame")
		return
	}
	if frame[0] != TypeChunkData {
		err = fmt.Errorf("DecodeChunkData: bad type 0x%02x", frame[0])
		return
	}
	fileSha = hex.EncodeToString(frame[1 : 1+FileShaLen])
	chunkIdx = int(binary.BigEndian.Uint32(frame[1+FileShaLen : headerLen]))
	payload = frame[headerLen:]
	return
}

// ──── 0x13 CHUNK_NAK ─────────────────────────────────────────────────────
//
// Sender отвечает «нет такого chunk'а» (например файл удалён локально).
//
// Layout:
//
//	[ 1 ] type = 0x13
//	[32 ] file_sha
//	[ 4 ] chunk_idx
//	[ 1 ] reason  (0=not_found, 1=forbidden, 2=removed)

const (
	NakNotFound  byte = 0
	NakForbidden byte = 1
	NakRemoved   byte = 2
)

func EncodeChunkNak(fileSha string, chunkIdx int, reason byte) ([]byte, error) {
	frame := make([]byte, 1+FileShaLen+4+1)
	frame[0] = TypeChunkNak
	if err := writeSha(frame[1:1+FileShaLen], fileSha); err != nil {
		return nil, fmt.Errorf("EncodeChunkNak: file_sha: %w", err)
	}
	binary.BigEndian.PutUint32(frame[1+FileShaLen:1+FileShaLen+4], uint32(chunkIdx))
	frame[1+FileShaLen+4] = reason
	return frame, nil
}

func DecodeChunkNak(frame []byte) (fileSha string, chunkIdx int, reason byte, err error) {
	want := 1 + FileShaLen + 4 + 1
	if len(frame) < want {
		err = fmt.Errorf("DecodeChunkNak: short frame")
		return
	}
	if frame[0] != TypeChunkNak {
		err = fmt.Errorf("DecodeChunkNak: bad type 0x%02x", frame[0])
		return
	}
	fileSha = hex.EncodeToString(frame[1 : 1+FileShaLen])
	chunkIdx = int(binary.BigEndian.Uint32(frame[1+FileShaLen : 1+FileShaLen+4]))
	reason = frame[1+FileShaLen+4]
	return
}

// ──── 0x14 FILE_DECLINE ──────────────────────────────────────────────────
//
// Receiver сообщает sender'у что отклонил файл (юзер нажал «отклонить» в
// approval-модалке). Sender очищает свой state и не ждёт chunk_request.
//
// Layout:
//
//	[ 1 ] type = 0x14
//	[16 ] msgID

func EncodeFileDecline(msgID string) ([]byte, error) {
	frame := make([]byte, 1+MsgIDLen)
	frame[0] = TypeFileDecline
	if err := writeUUID(frame[1:], msgID); err != nil {
		return nil, fmt.Errorf("EncodeFileDecline: %w", err)
	}
	return frame, nil
}

func DecodeFileDecline(frame []byte) (string, error) {
	if len(frame) < 1+MsgIDLen {
		return "", fmt.Errorf("DecodeFileDecline: short frame")
	}
	if frame[0] != TypeFileDecline {
		return "", fmt.Errorf("DecodeFileDecline: bad type 0x%02x", frame[0])
	}
	return readUUID(frame[1 : 1+MsgIDLen]), nil
}

// ──── helpers ────────────────────────────────────────────────────────────

// writeUUID парсит UUID-строку и пишет 16 raw байт в dst.
// Пустая строка → нули (для groupID=DM).
func writeUUID(dst []byte, s string) error {
	if len(dst) != MsgIDLen {
		return fmt.Errorf("writeUUID: dst len %d", len(dst))
	}
	if s == "" {
		for i := range dst {
			dst[i] = 0
		}
		return nil
	}
	// UUID v4 hex string: 36 символов с дефисами или 32 без.
	hexStr := stripDashes(s)
	if len(hexStr) != 32 {
		return fmt.Errorf("writeUUID: bad uuid %q", s)
	}
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return fmt.Errorf("writeUUID: %w", err)
	}
	copy(dst, raw)
	return nil
}

// readUUID превращает 16 raw байт в UUID-строку с дефисами (стандарт RFC 4122).
// Если все нули — возвращает пустую строку (наша конвенция для DM).
func readUUID(src []byte) string {
	allZero := true
	for _, b := range src {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return ""
	}
	h := hex.EncodeToString(src)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// writeSha парсит hex-sha256 (64 char) и пишет 32 raw байта в dst.
func writeSha(dst []byte, hex64 string) error {
	if len(dst) != FileShaLen {
		return fmt.Errorf("writeSha: dst len %d", len(dst))
	}
	if len(hex64) != 64 {
		return fmt.Errorf("writeSha: bad sha %q", hex64)
	}
	raw, err := hex.DecodeString(hex64)
	if err != nil {
		return fmt.Errorf("writeSha: %w", err)
	}
	copy(dst, raw)
	return nil
}

func stripDashes(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
