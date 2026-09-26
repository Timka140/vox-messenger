package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vox_app/internal/services/files/wire"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func TestMissingNotInFlight(t *testing.T) {
	bm := NewBitmap(20)
	for _, i := range []int{0, 1, 2, 5} {
		bm.Set(i)
	}
	inFlight := map[int]bool{3: true, 4: true, 6: true}
	// Missing и не in-flight: 7..19 (13 штук). batch=4, maxChunks=10 →
	// [7..10], [11..14], [15..16].
	got := missingNotInFlight(bm, inFlight, nil, time.Now(), 10, 4)
	want := []ChunkRange{{Start: 7, Count: 4}, {Start: 11, Count: 4}, {Start: 15, Count: 2}}
	if len(got) != len(want) {
		t.Fatalf("ranges: got %+v want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	// Дырки внутри: in-flight разрывает диапазон.
	inFlight[9] = true
	got = missingNotInFlight(bm, inFlight, nil, time.Now(), 100, 8)
	want = []ChunkRange{{Start: 7, Count: 2}, {Start: 10, Count: 8}, {Start: 18, Count: 2}}
	if len(got) != len(want) {
		t.Fatalf("ranges with hole: got %+v want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if r := missingNotInFlight(bm, inFlight, nil, time.Now(), 0, 8); r != nil {
		t.Fatalf("maxChunks=0 должен вернуть nil, got %+v", r)
	}
	// Deferred (после NAK) пропускаются, пока не истекли; истёкшие — снова в игре.
	now := time.Now()
	deferred := map[int]time.Time{7: now.Add(time.Minute), 8: now.Add(-time.Second)}
	got = missingNotInFlight(bm, inFlight, deferred, now, 100, 8)
	want = []ChunkRange{{Start: 8, Count: 1}, {Start: 10, Count: 8}, {Start: 18, Count: 2}}
	if len(got) != len(want) {
		t.Fatalf("ranges with deferred: got %+v want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if _, still := deferred[8]; still {
		t.Fatalf("истёкший deferred должен быть удалён")
	}
}

func TestPullMaxInFlight(t *testing.T) {
	if got := pullMaxInFlight(64 * 1024); got != 24 {
		t.Fatalf("64 KiB: got %d want 24", got)
	}
	if got := pullMaxInFlight(pullWindowBytes * 4); got != pullBatchSize {
		t.Fatalf("огромный chunk: got %d want %d", got, pullBatchSize)
	}
	if got := pullMaxInFlight(0); got != pullBatchSize {
		t.Fatalf("chunkSize=0: got %d want %d", got, pullBatchSize)
	}
}

// fakeNet — NetworkOps, который сам играет sender'а: на CHUNK_REQUEST отвечает
// chunk_data с задержкой, один чанк один раз NAK'ает, один — отдаёт битым.
// Считает окно in-flight и паузы между ответом и следующим запросом.
type fakeNet struct {
	svc   *FilesService
	peer  string
	sha   string
	data  [][]byte
	delay time.Duration

	mu          sync.Mutex
	inflight    int
	maxInflight int
	requests    int
	lastResp    time.Time
	maxGap      time.Duration
	nakOnce     map[int]bool
	corruptOnce map[int]bool
}

func (f *fakeNet) IsP2PConnected(string) bool                       { return true }
func (f *fakeNet) IsAttachDCOpen(string) bool                       { return true }
func (f *fakeNet) SendP2PData(string, []byte) error                 { return nil }
func (f *fakeNet) EnsureFilesPCReady(context.Context, string) error { return nil }
func (f *fakeNet) AttachBufferedAmount(string) uint64               { return 0 }

func (f *fakeNet) SendAttachChunk(peer string, frame []byte) error {
	if wire.FrameType(frame) != wire.TypeChunkRequest {
		return nil
	}
	_, start, count, err := wire.DecodeChunkRequest(frame)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.requests++
	if !f.lastResp.IsZero() {
		if gap := time.Since(f.lastResp); gap > f.maxGap {
			f.maxGap = gap
		}
	}
	f.inflight += count
	if f.inflight > f.maxInflight {
		f.maxInflight = f.inflight
	}
	f.mu.Unlock()

	go func() {
		time.Sleep(f.delay)
		for i := 0; i < count; i++ {
			idx := start + i
			var resp []byte
			f.mu.Lock()
			switch {
			case f.nakOnce[idx]:
				delete(f.nakOnce, idx)
				resp, _ = wire.EncodeChunkNak(f.sha, idx, wire.NakNotFound)
			case f.corruptOnce[idx]:
				delete(f.corruptOnce, idx)
				bad := append([]byte(nil), f.data[idx]...)
				bad[0] ^= 0xff
				resp, _ = wire.EncodeChunkData(f.sha, idx, bad)
			default:
				resp, _ = wire.EncodeChunkData(f.sha, idx, f.data[idx])
			}
			f.inflight--
			f.lastResp = time.Now()
			f.mu.Unlock()
			f.svc.HandleFrame(peer, resp)
		}
	}()
	return nil
}

// TestPullLoop_WindowAndCompletion — pullLoop с фейковым sender'ом: окно не
// превышает pullMaxInFlight, запросы уходят без пауз (событийно, не по
// sleep'у), NAK и SHA mismatch перезапрашиваются, файл собирается целиком.
func TestPullLoop_WindowAndCompletion(t *testing.T) {
	const chunkSize = 64 * 1024
	const n = 60
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc := New(logger, session.New(), store.DB())

	data := make([][]byte, n)
	shas := make([]string, n)
	var whole []byte
	for i := range data {
		data[i] = make([]byte, chunkSize)
		if i == n-1 {
			data[i] = data[i][:chunkSize/2] // хвост короче
		}
		_, _ = rand.Read(data[i])
		h := sha256.Sum256(data[i])
		shas[i] = hex.EncodeToString(h[:])
		whole = append(whole, data[i]...)
	}
	fileHash := sha256.Sum256(whole)
	fileSha := hex.EncodeToString(fileHash[:])
	peer := "peer-test"

	fn := &fakeNet{
		svc: svc, peer: peer, sha: fileSha, data: data, delay: 20 * time.Millisecond,
		nakOnce:     map[int]bool{17: true},
		corruptOnce: map[int]bool{33: true},
	}
	svc.SetNetwork(fn)

	dir := t.TempDir()
	partialPath := filepath.Join(dir, "t.bin.vox-partial")
	finalPath := filepath.Join(dir, "t.bin")
	pf, err := os.OpenFile(partialPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	st := &downloadState{
		fileSha:    fileSha,
		sourcePeer: peer,
		manifest: &wire.FileManifestEntry{
			Sha: fileSha, Size: int64(len(whole)), ChunkSize: chunkSize,
			ChunkCount: n, Chunks: shas, Name: "t.bin", Kind: "file",
		},
		bitmap:      NewBitmap(n),
		inFlight:    make(map[int]bool),
		wake:        make(chan struct{}, 1),
		pendingMsgs: map[string]bool{"msg1": true},
		lastActive:  time.Now(),
		cancel:      make(chan struct{}),
		partialFile: pf,
		partialPath: partialPath,
		finalPath:   finalPath,
	}
	svc.mu.Lock()
	svc.downloads[fileSha+"|"+peer] = st
	svc.mu.Unlock()

	go svc.pullLoop(st, "msg1")

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(finalPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fn.mu.Lock()
			t.Fatalf("файл не собрался за 30s: requests=%d inflight=%d", fn.requests, fn.inflight)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(got, whole) {
		t.Fatalf("содержимое не совпадает: got %d bytes want %d", len(got), len(whole))
	}

	fn.mu.Lock()
	defer fn.mu.Unlock()
	maxIF := pullMaxInFlight(chunkSize)
	if fn.maxInflight > maxIF {
		t.Fatalf("окно превышено: max inflight %d > %d", fn.maxInflight, maxIF)
	}
	if fn.maxInflight < maxIF {
		t.Fatalf("окно не заполнялось: max inflight %d < %d", fn.maxInflight, maxIF)
	}
	if fn.requests < n/pullBatchSize {
		t.Fatalf("слишком мало запросов: %d", fn.requests)
	}
	if fn.maxGap > 300*time.Millisecond {
		t.Fatalf("пауза между ответом и следующим запросом %s — loop не событийный", fn.maxGap)
	}
	if got := svc.stats.naks.Load(); got != 1 {
		t.Fatalf("naks: got %d want 1", got)
	}
	if got := svc.stats.mismatches.Load(); got != 1 {
		t.Fatalf("mismatches: got %d want 1", got)
	}
	if got := svc.stats.chunksIn.Load(); got != n {
		t.Fatalf("chunks_in: got %d want %d", got, n)
	}
	svc.mu.Lock()
	_, still := svc.downloads[fileSha+"|"+peer]
	svc.mu.Unlock()
	if still {
		t.Fatalf("download state не удалён после complete")
	}
}
