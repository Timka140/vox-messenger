package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// BlobStore хранит attachment-blob'ы на диске с AES-GCM шифрованием at-rest.
//
// Формат файла (chunked, v1 — "VXB1"):
//
//	bytes 0..3     magic "VXB1"
//	byte  4        version = 1
//	bytes 5..7     reserved (zero)
//	bytes 8..11    chunkSize (plaintext bytes per chunk), little-endian uint32
//	bytes 12..     repeated { nonce[12] || ciphertext_with_tag }, пока не EOF.
//	               Каждый чанк — plaintext не больше chunkSize байт (последний
//	               меньше). AAD для каждого чанка = header[:12] || uint64(idx LE).
//	               AAD защищает от reorder/truncation attack'ов.
//
// Dedup:   путь = sha256(plaintext); один и тот же файл лежит ровно один раз.
//          Refcount-учёт делает caller (через таблицу attachments в БД).
//
// Backward-compat чтение: если первые 4 байта НЕ "VXB1", файл читается как
// legacy-формат — nonce(12) || seal(весь_plaintext). OpenReader в этом случае
// расшифровывает всё сразу в память и отдаёт через bytes.Reader — OK только
// потому что legacy-файлы малы (до 100 MB) и в продакшене их практически нет.
//
// Ключ шифрования — копия dbKey (32 байта). Файлы Vox отдельно от vox.db
// в незашифрованной папке вызывали бы вопросы при изъятии диска; шифруя
// тем же ключом мы сохраняем единое симметричное состояние "БД залочена
// → файлы недоступны".
type BlobStore struct {
	root string      // <dataDir>/attachments
	aead cipher.AEAD // AES-256-GCM, ключ из dbKey
}

const (
	blobMagic        = "VXB1"
	blobHeaderLen    = 12
	blobChunkPlain   = 1 << 20 // 1 MiB plaintext per chunk
	blobVersion byte = 1
)

// NewBlobStore инициализирует хранилище: создаёт папку attachments/ и AEAD.
// dbKey — 32 байта из identity.LoadOrCreateDBKey.
func NewBlobStore(dataDir string, dbKey []byte) (*BlobStore, error) {
	const op = "storage.NewBlobStore"

	if len(dbKey) != 32 {
		return nil, fmt.Errorf("%s: dbKey must be 32 bytes, got %d", op, len(dbKey))
	}

	root := filepath.Join(dataDir, "attachments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("%s: mkdir: %w", op, err)
	}

	block, err := aes.NewCipher(dbKey)
	if err != nil {
		return nil, fmt.Errorf("%s: cipher: %w", op, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%s: gcm: %w", op, err)
	}

	return &BlobStore{root: root, aead: aead}, nil
}

// --- Convenience API (сохраняет старый контракт) ---

// Put шифрует и записывает blob целиком; возвращает sha256 plaintext в hex.
// Внутри использует streaming Writer — поэтому память ~1 MiB даже для больших
// буферов. Для очень больших файлов лучше PutFromReader / Writer напрямую.
func (b *BlobStore) Put(plaintext []byte) (string, error) {
	w, err := b.NewWriter()
	if err != nil {
		return "", err
	}
	if _, err := w.Write(plaintext); err != nil {
		_ = w.Abort()
		return "", err
	}
	return w.Finalize()
}

// PutFromReader стримит r целиком в blob, возвращает sha256. Caller обязан
// гарантировать конечность r.
func (b *BlobStore) PutFromReader(r io.Reader) (string, error) {
	w, err := b.NewWriter()
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Abort()
		return "", err
	}
	return w.Finalize()
}

// Get расшифровывает и возвращает plaintext целиком. Для больших файлов
// используй OpenReader — Get держит всё в RAM.
func (b *BlobStore) Get(hash string) ([]byte, error) {
	rc, err := b.OpenReader(hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Delete удаляет blob с диска. Caller гарантирует что refcount=0.
// Не-существующий blob не считается ошибкой.
func (b *BlobStore) Delete(hash string) error {
	const op = "BlobStore.Delete"
	path := b.pathFor(hash)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// Exists проверяет наличие файла на диске.
func (b *BlobStore) Exists(hash string) bool {
	_, err := os.Stat(b.pathFor(hash))
	return err == nil
}

// PathRel — относительный путь (для хранения в БД).
func (b *BlobStore) PathRel(hash string) string {
	if len(hash) < 2 {
		return hash
	}
	return filepath.ToSlash(filepath.Join(hash[:2], hash+".bin"))
}

// Root — абсолютный путь корневой директории attachment'ов.
// Receiver может использовать для placement временных файлов.
func (b *BlobStore) Root() string {
	return b.root
}

// pathFor — абсолютный путь к blob'у.
func (b *BlobStore) pathFor(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(b.root, hash+".bin")
	}
	return filepath.Join(b.root, hash[:2], hash+".bin")
}

// HashOf утилита: sha256 hex.
func HashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ErrBlobNotFound — sentinel для проверок наличия.
var ErrBlobNotFound = errors.New("blob not found")

// --- Writer ---

// BlobWriter стримит plaintext → encrypted chunks. После Finalize blob лежит
// в каноническом пути. Не потокобезопасен: один писатель — один caller.
type BlobWriter struct {
	bs        *BlobStore
	tmp       *os.File
	tmpPath   string
	hasher    hash.Hash
	header    [blobHeaderLen]byte
	chunkSize int
	buf       []byte // plaintext staging (cap=chunkSize)
	chunkIdx  uint64
	closed    bool // true после Finalize/Abort — любой Write ошибка
}

// NewWriter создаёт tmp-файл в Root и записывает header.
func (b *BlobStore) NewWriter() (*BlobWriter, error) {
	const op = "BlobStore.NewWriter"

	if err := os.MkdirAll(b.root, 0o700); err != nil {
		return nil, fmt.Errorf("%s: mkdir root: %w", op, err)
	}

	tmp, err := os.CreateTemp(b.root, "blob-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("%s: create tmp: %w", op, err)
	}

	w := &BlobWriter{
		bs:        b,
		tmp:       tmp,
		tmpPath:   tmp.Name(),
		hasher:    sha256.New(),
		chunkSize: blobChunkPlain,
		buf:       make([]byte, 0, blobChunkPlain),
	}
	copy(w.header[:4], []byte(blobMagic))
	w.header[4] = blobVersion
	// header[5..7] reserved
	binary.LittleEndian.PutUint32(w.header[8:12], uint32(w.chunkSize))

	if _, err := tmp.Write(w.header[:]); err != nil {
		_ = tmp.Close()
		_ = os.Remove(w.tmpPath)
		return nil, fmt.Errorf("%s: write header: %w", op, err)
	}
	return w, nil
}

// Write аккумулирует p в chunk-буфер и flush'ит когда буфер ≥ chunkSize.
func (w *BlobWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("BlobWriter: closed")
	}
	written := 0
	for len(p) > 0 {
		space := w.chunkSize - len(w.buf)
		take := len(p)
		if take > space {
			take = space
		}
		w.buf = append(w.buf, p[:take]...)
		w.hasher.Write(p[:take])
		written += take
		p = p[take:]
		if len(w.buf) == w.chunkSize {
			if err := w.flushChunk(false); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flushChunk шифрует и пишет текущий buf как один chunk. Если tail=true,
// допускает размер меньше chunkSize (последний чанк). Если buf пуст и !tail,
// no-op; если buf пуст и tail=true, chunk не пишется (файл может оканчиваться
// на полном чанке).
func (w *BlobWriter) flushChunk(tail bool) error {
	if len(w.buf) == 0 {
		return nil
	}
	nonce := make([]byte, w.bs.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	aad := aadFor(w.header, w.chunkIdx)
	// Seal: append nonce первым, затем ciphertext — за один write в файл.
	out := make([]byte, 0, len(nonce)+len(w.buf)+w.bs.aead.Overhead())
	out = append(out, nonce...)
	out = w.bs.aead.Seal(out, nonce, w.buf, aad)
	if _, err := w.tmp.Write(out); err != nil {
		return fmt.Errorf("write chunk: %w", err)
	}
	w.chunkIdx++
	w.buf = w.buf[:0]
	_ = tail
	return nil
}

// Finalize дописывает хвостовой чанк, fsync, close, rename в канонический путь.
// Возвращает sha256 hex. После Finalize writer нельзя использовать.
func (w *BlobWriter) Finalize() (string, error) {
	const op = "BlobWriter.Finalize"
	if w.closed {
		return "", errors.New(op + ": already closed")
	}
	w.closed = true

	if err := w.flushChunk(true); err != nil {
		_ = w.tmp.Close()
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%s: flush tail: %w", op, err)
	}
	if err := w.tmp.Sync(); err != nil {
		_ = w.tmp.Close()
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%s: fsync: %w", op, err)
	}
	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%s: close: %w", op, err)
	}

	sha := hex.EncodeToString(w.hasher.Sum(nil))
	finalPath := w.bs.pathFor(sha)

	// Идемпотентность: если blob уже есть на диске — выиграли гонку, наш tmp не
	// нужен. Мы не можем rename'ом перезаписать существующий файл кросс-платформенно
	// (Windows ENOFOLLOW), да и не должны — существующий файл уже проверен AEAD.
	if _, err := os.Stat(finalPath); err == nil {
		_ = os.Remove(w.tmpPath)
		return sha, nil
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%s: mkdir: %w", op, err)
	}
	if err := os.Rename(w.tmpPath, finalPath); err != nil {
		// На Windows Rename иногда не может "перепрыгнуть" уже существующий файл
		// — считаем это успехом гонки (см. Stat выше, race window).
		if _, sterr := os.Stat(finalPath); sterr == nil {
			_ = os.Remove(w.tmpPath)
			return sha, nil
		}
		_ = os.Remove(w.tmpPath)
		return "", fmt.Errorf("%s: rename: %w", op, err)
	}
	return sha, nil
}

// Abort закрывает tmp и удаляет его. Безопасно вызывать повторно.
func (w *BlobWriter) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.tmp.Close()
	return os.Remove(w.tmpPath)
}

// --- Reader ---

// OpenReader открывает blob для streaming-чтения с начала. Автоопределяет
// формат (VXB1 chunked vs legacy nonce+seal). Эквивалент OpenReaderAt(hash, 0).
func (b *BlobStore) OpenReader(hash string) (io.ReadCloser, error) {
	return b.OpenReaderAt(hash, 0)
}

// OpenReaderAt открывает blob для streaming-чтения с указанного plaintext-offset.
// Используется для resume чанковой передачи: fileOffset = headerLen + targetChunk *
// (nonceSize + chunkSize + tagSize); внутри chunk'а пропускаем offset % chunkSize.
//
// Для legacy-формата seek в зашифрованных данных невозможен — расшифровываем
// весь файл и отдаём срез от offset. Legacy-блобы малы, OOM не страшен.
func (b *BlobStore) OpenReaderAt(hash string, offset int64) (io.ReadCloser, error) {
	const op = "BlobStore.OpenReaderAt"
	if offset < 0 {
		return nil, fmt.Errorf("%s: negative offset %d", op, offset)
	}
	path := b.pathFor(hash)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", op, ErrBlobNotFound)
		}
		return nil, fmt.Errorf("%s: open: %w", op, err)
	}

	var header [blobHeaderLen]byte
	n, rerr := io.ReadFull(f, header[:])
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		_ = f.Close()
		return nil, fmt.Errorf("%s: read header: %w", op, rerr)
	}
	if n < 4 || string(header[:4]) != blobMagic {
		// Legacy: nonce + seal(whole). Расшифровываем целиком, отдаём срез.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s: rewind: %w", op, err)
		}
		enc, rerr := io.ReadAll(f)
		_ = f.Close()
		if rerr != nil {
			return nil, fmt.Errorf("%s: legacy read: %w", op, rerr)
		}
		ns := b.aead.NonceSize()
		if len(enc) < ns+b.aead.Overhead() {
			return nil, fmt.Errorf("%s: legacy too short", op)
		}
		pt, err := b.aead.Open(nil, enc[:ns], enc[ns:], nil)
		if err != nil {
			return nil, fmt.Errorf("%s: legacy decrypt: %w", op, err)
		}
		if offset > int64(len(pt)) {
			return nil, fmt.Errorf("%s: offset %d > legacy size %d", op, offset, len(pt))
		}
		return &legacyReader{buf: pt[offset:]}, nil
	}
	if header[4] != blobVersion {
		_ = f.Close()
		return nil, fmt.Errorf("%s: unsupported version %d", op, header[4])
	}
	chunkSize := int(binary.LittleEndian.Uint32(header[8:12]))
	if chunkSize <= 0 || chunkSize > 16<<20 {
		_ = f.Close()
		return nil, fmt.Errorf("%s: bad chunkSize %d", op, chunkSize)
	}

	// Прыжок к нужному chunk'у. Каждый non-tail chunk занимает ровно
	// nonceSize + chunkSize + tagSize байт. Tail-chunk короче, но прыгать
	// в его середину мы не пытаемся — caller всегда указывает offset на границе
	// chunk'а или внутри tail.
	targetChunk := uint64(offset / int64(chunkSize))
	intraChunk := int(offset % int64(chunkSize))
	if targetChunk > 0 {
		nonceSize := int64(b.aead.NonceSize())
		tagSize := int64(b.aead.Overhead())
		perChunk := nonceSize + int64(chunkSize) + tagSize
		fileOff := int64(blobHeaderLen) + int64(targetChunk)*perChunk
		if _, err := f.Seek(fileOff, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s: seek chunk %d: %w", op, targetChunk, err)
		}
	}
	return &chunkedReader{
		bs:        b,
		f:         f,
		header:    header,
		chunkSize: chunkSize,
		chunkIdx:  targetChunk,
		skipFirst: intraChunk,
	}, nil
}

type legacyReader struct {
	buf []byte
	pos int
}

func (r *legacyReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

func (r *legacyReader) Close() error {
	r.buf = nil
	return nil
}

type chunkedReader struct {
	bs        *BlobStore
	f         *os.File
	header    [blobHeaderLen]byte
	chunkSize int
	chunkIdx  uint64
	plain     []byte // decrypted chunk waiting to be consumed
	plainPos  int
	skipFirst int  // bytes to drop at the start of the very first decrypted chunk (resume)
	eof       bool
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.plainPos >= len(r.plain) {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.loadChunk(); err != nil {
			return 0, err
		}
		if r.plainPos >= len(r.plain) {
			return 0, io.EOF
		}
	}
	n := copy(p, r.plain[r.plainPos:])
	r.plainPos += n
	return n, nil
}

func (r *chunkedReader) loadChunk() error {
	ns := r.bs.aead.NonceSize()
	nonce := make([]byte, ns)
	if _, err := io.ReadFull(r.f, nonce); err != nil {
		if errors.Is(err, io.EOF) {
			r.eof = true
			r.plain = r.plain[:0]
			r.plainPos = 0
			return nil
		}
		return fmt.Errorf("read nonce: %w", err)
	}
	// Читаем ciphertext: до chunkSize + tag. Для последнего chunk'а меньше.
	maxCt := r.chunkSize + r.bs.aead.Overhead()
	ctBuf := make([]byte, maxCt)
	m, err := io.ReadFull(r.f, ctBuf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("read ct: %w", err)
	}
	ctBuf = ctBuf[:m]

	aad := aadFor(r.header, r.chunkIdx)
	pt, err := r.bs.aead.Open(nil, nonce, ctBuf, aad)
	if err != nil {
		return fmt.Errorf("decrypt chunk %d: %w", r.chunkIdx, err)
	}
	r.plain = pt
	r.plainPos = 0
	if r.skipFirst > 0 {
		if r.skipFirst >= len(pt) {
			// Указанный resume-offset попал в самый конец последнего chunk'а
			// или вышел за его границу — отдаём пустой буфер, EOF на следующем Read.
			r.plainPos = len(pt)
		} else {
			r.plainPos = r.skipFirst
		}
		r.skipFirst = 0
	}
	r.chunkIdx++
	// Если расшифрованный чанк короче chunkSize — это последний чанк.
	if len(pt) < r.chunkSize {
		r.eof = true
	}
	return nil
}

func (r *chunkedReader) Close() error {
	r.plain = nil
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// aadFor — AAD для чанка: первые 12 байт header'а + uint64 idx LE. Защищает
// от reorder и truncation (каждый chunk завязан на свой порядковый номер).
func aadFor(header [blobHeaderLen]byte, idx uint64) []byte {
	aad := make([]byte, blobHeaderLen+8)
	copy(aad, header[:])
	binary.LittleEndian.PutUint64(aad[blobHeaderLen:], idx)
	return aad
}
