package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// makeStore — helper: временный BlobStore с 32-байтным ключом.
func makeStore(t *testing.T) (*BlobStore, []byte) {
	t.Helper()
	tmp := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	bs, err := NewBlobStore(tmp, key)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	return bs, key
}

// randBytes — псевдо-случайный детерминированный buffer размера n.
func randBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func TestPutGet_SmallRoundTrip(t *testing.T) {
	bs, _ := makeStore(t)
	data := []byte("hello attachment world")

	sha, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	expect := sha256.Sum256(data)
	if sha != hex.EncodeToString(expect[:]) {
		t.Fatalf("sha mismatch: got %s want %s", sha, hex.EncodeToString(expect[:]))
	}

	got, err := bs.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("roundtrip data mismatch")
	}
}

func TestPutGet_LargeRoundTrip(t *testing.T) {
	// 5 MiB — пересекает несколько chunk'ов (>4 chunks по 1 MiB).
	sizes := []int{0, 1, 1023, 1 << 20, (1 << 20) + 1, 5 << 20}
	bs, _ := makeStore(t)
	for _, n := range sizes {
		data := randBytes(n)
		sha, err := bs.Put(data)
		if err != nil {
			t.Fatalf("Put(%d): %v", n, err)
		}
		got, err := bs.Get(sha)
		if err != nil {
			t.Fatalf("Get(%d): %v", n, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("roundtrip mismatch at n=%d", n)
		}
	}
}

func TestPutFromReader_Streaming(t *testing.T) {
	bs, _ := makeStore(t)
	data := randBytes(3 << 20)
	sha, err := bs.PutFromReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PutFromReader: %v", err)
	}
	expect := sha256.Sum256(data)
	if sha != hex.EncodeToString(expect[:]) {
		t.Fatalf("sha mismatch")
	}
	if !bs.Exists(sha) {
		t.Fatalf("Exists(sha)=false after put")
	}
}

func TestOpenReader_Streams(t *testing.T) {
	bs, _ := makeStore(t)
	data := randBytes(2500000) // ~2.4 MiB — неровный хвост
	sha, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	r, err := bs.OpenReader(sha)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	// Читаем маленькими кусками — проверка что chunk boundaries не ломают вывод.
	out := &bytes.Buffer{}
	small := make([]byte, 997)
	for {
		n, rerr := r.Read(small)
		if n > 0 {
			out.Write(small[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatalf("Read: %v", rerr)
		}
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("streamed read mismatch: got %d bytes, want %d", out.Len(), len(data))
	}
}

func TestWriter_AbortLeavesNoGarbage(t *testing.T) {
	bs, _ := makeStore(t)
	w, err := bs.NewWriter()
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := w.Write(randBytes(100000)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// В root не должно остаться *.tmp.
	entries, err := os.ReadDir(bs.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".tmp" {
			t.Fatalf("leftover tmp after Abort: %s", name)
		}
	}
}

func TestBackwardCompat_LegacyFormat(t *testing.T) {
	// Создаём файл в старом формате (nonce + seal(всё)) напрямую и проверяем
	// что OpenReader его читает.
	tmp := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	bs, err := NewBlobStore(tmp, key)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}

	data := []byte("legacy-format payload — must round-trip without magic header")
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	// Прямо шифруем как старый Put (nonce(12) || seal(пт)).
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	enc := append([]byte{}, nonce...)
	enc = aead.Seal(enc, nonce, data, nil)

	// Пишем в каноничный путь.
	path := bs.pathFor(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := bs.Get(sha)
	if err != nil {
		t.Fatalf("Get legacy: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("legacy roundtrip mismatch")
	}
}

func TestDelete_IdempotentOnMissing(t *testing.T) {
	bs, _ := makeStore(t)
	if err := bs.Delete("0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("Delete non-existent should not error: %v", err)
	}
	sha, err := bs.Put([]byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := bs.Delete(sha); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if bs.Exists(sha) {
		t.Fatalf("Exists still true after Delete")
	}
}

func TestPut_IdempotentSameData(t *testing.T) {
	bs, _ := makeStore(t)
	data := randBytes(50000)
	sha1, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put#1: %v", err)
	}
	sha2, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put#2: %v", err)
	}
	if sha1 != sha2 {
		t.Fatalf("sha differs: %s vs %s", sha1, sha2)
	}
}

func TestOpenReaderAt_JumpsToOffset(t *testing.T) {
	bs, _ := makeStore(t)
	// 3 полных chunk'а + неровный хвост — проверяем все ветки.
	data := randBytes(3*(1<<20) + 12345)
	sha, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	cases := []int64{
		0,
		1,
		1 << 20,            // ровно граница chunk'а
		(1 << 20) + 500,    // внутри второго
		(2 << 20) + 999999, // ближе к концу третьего
		3 << 20,            // граница последнего (tail)
		3*(1<<20) + 12340,  // почти конец
		int64(len(data)),   // самый конец
	}
	for _, off := range cases {
		r, err := bs.OpenReaderAt(sha, off)
		if err != nil {
			t.Fatalf("OpenReaderAt(%d): %v", off, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("ReadAll(%d): %v", off, err)
		}
		want := data[off:]
		if !bytes.Equal(got, want) {
			t.Fatalf("offset %d: got %d bytes, want %d (first diff at %d)",
				off, len(got), len(want), firstDiff(got, want))
		}
	}
}

func TestOpenReaderAt_NegativeOffsetErrors(t *testing.T) {
	bs, _ := makeStore(t)
	sha, _ := bs.Put([]byte("x"))
	if _, err := bs.OpenReaderAt(sha, -1); err == nil {
		t.Fatalf("expected error for negative offset")
	}
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func TestReader_TamperedChunkFails(t *testing.T) {
	bs, _ := makeStore(t)
	data := randBytes(1_500_000) // 2 chunk'а
	sha, err := bs.Put(data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Ломаем один байт в середине файла.
	path := bs.pathFor(sha)
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(buf) < 100 {
		t.Fatalf("file suspiciously small: %d", len(buf))
	}
	buf[len(buf)/2] ^= 0x01
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r, err := bs.OpenReader(sha)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	_, err = io.ReadAll(r)
	if err == nil {
		t.Fatalf("expected decrypt error after tampering, got nil")
	}
}
