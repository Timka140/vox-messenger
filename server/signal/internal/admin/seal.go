package admin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Шифрование ответов admin API (ADMIN-CONTRACT.md §1, «Шифрование ответов»).
// TLS на узлах нет, поэтому тело каждого ответа после проверки подписи —
// поток кадров AES-256-GCM на ключе из ADMIN_TOKEN:
//
//	кадр = [4 байта BE: N][12 байт nonce][N байт шифротекста с тегом 16 байт]
//	AAD  = "<X-NW-Sig запроса, hex нижний регистр>\n<номер кадра>\n<final 0|1>"
//
// Открытый текст кадра ≤ 64 КиБ, последний кадр final=1 (может быть пустым).
// AAD привязывает ответ к запросу (старый ответ не подсунуть) и ловит
// обрезку/перестановку кадров.

const (
	encHeader     = "X-NW-Enc"
	encTypeHeader = "X-NW-Type"
	encSalt       = "northwind-admin-v1"
	encInfo       = "response"
	frameMaxPlain = 64 << 10
	frameNonce    = 12
	frameTag      = 16
	frameMaxCT    = frameMaxPlain + frameTag
)

var (
	errFrameAuth      = errors.New("admin enc: frame authentication failed")
	errFrameTooLarge  = errors.New("admin enc: frame too large")
	errFrameShort     = errors.New("admin enc: frame too short")
	errNoFinal        = errors.New("admin enc: stream ended without final frame")
	errTrailingData   = errors.New("admin enc: data after final frame")
	errSealerFinished = errors.New("admin enc: write after final frame")
)

// responseKey — HKDF-SHA256(ADMIN_TOKEN, salt, info) → 32 байта AES-256.
func responseKey(token []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, token, []byte(encSalt), encInfo, 32)
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func frameAAD(sig string, idx uint64, final bool) []byte {
	b := make([]byte, 0, len(sig)+24)
	b = append(b, sig...)
	b = append(b, '\n')
	b = strconv.AppendUint(b, idx, 10)
	b = append(b, '\n')
	if final {
		return append(b, '1')
	}
	return append(b, '0')
}

// frameSealer — io.Writer, режущий открытый текст на кадры. Полный кадр
// отправляется только когда пришли следующие данные, поэтому ответ ровно в
// 64 КиБ — один финальный кадр, а не полный + пустой.
type frameSealer struct {
	w     io.Writer
	aead  cipher.AEAD
	sig   string
	nonce io.Reader
	idx   uint64
	buf   []byte
	done  bool
	err   error
}

func newFrameSealer(w io.Writer, key []byte, sig string, nonce io.Reader) (*frameSealer, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if nonce == nil {
		nonce = rand.Reader
	}
	return &frameSealer{w: w, aead: aead, sig: sig, nonce: nonce, buf: make([]byte, 0, frameMaxPlain)}, nil
}

func (s *frameSealer) emit(final bool) error {
	out := make([]byte, 4+frameNonce, 4+frameNonce+len(s.buf)+frameTag)
	nonce := out[4 : 4+frameNonce]
	if _, err := io.ReadFull(s.nonce, nonce); err != nil {
		return err
	}
	out = s.aead.Seal(out, nonce, s.buf, frameAAD(s.sig, s.idx, final))
	binary.BigEndian.PutUint32(out[:4], uint32(len(out)-4-frameNonce))
	s.idx++
	s.buf = s.buf[:0]
	_, err := s.w.Write(out)
	return err
}

func (s *frameSealer) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.done {
		return 0, errSealerFinished
	}
	n := len(p)
	for len(p) > 0 {
		if len(s.buf) == frameMaxPlain {
			if s.err = s.emit(false); s.err != nil {
				return n - len(p), s.err
			}
		}
		k := min(frameMaxPlain-len(s.buf), len(p))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
	}
	return n, nil
}

// Flush — отправить накопленное нефинальным кадром.
func (s *frameSealer) Flush() error {
	if s.err != nil || s.done || len(s.buf) == 0 {
		return s.err
	}
	s.err = s.emit(false)
	return s.err
}

// Close — финальный кадр (с остатком, возможно пустой). Повторный вызов — no-op.
func (s *frameSealer) Close() error {
	if s.err != nil || s.done {
		return s.err
	}
	s.done = true
	s.err = s.emit(true)
	return s.err
}

// frameOpener — обратная сторона: io.Reader открытого текста. Ошибка — если
// кадр не прошёл проверку, больше лимита, поток кончился без финального кадра
// или после финального есть данные.
type frameOpener struct {
	r    io.Reader
	aead cipher.AEAD
	sig  string
	idx  uint64
	buf  []byte // расшифрованный, ещё не отданный текст
	done bool
	err  error
}

func newFrameOpener(r io.Reader, key []byte, sig string) (*frameOpener, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &frameOpener{r: r, aead: aead, sig: sig}, nil
}

func (o *frameOpener) next() error {
	var hdr [4 + frameNonce]byte
	if _, err := io.ReadFull(o.r, hdr[:4]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return errNoFinal
		}
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n > frameMaxCT {
		return errFrameTooLarge
	}
	if n < frameTag {
		return errFrameShort
	}
	ct := make([]byte, int(n))
	if _, err := io.ReadFull(o.r, hdr[4:]); err != nil {
		return errNoFinal
	}
	if _, err := io.ReadFull(o.r, ct); err != nil {
		return errNoFinal
	}
	nonce := hdr[4:]
	// final не передаётся открыто: пробуем оба варианта AAD.
	for _, final := range []bool{false, true} {
		pt, err := o.aead.Open(ct[:0:0], nonce, ct, frameAAD(o.sig, o.idx, final))
		if err != nil {
			continue
		}
		o.idx++
		o.buf = pt
		if final {
			o.done = true
			var one [1]byte
			if k, _ := io.ReadFull(o.r, one[:]); k != 0 {
				return errTrailingData
			}
		}
		return nil
	}
	return errFrameAuth
}

func (o *frameOpener) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		if o.done {
			return 0, io.EOF
		}
		o.err = o.next()
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}

// sealWriter — ResponseWriter, шифрующий тело ответа. Заголовки: X-NW-Enc: 1,
// Content-Type: application/octet-stream, исходный тип — в X-NW-Type,
// Content-Length снимается. Content-Disposition и код ответа остаются открытыми.
type sealWriter struct {
	rw          http.ResponseWriter
	fs          *frameSealer
	wroteHeader bool
}

func (w *sealWriter) Header() http.Header { return w.rw.Header() }

func (w *sealWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	h := w.rw.Header()
	if ct := h.Get("Content-Type"); ct != "" {
		h.Set(encTypeHeader, ct)
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set(encHeader, "1")
	h.Del("Content-Length")
	h.Set("Cache-Control", "no-store")
	w.rw.WriteHeader(code)
}

func (w *sealWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.fs.Write(p)
}

// Flush — накопленное уходит отдельным кадром и дальше по сети.
func (w *sealWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.fs.Flush() == nil {
		if f, ok := w.rw.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// finish — финальный кадр; вызывается после обработчика.
func (w *sealWriter) finish() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.fs.Close()
}

// OpenResponse — расшифровка ответа admin API на стороне клиента (тесты,
// утилиты): token — ADMIN_TOKEN, sig — X-NW-Sig запроса (hex). Ошибка
// проверки кадра, обрезка или лишние данные приходят ошибкой Read.
func OpenResponse(token, sig string, body io.Reader) (io.Reader, error) {
	key, err := responseKey([]byte(token))
	if err != nil {
		return nil, err
	}
	return newFrameOpener(body, key, strings.ToLower(sig))
}
