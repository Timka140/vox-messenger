package services

import "crypto/sha256"

// Wire-протоколы голосового канала.
//
// VOX1 — обязательная обёртка opus-фрейма (release-сборка).
// DFNP — debug-обёртка для замера wire-latency (только voiceDebug build).
// VOXS — SFU обёртка: sender identification для SFU forwarding.
//
// Все форматы самоидентифицируются магиком в первых 4 байтах. Отсутствие
// магика трактуется как legacy-пакет (raw opus без обёртки) — обработка
// фолбэчится на неё.

// --- VOX1: sequence numbering для FEC/PLC recovery ---
//
// Формат: [4B magic "VOX1"] [2B seq LE uint16] [opus payload]
// Overhead: 6 байт. Receiver без VOX1-поддержки увидит магик как часть opus
// payload, opus_decode вернёт ошибку — фрейм молча дропается.
var voxvMagic = [4]byte{'V', 'O', 'X', '1'}

func wrapVOX1(opusData []byte, seq uint16) []byte {
	out := make([]byte, 6+len(opusData))
	copy(out[0:4], voxvMagic[:])
	out[4] = byte(seq)
	out[5] = byte(seq >> 8)
	copy(out[6:], opusData)
	return out
}

// unwrapVOX1 возвращает (opusPayload, seq, ok). ok=false — это legacy пакет
// без магика, вызывающий код должен использовать fallback путь.
func unwrapVOX1(pkt []byte) ([]byte, uint16, bool) {
	if len(pkt) < 6 {
		return pkt, 0, false
	}
	if pkt[0] != voxvMagic[0] || pkt[1] != voxvMagic[1] || pkt[2] != voxvMagic[2] || pkt[3] != voxvMagic[3] {
		return pkt, 0, false
	}
	seq := uint16(pkt[4]) | uint16(pkt[5])<<8
	return pkt[6:], seq, true
}

// --- DFNP: wire-latency probe (debug-only обёртка) ---
//
// Формат: [4B magic "DFNP"] [8B int64 LE монотонный ns send timestamp] [opus...]
// Receiver стрипает header и декодирует opus как обычно. Отсутствие магика =
// legacy-пакет, обрабатывается без измерения. DFNP не переименован (не бренд
// — это DeepFilterNet Probe).
var dfnpMagic = [4]byte{'D', 'F', 'N', 'P'}

func wrapDFNP(opusData []byte, nowNs int64) []byte {
	out := make([]byte, 12+len(opusData))
	copy(out[0:4], dfnpMagic[:])
	// little-endian int64
	for i := 0; i < 8; i++ {
		out[4+i] = byte(nowNs >> (i * 8))
	}
	copy(out[12:], opusData)
	return out
}

// unwrapDFNP возвращает (payload, sendNs, hasHeader).
func unwrapDFNP(pkt []byte) ([]byte, int64, bool) {
	if len(pkt) < 12 {
		return pkt, 0, false
	}
	if pkt[0] != dfnpMagic[0] || pkt[1] != dfnpMagic[1] || pkt[2] != dfnpMagic[2] || pkt[3] != dfnpMagic[3] {
		return pkt, 0, false
	}
	var sendNs int64
	for i := 0; i < 8; i++ {
		sendNs |= int64(pkt[4+i]) << (i * 8)
	}
	return pkt[12:], sendNs, true
}

// --- VOXS: SFU sender identification ---
//
// Формат: [4B magic "VOXS"] [4B sender_hash LE uint32] [VOX1+opus payload]
// Overhead: 8 байт. sender_hash — первые 4 байта SHA-256(peerID), позволяет
// получателю идентифицировать отправителя без передачи полного peerID в каждом
// фрейме. SFU пересылает фрейм as-is, не декодируя payload.
var voxsMagic = [4]byte{'V', 'O', 'X', 'S'}

// peerIDHash возвращает первые 4 байта SHA-256(peerID) как uint32 LE.
func peerIDHash(peerID string) uint32 {
	h := sha256.Sum256([]byte(peerID))
	return uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16 | uint32(h[3])<<24
}

func wrapVOXS(payload []byte, senderHash uint32) []byte {
	out := make([]byte, 8+len(payload))
	copy(out[0:4], voxsMagic[:])
	out[4] = byte(senderHash)
	out[5] = byte(senderHash >> 8)
	out[6] = byte(senderHash >> 16)
	out[7] = byte(senderHash >> 24)
	copy(out[8:], payload)
	return out
}

// unwrapVOXS возвращает (payload, senderHash, ok).
func unwrapVOXS(pkt []byte) ([]byte, uint32, bool) {
	if len(pkt) < 8 {
		return pkt, 0, false
	}
	if pkt[0] != voxsMagic[0] || pkt[1] != voxsMagic[1] || pkt[2] != voxsMagic[2] || pkt[3] != voxsMagic[3] {
		return pkt, 0, false
	}
	hash := uint32(pkt[4]) | uint32(pkt[5])<<8 | uint32(pkt[6])<<16 | uint32(pkt[7])<<24
	return pkt[8:], hash, true
}
