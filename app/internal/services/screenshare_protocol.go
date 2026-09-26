package services

import (
	"encoding/binary"
	"fmt"
)

// Screen DC wire format. Каждое сообщение в "screen" DataChannel имеет
// 20-байтный заголовок и payload-чанк закодированного кадра.
//
// Layout (LE):
//   [0:4]   magic "SCRN"
//   [4]     version (u8) = screenWireVersion
//   [5]     flags (u8): bit 0 = keyframe, bit 1 = last_chunk_of_frame
//   [6]     codec (u8): 0 = JPEG, 1 = H264 (NALU)
//   [7]     reserved (u8) = 0
//   [8:12]  frameID (u32) — монотонный счётчик отправителя, нужен для сборки
//                          и определения "потерял I-кадр, ждать следующего"
//   [12:14] chunkIdx (u16) — индекс этого chunk в пределах кадра
//   [14:16] chunkTotal (u16) — всего chunks в кадре (>=1)
//   [16:18] width (u16) — натуральная ширина кадра
//   [18:20] height (u16) — натуральная высота
//   [20:]   payload — сегмент закодированного кадра (≤ screenWireChunkSize)
//
// pion v4 declares a=max-message-size:65535 в SDP, поэтому DC.Send спокойно
// пропускает 60-килобайтные сообщения — SCTP фрагментирует под капотом, ресивер
// собирает обратно. 60 KiB вместо 16 KiB = 4× меньше Send'ов на JPEG-кадр
// (1080p quality=80 ≈ 250 KiB → 4 chunks vs 16), что ощутимо снижает overhead
// сериализации SCTP/DTLS и backpressure-проверок.
const (
	screenWireMagic       = "SCRN"
	screenWireVersion     = 1
	screenWireHeaderSize  = 20
	screenWireChunkSize   = 60 * 1024
	screenWirePayloadSize = screenWireChunkSize - screenWireHeaderSize
)

// Codec идентификаторы. Совпадают с numerics в JS-декодере.
const (
	screenCodecJPEG = 0
	screenCodecH264 = 1
)

// Flags
const (
	screenFlagKeyframe  = 1 << 0
	screenFlagLastChunk = 1 << 1
	// screenFlagControl — не кадр, а управляющее сообщение зритель → отправитель.
	// payload = [op u8]; frameID/chunk*/width/height = 0. Идёт по тому же DC
	// в обратную сторону (DataChannel двунаправленный).
	screenFlagControl = 1 << 2
)

// Control ops.
const (
	// screenCtrlPLI — Picture Loss Indication: зритель просит keyframe
	// (старт с середины потока, ошибка декодера, reconnect).
	screenCtrlPLI = 1
	// screenCtrlStop — отправитель остановил демонстрацию: зритель убирает
	// тайл сразу, не дожидаясь таймаута неактивности.
	screenCtrlStop = 2
)

// encodeScreenControl — control-фрейм с одним op.
func encodeScreenControl(op byte) []byte {
	buf := make([]byte, screenWireHeaderSize+1)
	copy(buf[0:4], screenWireMagic)
	buf[4] = screenWireVersion
	buf[5] = screenFlagControl
	binary.LittleEndian.PutUint16(buf[14:16], 1) // chunkTotal
	buf[screenWireHeaderSize] = op
	return buf
}

// decodeScreenControl — (op, true), если data — control-фрейм.
func decodeScreenControl(data []byte) (byte, bool) {
	h, payload, err := decodeScreenHeader(data)
	if err != nil || h.Flags&screenFlagControl == 0 || len(payload) < 1 {
		return 0, false
	}
	return payload[0], true
}

// screenFrameHeader — распакованное представление 20-байтного заголовка.
type screenFrameHeader struct {
	Version    uint8
	Flags      uint8
	Codec      uint8
	FrameID    uint32
	ChunkIdx   uint16
	ChunkTotal uint16
	Width      uint16
	Height     uint16
}

// IsKeyframe — true если bit 0 во flags выставлен.
func (h screenFrameHeader) IsKeyframe() bool { return h.Flags&screenFlagKeyframe != 0 }

// IsLastChunk — true если этот chunk последний в кадре (для UI/латентности).
func (h screenFrameHeader) IsLastChunk() bool { return h.Flags&screenFlagLastChunk != 0 }

// encodeScreenChunks нарезает закодированный кадр на сообщения для DC.
// В каждом chunk header повторяется одинаковый, отличается только chunkIdx.
// last chunk помечается screenFlagLastChunk; keyframe-флаг ставится на ВСЕ chunks
// I-кадра, чтобы потеря середины не превратила кадр в "non-keyframe" на receiver.
func encodeScreenChunks(payload []byte, h screenFrameHeader) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	total := (len(payload) + screenWirePayloadSize - 1) / screenWirePayloadSize
	if total > 0xFFFF {
		// На 1080p H264 I-frame это ≈ 1 GiB кадр — в реальности недостижимо.
		// Защита от переполнения u16 chunkTotal.
		total = 0xFFFF
	}
	h.ChunkTotal = uint16(total)
	out := make([][]byte, 0, total)
	for i := range total {
		start := i * screenWirePayloadSize
		end := start + screenWirePayloadSize
		if end > len(payload) {
			end = len(payload)
		}
		seg := payload[start:end]

		buf := make([]byte, screenWireHeaderSize+len(seg))
		copy(buf[0:4], screenWireMagic)
		buf[4] = screenWireVersion
		buf[5] = h.Flags
		if i == total-1 {
			buf[5] |= screenFlagLastChunk
		}
		buf[6] = h.Codec
		buf[7] = 0
		binary.LittleEndian.PutUint32(buf[8:12], h.FrameID)
		binary.LittleEndian.PutUint16(buf[12:14], uint16(i))
		binary.LittleEndian.PutUint16(buf[14:16], h.ChunkTotal)
		binary.LittleEndian.PutUint16(buf[16:18], h.Width)
		binary.LittleEndian.PutUint16(buf[18:20], h.Height)
		copy(buf[screenWireHeaderSize:], seg)
		out = append(out, buf)
	}
	return out
}

// decodeScreenHeader разбирает 20-байтный заголовок. Возвращает payload-slice
// (zero-copy от исходного buf'а) и заголовок. Ошибка — если magic/version не
// совпадают или buf слишком короткий.
func decodeScreenHeader(buf []byte) (screenFrameHeader, []byte, error) {
	if len(buf) < screenWireHeaderSize {
		return screenFrameHeader{}, nil, fmt.Errorf("screen frame too short: %d", len(buf))
	}
	if string(buf[0:4]) != screenWireMagic {
		return screenFrameHeader{}, nil, fmt.Errorf("screen frame bad magic")
	}
	if buf[4] != screenWireVersion {
		return screenFrameHeader{}, nil, fmt.Errorf("screen frame unsupported version: %d", buf[4])
	}
	h := screenFrameHeader{
		Version:    buf[4],
		Flags:      buf[5],
		Codec:      buf[6],
		FrameID:    binary.LittleEndian.Uint32(buf[8:12]),
		ChunkIdx:   binary.LittleEndian.Uint16(buf[12:14]),
		ChunkTotal: binary.LittleEndian.Uint16(buf[14:16]),
		Width:      binary.LittleEndian.Uint16(buf[16:18]),
		Height:     binary.LittleEndian.Uint16(buf[18:20]),
	}
	if h.ChunkTotal == 0 {
		return h, nil, fmt.Errorf("screen frame chunkTotal=0")
	}
	if h.ChunkIdx >= h.ChunkTotal {
		return h, nil, fmt.Errorf("screen frame chunkIdx %d >= total %d", h.ChunkIdx, h.ChunkTotal)
	}
	return h, buf[screenWireHeaderSize:], nil
}
