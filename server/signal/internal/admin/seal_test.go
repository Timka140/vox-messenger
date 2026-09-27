package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Общие векторы с шлюзом: design/northwind-site/test/gateway.test.mjs
// ("response encryption: Go vectors"). Меняя схему, обновить оба теста.
// Значения посчитаны Go-кодом; JS-тест независимо выводит тот же ключ
// (node:crypto hkdfSync), расшифровывает эти потоки и шифрует байт в байт те же.
const (
	vecToken    = "northwind-vector-token-0123456789abcdef"
	vecSig      = "5f1c0c5d9a7e1f3b2c4d6e8f00112233445566778899aabbccddeeff00112233"
	vecPart1    = `{"hello":"world"}`
	vecPart2    = "\n{\"n\":2,\"txt\":\"привет\"}\n"
	vecKeyHex   = "87b7edc1ef529a6f44c2d55e7281f2e2a8170e82dbcc31753040a9cb09a63855"
	vecSmallHex = "00000021000102030405060708090a0bc2d383b5384fa4df8d77c9f89650658581c9c20b7c8039a7f1209152a0826e81710000002e0c0d0e0f1011121314151617138c5867da1f9bafde7461bb74a8072eba1b128296c51c6bbf41f3db83620622479a4ec3236f792cb862346f67d00000001018191a1b1c1d1e1f20212223d4517950a294f02b5a3898876c195b0f"
	vecLargeLen = 150096
	vecLargeSHA = "0669b51a7968652a711aa94aaded74681f1e3b6362cc599015ef7581cb3284fc"
)

// counterNonce — детерминированные nonce: байты 0,1,2,…,255,0,… подряд;
// кадр i получает (12i+k) mod 256.
type counterNonce struct{ n int }

func (c *counterNonce) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n)
		c.n++
	}
	return len(p), nil
}

// vecLarge — 150000 байт (i*7+3) mod 251: три кадра 65536+65536+18928.
func vecLarge() []byte {
	b := make([]byte, 150000)
	for i := range b {
		b[i] = byte((i*7 + 3) % 251)
	}
	return b
}

func vecKey(t *testing.T) []byte {
	t.Helper()
	key, err := responseKey([]byte(vecToken))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// sealSmall — Write(part1) Flush Write(part2) Flush Close: кадры part1, part2, пустой финальный.
func sealSmall(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	s, err := newFrameSealer(&out, vecKey(t), vecSig, &counterNonce{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { _, err := s.Write([]byte(vecPart1)); return err },
		s.Flush,
		func() error { _, err := s.Write([]byte(vecPart2)); return err },
		s.Flush,
		s.Flush, // пустой Flush кадра не даёт
		s.Close,
		s.Close, // повторный Close — no-op
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("write after Close accepted")
	}
	return out.Bytes()
}

func sealLarge(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	s, err := newFrameSealer(&out, vecKey(t), vecSig, &counterNonce{})
	if err != nil {
		t.Fatal(err)
	}
	// Пишем кусками разного размера — кадрирование от этого не зависит.
	data := vecLarge()
	for i := 0; i < len(data); i += 7777 {
		if _, err := s.Write(data[i:min(i+7777, len(data))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func openAll(stream []byte, key []byte, sig string) ([]byte, error) {
	o, err := newFrameOpener(bytes.NewReader(stream), key, sig)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(o)
}

// frames — разбор потока на кадры (для подделок в тестах).
func frames(t *testing.T, stream []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(stream) > 0 {
		n := int(binary.BigEndian.Uint32(stream[:4]))
		out = append(out, stream[:4+frameNonce+n])
		stream = stream[4+frameNonce+n:]
	}
	return out
}

func TestSealVectors(t *testing.T) {
	if got := hex.EncodeToString(vecKey(t)); got != vecKeyHex {
		t.Fatalf("key %s", got)
	}
	small := sealSmall(t)
	if got := hex.EncodeToString(small); got != vecSmallHex {
		t.Fatalf("small stream changed:\n%s", got)
	}
	if fr := frames(t, small); len(fr) != 3 || len(fr[2]) != 4+frameNonce+frameTag {
		t.Fatalf("small frames %d", len(fr))
	}
	large := sealLarge(t)
	sum := sha256.Sum256(large)
	if len(large) != vecLargeLen || hex.EncodeToString(sum[:]) != vecLargeSHA {
		t.Fatalf("large stream changed: len %d sha %x", len(large), sum)
	}
	fr := frames(t, large)
	if len(fr) != 3 || len(fr[0]) != 4+frameNonce+frameMaxCT || len(fr[2]) != 4+frameNonce+18928+frameTag {
		t.Fatalf("large frames %d", len(fr))
	}

	// Поток, записанный шлюзом (JS), байт в байт тот же vecSmallHex — Go его читает.
	js, _ := hex.DecodeString(vecSmallHex)
	plain, err := openAll(js, vecKey(t), vecSig)
	if err != nil || string(plain) != vecPart1+vecPart2 {
		t.Fatalf("open small: %q %v", plain, err)
	}
	plain, err = openAll(large, vecKey(t), vecSig)
	if err != nil || !bytes.Equal(plain, vecLarge()) {
		t.Fatalf("open large: %v", err)
	}
}

func TestSealExactFrame(t *testing.T) {
	// 0 байт — один пустой финальный кадр; ровно 64 КиБ — один финальный; +1 — два.
	for _, n := range []int{0, frameMaxPlain, frameMaxPlain + 1} {
		var out bytes.Buffer
		s, _ := newFrameSealer(&out, vecKey(t), vecSig, nil)
		_, _ = s.Write(make([]byte, n))
		_ = s.Close()
		want := 1
		if n > frameMaxPlain {
			want = 2
		}
		if got := len(frames(t, out.Bytes())); got != want {
			t.Fatalf("n=%d frames %d", n, got)
		}
		if plain, err := openAll(out.Bytes(), vecKey(t), vecSig); err != nil || len(plain) != n {
			t.Fatalf("n=%d: %d %v", n, len(plain), err)
		}
	}
}

func TestSealRejects(t *testing.T) {
	key := vecKey(t)
	small := sealSmall(t)
	large := sealLarge(t)
	fr := frames(t, large)
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	flip := func(b []byte, i int) []byte {
		c := bytes.Clone(b)
		c[i] ^= 1
		return c
	}
	oversize := make([]byte, 4+frameNonce)
	binary.BigEndian.PutUint32(oversize, frameMaxCT+1)
	short := make([]byte, 4+frameNonce+frameTag-1)
	binary.BigEndian.PutUint32(short, frameTag-1)

	otherKey, _ := responseKey([]byte("another-token-0123456789abcdef0123456789"))
	cases := []struct {
		name   string
		stream []byte
		key    []byte
		sig    string
		want   error
	}{
		{"empty", nil, key, vecSig, errNoFinal},
		{"missing final", join(fr[0], fr[1]), key, vecSig, errNoFinal},
		{"truncated mid-frame", large[:len(large)-5], key, vecSig, errNoFinal},
		{"truncated header", large[:len(fr[0])+2], key, vecSig, errNoFinal},
		{"trailing data", join(large, []byte{0}), key, vecSig, errTrailingData},
		{"trailing frame", join(small, fr[0]), key, vecSig, errTrailingData},
		{"reordered", join(fr[1], fr[0], fr[2]), key, vecSig, errFrameAuth},
		{"dropped middle", join(fr[0], fr[2]), key, vecSig, errFrameAuth},
		{"tampered ciphertext", flip(large, 4+frameNonce+100), key, vecSig, errFrameAuth},
		{"tampered tag", flip(small, len(small)-1), key, vecSig, errFrameAuth},
		{"tampered nonce", flip(small, 5), key, vecSig, errFrameAuth},
		{"other request sig", small, key, strings.Repeat("0", 64), errFrameAuth},
		{"other key", small, otherKey, vecSig, errFrameAuth},
		{"oversize frame", oversize, key, vecSig, errFrameTooLarge},
		{"short frame", short, key, vecSig, errFrameShort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := openAll(c.stream, c.key, c.sig)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
		})
	}
}

// Через весь API: ответ привязан к подписи запроса, ошибки обработчиков
// шифруются, Flush обработчика даёт отдельный кадр.
func TestSealAPI(t *testing.T) {
	api := New(Options{Token: testToken, NodeID: "abc", Now: func() time.Time { return testNow }})
	api.nonce = &counterNonce{}
	h := api.Handler()
	ts := strconv.FormatInt(testNow.UnixMilli(), 10)
	r := httptest.NewRequest("GET", "/admin/v1/status", nil)
	sig := Sign(testToken, "GET", "/admin/v1/status", ts, nil)
	r.Header.Set("X-NW-Ts", ts)
	r.Header.Set("X-NW-Sig", sig)
	r.Header.Set("X-NW-Enc", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	key, _ := responseKey([]byte(testToken))
	other := Sign(testToken, "GET", "/admin/v1/status", ts, []byte("x"))
	if _, err := openAll(w.Body.Bytes(), key, other); !errors.Is(err, errFrameAuth) {
		t.Fatalf("response opened with another request's sig: %v", err)
	}
	plain, err := openAll(w.Body.Bytes(), key, sig)
	if err != nil || !strings.Contains(string(plain), `"node_id":"abc"`) {
		t.Fatalf("%q %v", plain, err)
	}

	w = req(h, "GET", "/admin/v1/nope", nil, time.Millisecond)
	if w.Code != 404 || w.Body.String() != "{\"error\":\"not found\"}\n" || w.Header().Get("X-NW-Enc") != "1" {
		t.Fatalf("404: %d %q", w.Code, w.Body)
	}

	rec := httptest.NewRecorder()
	fs, _ := newFrameSealer(rec, key, sig, nil)
	sw := &sealWriter{rw: rec, fs: fs}
	sw.Header().Set("Content-Type", "application/x-ndjson")
	sw.Header().Set("Content-Length", "10")
	_, _ = sw.Write([]byte("a\n"))
	sw.Flush()
	_, _ = sw.Write([]byte("b\n"))
	if err := sw.finish(); err != nil {
		t.Fatal(err)
	}
	if !rec.Flushed || rec.Header().Get("X-NW-Type") != "application/x-ndjson" || rec.Header().Get("Content-Length") != "" {
		t.Fatalf("flushed %v headers %v", rec.Flushed, rec.Header())
	}
	if n := len(frames(t, rec.Body.Bytes())); n != 2 {
		t.Fatalf("frames %d", n)
	}
	if plain, err := openAll(rec.Body.Bytes(), key, sig); err != nil || string(plain) != "a\nb\n" {
		t.Fatalf("%q %v", plain, err)
	}
}
