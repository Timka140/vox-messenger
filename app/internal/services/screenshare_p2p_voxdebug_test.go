//go:build voxdebug

package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"image"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"vox_app/internal/codec/h264enc"
	"vox_app/internal/network"
)

// dirRouter — однонаправленный маршрутизатор сигналов между двумя PeerManager'ами
// в тесте. Гарантирует порядок «SDP раньше ICE»: HandleICE в peermgr ОТБРАСЫВАЕТ
// кандидата, если remote description ещё не установлен, поэтому ICE-сообщения
// буферизуются до доставки offer/answer для того же pcId.
type dirRouter struct {
	ch chan *network.Message
}

func newDirRouter(deliver func(*network.Message)) *dirRouter {
	r := &dirRouter{ch: make(chan *network.Message, 512)}
	go func() {
		sdpSeen := map[int]bool{}
		iceBuf := map[int][]*network.Message{}
		for m := range r.ch {
			if m.Type == network.TypeICE && !sdpSeen[m.PcId] {
				iceBuf[m.PcId] = append(iceBuf[m.PcId], m)
				continue
			}
			deliver(m)
			if m.Type == network.TypeOffer || m.Type == network.TypeAnswer {
				sdpSeen[m.PcId] = true
				for _, bm := range iceBuf[m.PcId] {
					deliver(bm)
				}
				iceBuf[m.PcId] = nil
			}
		}
	}()
	return r
}

func (r *dirRouter) send(m *network.Message) { r.ch <- m }

func dispatchSignal(pm *network.PeerManager, fromPK string, m *network.Message) {
	switch m.Type {
	case network.TypeOffer:
		_ = pm.HandleOffer(fromPK, m.Payload, m.PcId)
	case network.TypeAnswer:
		_ = pm.HandleAnswer(fromPK, m.Payload, m.PcId)
	case network.TypeICE:
		_ = pm.HandleICE(fromPK, m.Payload, m.PcId)
	}
}

// TestScreenShareP2PLoopback — полный end-to-end сетевой тест демонстрации экрана
// БЕЗ GUI и без TURN: два PeerManager'а в одном процессе, сигналинг закольцован
// in-process, screen PC поднимается по loopback (voxdebug → ICE All + loopback),
// кадр реально летит через настоящий pion DataChannel и собирается на ресивере.
//
// Проверяет именно тот код, что я менял: EnsureScreenPC, setupScreenDataChannel,
// SendScreenChunk (backpressure), фрагментацию на chunks и reassembly.
func TestScreenShareP2PLoopback(t *testing.T) {
	privA, privB, pkA, pkB := signedPair(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	noopMsg := func(string, []byte) {}
	noopState := func(string, bool) {}

	var pmA, pmB *network.PeerManager
	routeAtoB := newDirRouter(func(m *network.Message) { dispatchSignal(pmB, pkA, m) })
	routeBtoA := newDirRouter(func(m *network.Message) { dispatchSignal(pmA, pkB, m) })

	pmA = network.NewPeerManager(logger, pkA, func(m *network.Message) error { routeAtoB.send(m); return nil }, noopMsg, noopState)
	pmB = network.NewPeerManager(logger, pkB, func(m *network.Message) error { routeBtoA.send(m); return nil }, noopMsg, noopState)
	defer pmA.CloseAll()
	defer pmB.CloseAll()
	pmA.SetICEServers(nil)
	pmB.SetICEServers(nil)
	pmA.SetSigner(privA)
	pmB.SetSigner(privB)

	var rxMu sync.Mutex
	var rx [][]byte
	pmB.SetOnScreenMessage(func(from string, data []byte) {
		rxMu.Lock()
		rx = append(rx, append([]byte(nil), data...))
		rxMu.Unlock()
	})

	if err := pmA.EnsureScreenPC(pkB); err != nil {
		t.Fatalf("EnsureScreenPC: %v", err)
	}

	// Ждём открытия screen DC с обеих сторон.
	waitOpen := func(name string, pm *network.PeerManager, peer string) {
		deadline := time.Now().Add(25 * time.Second)
		for !pm.IsScreenDCOpen(peer) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: screen DC не открылся за 25s", name)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitOpen("sender(A)", pmA, pkB)
	waitOpen("viewer(B)", pmB, pkA)
	t.Log("screen DC открыт с обеих сторон")

	// Кадр из 3 chunks (фрагментация через SCTP) с детерминированным payload.
	chunks, payload := buildScreenFrame(42, 2*screenWirePayloadSize+50, true)
	if len(chunks) != 3 {
		t.Fatalf("ожидалось 3 chunks, получено %d", len(chunks))
	}
	for i, c := range chunks {
		if err := pmA.SendScreenChunk(pkB, c); err != nil {
			t.Fatalf("SendScreenChunk %d: %v", i, err)
		}
	}

	// Ждём прихода всех 3 chunks на ресивер.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rxMu.Lock()
		n := len(rx)
		rxMu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("получено только %d/3 chunks за 10s", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Собираем кадр на ресивере через ту же логику, что в проде (ingestChunk).
	recv := newTestScreenSvc()
	var assembled *assembledScreenFrame
	rxMu.Lock()
	got := rx
	rxMu.Unlock()
	for _, c := range got {
		if f, ok := recv.ingestChunk(pkA, c); ok {
			assembled = f
		}
	}
	if assembled == nil {
		t.Fatal("кадр не собрался из принятых по сети chunks")
	}
	if !bytes.Equal(assembled.data, payload) {
		t.Fatalf("payload не совпал: got %d bytes, want %d", len(assembled.data), len(payload))
	}
	if assembled.frameID != 42 || !assembled.keyframe {
		t.Fatalf("метаданные кадра неверны: %+v", assembled)
	}
	t.Logf("OK: кадр %d (%d байт) доставлен по loopback и собран корректно", assembled.frameID, len(assembled.data))
}

// TestScreenShareP2PStreaming — стриминг множества кадров через реальный screen DC,
// как это делает senderLoop (chunks кадра сериями, кадр за кадром). Проверяет, что
// ресивер собирает кадры с монотонными frameID, корректными payload и без потерь
// из-за трешинга буфера (главная болезнь старого pipeline=2).
func TestScreenShareP2PStreaming(t *testing.T) {
	privA, privB, pkA, pkB := signedPair(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	noopMsg := func(string, []byte) {}
	noopState := func(string, bool) {}

	var pmA, pmB *network.PeerManager
	routeAtoB := newDirRouter(func(m *network.Message) { dispatchSignal(pmB, pkA, m) })
	routeBtoA := newDirRouter(func(m *network.Message) { dispatchSignal(pmA, pkB, m) })
	pmA = network.NewPeerManager(logger, pkA, func(m *network.Message) error { routeAtoB.send(m); return nil }, noopMsg, noopState)
	pmB = network.NewPeerManager(logger, pkB, func(m *network.Message) error { routeBtoA.send(m); return nil }, noopMsg, noopState)
	defer pmA.CloseAll()
	defer pmB.CloseAll()
	pmA.SetICEServers(nil)
	pmB.SetICEServers(nil)
	pmA.SetSigner(privA)
	pmB.SetSigner(privB)

	// Ресивер собирает кадры через прод-логику ingestChunk.
	recv := newTestScreenSvc()
	var rxMu sync.Mutex
	assembled := map[uint32][]byte{}
	pmB.SetOnScreenMessage(func(from string, data []byte) {
		if f, ok := recv.ingestChunk(from, data); ok {
			rxMu.Lock()
			assembled[f.frameID] = f.data
			rxMu.Unlock()
		}
	})

	if err := pmA.EnsureScreenPC(pkB); err != nil {
		t.Fatalf("EnsureScreenPC: %v", err)
	}
	deadline := time.Now().Add(25 * time.Second)
	for !pmA.IsScreenDCOpen(pkB) || !pmB.IsScreenDCOpen(pkA) {
		if time.Now().After(deadline) {
			t.Fatal("screen DC не открылся за 25s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Шлём 50 кадров. Payload кадров — разной длины (1..3 chunks) и содержимого.
	const frames = 50
	want := map[uint32][]byte{}
	for k := 1; k <= frames; k++ {
		fid := uint32(k)
		payloadLen := 5000 + (k%3)*screenWirePayloadSize // 1..3 chunks
		chunks, payload := buildScreenFrame(fid, payloadLen, true)
		want[fid] = payload
		for _, c := range chunks {
			if err := pmA.SendScreenChunk(pkB, c); err != nil {
				t.Fatalf("SendScreenChunk кадр %d: %v", fid, err)
			}
		}
		time.Sleep(2 * time.Millisecond) // ~ темп реального захвата
	}

	// Ждём пока приём стабилизируется.
	dl := time.Now().Add(10 * time.Second)
	for {
		rxMu.Lock()
		n := len(assembled)
		rxMu.Unlock()
		if n >= frames || time.Now().After(dl) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	rxMu.Lock()
	defer rxMu.Unlock()
	// Loopback DC без потерь → ожидаем ВСЕ кадры целыми и корректными.
	if len(assembled) != frames {
		t.Fatalf("собрано %d/%d кадров (loopback не должен терять)", len(assembled), frames)
	}
	for fid, data := range want {
		if !bytes.Equal(assembled[fid], data) {
			t.Fatalf("кадр %d: payload mismatch (got %d, want %d байт)", fid, len(assembled[fid]), len(data))
		}
	}
	t.Logf("OK: все %d кадров доставлены и собраны без потерь и искажений", frames)
}

// TestScreenShareP2PH264 — настоящий H.264-кадр (h264enc) по loopback screen
// DC (теперь ordered+reliable): ресивер собирает кадр с codec=H264 и
// keyframe-флагом, поток содержит SPS/PPS/IDR. Плюс обратный control-фрейм
// PLI зритель → отправитель по тому же DC.
func TestScreenShareP2PH264(t *testing.T) {
	privA, privB, pkA, pkB := signedPair(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	noopMsg := func(string, []byte) {}
	noopState := func(string, bool) {}

	var pmA, pmB *network.PeerManager
	routeAtoB := newDirRouter(func(m *network.Message) { dispatchSignal(pmB, pkA, m) })
	routeBtoA := newDirRouter(func(m *network.Message) { dispatchSignal(pmA, pkB, m) })
	pmA = network.NewPeerManager(logger, pkA, func(m *network.Message) error { routeAtoB.send(m); return nil }, noopMsg, noopState)
	pmB = network.NewPeerManager(logger, pkB, func(m *network.Message) error { routeBtoA.send(m); return nil }, noopMsg, noopState)
	defer pmA.CloseAll()
	defer pmB.CloseAll()
	pmA.SetICEServers(nil)
	pmB.SetICEServers(nil)
	pmA.SetSigner(privA)
	pmB.SetSigner(privB)

	var rxMu sync.Mutex
	var rxB, rxA [][]byte
	pmB.SetOnScreenMessage(func(from string, data []byte) {
		rxMu.Lock()
		rxB = append(rxB, append([]byte(nil), data...))
		rxMu.Unlock()
	})
	pmA.SetOnScreenMessage(func(from string, data []byte) {
		rxMu.Lock()
		rxA = append(rxA, append([]byte(nil), data...))
		rxMu.Unlock()
	})
	if err := pmA.EnsureScreenPC(pkB); err != nil {
		t.Fatalf("EnsureScreenPC: %v", err)
	}
	waitOpen := func(name string, pm *network.PeerManager, peer string) {
		deadline := time.Now().Add(25 * time.Second)
		for !pm.IsScreenDCOpen(peer) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: screen DC не открылся за 25s", name)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitOpen("sender(A)", pmA, pkB)
	waitOpen("viewer(B)", pmB, pkA)

	// Реальный keyframe 1280×720.
	const w, h = 1280, 720
	enc, err := h264enc.New(h264enc.Config{Width: w, Height: h, FPS: 30, Bitrate: 2_500_000})
	if err != nil {
		t.Fatalf("h264enc.New: %v", err)
	}
	defer enc.Close()
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range rgba.Pix {
		rgba.Pix[i] = uint8(i * 7)
	}
	yuv := image.NewYCbCr(rgba.Rect, image.YCbCrSubsampleRatio420)
	h264enc.RGBAToI420(rgba, yuv)
	payload, key, err := enc.Encode(yuv, true)
	if err != nil || !key || len(payload) == 0 {
		t.Fatalf("encode: err=%v key=%v len=%d", err, key, len(payload))
	}
	hdr := screenFrameHeader{Version: screenWireVersion, Flags: screenFlagKeyframe, Codec: screenCodecH264, FrameID: 7, Width: w, Height: h}
	chunks := encodeScreenChunks(payload, hdr)
	for i, c := range chunks {
		if err := pmA.SendScreenChunk(pkB, c); err != nil {
			t.Fatalf("SendScreenChunk %d: %v", i, err)
		}
	}
	// PLI в обратную сторону — по DC, который открыл A.
	if err := pmB.SendScreenChunk(pkA, encodeScreenControl(screenCtrlPLI)); err != nil {
		t.Fatalf("SendScreenChunk PLI: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		rxMu.Lock()
		nb, na := len(rxB), len(rxA)
		rxMu.Unlock()
		if nb >= len(chunks) && na >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("получено B=%d/%d chunks, A=%d control за 10s", nb, len(chunks), na)
		}
		time.Sleep(20 * time.Millisecond)
	}
	recv := newTestScreenSvc()
	var assembled *assembledScreenFrame
	rxMu.Lock()
	gotB, gotA := rxB, rxA
	rxMu.Unlock()
	for _, c := range gotB {
		if f, ok := recv.ingestChunk(pkA, c); ok {
			assembled = f
		}
	}
	if assembled == nil {
		t.Fatal("H.264-кадр не собрался")
	}
	if assembled.codec != screenCodecH264 || !assembled.keyframe || !bytes.Equal(assembled.data, payload) {
		t.Fatalf("кадр неверен: codec=%d key=%v len=%d/%d", assembled.codec, assembled.keyframe, len(assembled.data), len(payload))
	}
	if !h264enc.IsKeyframe(assembled.data) {
		t.Fatal("в собранном потоке нет IDR")
	}
	op, ok := decodeScreenControl(gotA[0])
	if !ok || op != screenCtrlPLI {
		t.Fatalf("PLI не дошёл до отправителя: ok=%v op=%d", ok, op)
	}
	t.Logf("OK: H.264 keyframe %d байт в %d chunks доставлен и собран; PLI получен", len(payload), len(chunks))
}

// signedPair — ключи профилей двух сторон: offer/answer подписываются
// (network/sdpsign.go), фиктивные hex-ключи больше не годятся.
func signedPair(t *testing.T) (ed25519.PrivateKey, ed25519.PrivateKey, string, string) {
	t.Helper()
	pa, a, _ := ed25519.GenerateKey(rand.Reader)
	pb, b, _ := ed25519.GenerateKey(rand.Reader)
	return a, b, hex.EncodeToString(pa), hex.EncodeToString(pb)
}
