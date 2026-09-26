package services

import (
	"bytes"
	"io"
	"log/slog"
	"testing"
)

func newTestScreenSvc() *ScreenShareService {
	return &ScreenShareService{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		rxBuf: make(map[string]*screenReassembly),
	}
}

// buildScreenFrame делает chunks для кадра с детерминированным payload.
func buildScreenFrame(frameID uint32, payloadLen int, keyframe bool) ([][]byte, []byte) {
	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = byte((int(frameID)*7 + i) % 251)
	}
	var flags uint8
	if keyframe {
		flags |= screenFlagKeyframe
	}
	h := screenFrameHeader{
		Version: screenWireVersion,
		Flags:   flags,
		Codec:   screenCodecJPEG,
		FrameID: frameID,
		Width:   1280,
		Height:  720,
	}
	return encodeScreenChunks(payload, h), payload
}

func TestReassembleSingleChunk(t *testing.T) {
	s := newTestScreenSvc()
	chunks, payload := buildScreenFrame(1, 1000, true)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	f, ok := s.ingestChunk("peerA", chunks[0])
	if !ok {
		t.Fatal("frame should be complete after its only chunk")
	}
	if !bytes.Equal(f.data, payload) {
		t.Fatal("reassembled data mismatch")
	}
	if !f.keyframe || f.frameID != 1 || f.width != 1280 || f.height != 720 {
		t.Fatalf("frame meta wrong: %+v", f)
	}
}

func TestReassembleMultiChunkInOrder(t *testing.T) {
	s := newTestScreenSvc()
	chunks, payload := buildScreenFrame(2, 2*screenWirePayloadSize+50, false)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	var f *assembledScreenFrame
	var ok bool
	for i, c := range chunks {
		f, ok = s.ingestChunk("peerA", c)
		if i < len(chunks)-1 && ok {
			t.Fatalf("frame completed early at chunk %d", i)
		}
	}
	if !ok {
		t.Fatal("frame not complete after all chunks")
	}
	if !bytes.Equal(f.data, payload) {
		t.Fatal("reassembled data mismatch (in order)")
	}
}

func TestReassembleMultiChunkOutOfOrder(t *testing.T) {
	s := newTestScreenSvc()
	chunks, payload := buildScreenFrame(3, 2*screenWirePayloadSize+50, false)
	// порядок 2,0,1 — unordered DC доставляет как попало
	order := []int{2, 0, 1}
	var f *assembledScreenFrame
	var ok bool
	for _, idx := range order {
		f, ok = s.ingestChunk("peerA", chunks[idx])
	}
	if !ok {
		t.Fatal("frame not complete after out-of-order delivery")
	}
	if !bytes.Equal(f.data, payload) {
		t.Fatal("reassembled data mismatch (out of order)")
	}
}

// TestReassembleInterleaveStaleGuard — ключевой кейс: пока собираем кадр N,
// приходит чанк кадра N+1 (буфер перезаводится на более свежий), а затем
// ОПОЗДАВШИЙ чанк кадра N — он должен быть проигнорирован, не сбросив прогресс N+1.
func TestReassembleInterleaveStaleGuard(t *testing.T) {
	s := newTestScreenSvc()
	aChunks, _ := buildScreenFrame(10, 2*screenWirePayloadSize+50, false)        // 3 chunks
	bChunks, bPayload := buildScreenFrame(11, 2*screenWirePayloadSize+50, false) // 3 chunks

	if _, ok := s.ingestChunk("peerA", aChunks[0]); ok {
		t.Fatal("frame 10 should not be complete")
	}
	if _, ok := s.ingestChunk("peerA", bChunks[0]); ok { // более свежий → перезаводит буфер
		t.Fatal("frame 11 should not be complete")
	}
	if _, ok := s.ingestChunk("peerA", aChunks[1]); ok { // СТАРЫЙ straggler → игнор
		t.Fatal("stale frame-10 chunk must be ignored, not complete a frame")
	}
	if _, ok := s.ingestChunk("peerA", bChunks[1]); ok {
		t.Fatal("frame 11 should not be complete yet")
	}
	f, ok := s.ingestChunk("peerA", bChunks[2])
	if !ok {
		t.Fatal("frame 11 must complete — stale frame-10 chunk should not have reset its buffer")
	}
	if f.frameID != 11 || !bytes.Equal(f.data, bPayload) {
		t.Fatalf("wrong frame assembled: id=%d data_ok=%v", f.frameID, bytes.Equal(f.data, bPayload))
	}
}

func TestIngestRejectsGarbage(t *testing.T) {
	s := newTestScreenSvc()
	if _, ok := s.ingestChunk("peerA", []byte{1, 2, 3}); ok { // короче header
		t.Fatal("garbage chunk must not produce a frame")
	}
	if _, ok := s.ingestChunk("peerA", make([]byte, screenWireHeaderSize)); ok { // bad magic
		t.Fatal("bad-magic chunk must not produce a frame")
	}
}

func TestScreenControlFrameRoundTrip(t *testing.T) {
	frame := encodeScreenControl(screenCtrlPLI)
	op, ok := decodeScreenControl(frame)
	if !ok || op != screenCtrlPLI {
		t.Fatalf("control PLI не распознан: ok=%v op=%d", ok, op)
	}
	// Обычный кадр — не control.
	chunks, _ := buildScreenFrame(5, 100, true)
	if _, ok := decodeScreenControl(chunks[0]); ok {
		t.Fatal("кадр с данными принят за control")
	}
	// Control не должен попадать в reassembly как кадр.
	s := newTestScreenSvc()
	if _, ok := s.ingestChunk("peerA", frame); ok {
		t.Fatal("control-фрейм собрался как кадр")
	}
}
