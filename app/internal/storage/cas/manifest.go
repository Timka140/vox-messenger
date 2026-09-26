package cas

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FileManifest — описание файла и его chunks. Этот же объект сериализуется в
// `file_manifest`-фрейме и отправляется sender'ом receiver'у (поля Source/
// OriginSize/OriginMtime — sender-only, в БД, по wire не передаются).
type FileManifest struct {
	Sha        string   `json:"sha"`        // file_sha = ManifestHash
	Size       int64    `json:"size"`       // суммарный plaintext
	ChunkSize  int      `json:"chunkSize"`  // plaintext per chunk (одинаковый для всех кроме последнего)
	ChunkCount int      `json:"chunkCount"` // == len(Chunks)
	Chunks     []string `json:"chunks"`     // chunk_sha[] в порядке файла
	Mime       string   `json:"mime,omitempty"`
	Width      int      `json:"width,omitempty"`
	Height     int      `json:"height,omitempty"`

	// Sender-only метаданные. По wire не сериализуются (json:"-"), receiver
	// всегда видит Source="" → пишет 'cas' через дефолт колонки.
	Source       string `json:"-"` // "cas" (default) или "streaming"
	OriginSize   int64  `json:"-"` // ожидаемый Stat().Size() оригинала
	OriginMtime  int64  `json:"-"` // ожидаемый Stat().ModTime().Unix()
}

// ManifestHash — детерминированный sha файла на основе chunks + meta.
//
//	sha256( chunk_sha[0] || chunk_sha[1] || ... || chunk_sha[N] ||
//	        size (8B BE) || chunk_size (4B BE) || mime )
//
// Не Merkle-tree, а hash-chain — для MVP достаточно: integrity всего набора
// chunks + интеграл с meta. Per-chunk integrity даёт сам chunk_sha при verify
// в receiver'е (sha256(received_payload) == manifest.Chunks[idx]).
func ManifestHash(chunks []string, size int64, chunkSize int, mime string) string {
	h := sha256.New()
	for _, c := range chunks {
		// Пишем raw bytes (32 байта на chunk) а не hex-строку — короче и
		// эквивалентно по entropy.
		raw, err := hex.DecodeString(c)
		if err == nil && len(raw) == 32 {
			h.Write(raw)
		} else {
			h.Write([]byte(c))
		}
	}
	var sizeBuf [8]byte
	beUint64(sizeBuf[:], uint64(size))
	h.Write(sizeBuf[:])
	var csBuf [4]byte
	beUint32(csBuf[:], uint32(chunkSize))
	h.Write(csBuf[:])
	h.Write([]byte(mime))
	return hex.EncodeToString(h.Sum(nil))
}

func beUint64(buf []byte, v uint64) {
	buf[0] = byte(v >> 56)
	buf[1] = byte(v >> 48)
	buf[2] = byte(v >> 40)
	buf[3] = byte(v >> 32)
	buf[4] = byte(v >> 24)
	buf[5] = byte(v >> 16)
	buf[6] = byte(v >> 8)
	buf[7] = byte(v)
}
func beUint32(buf []byte, v uint32) {
	buf[0] = byte(v >> 24)
	buf[1] = byte(v >> 16)
	buf[2] = byte(v >> 8)
	buf[3] = byte(v)
}

// Лимиты на входящий (недоверенный) манифест. sha самосогласован для любых
// полей, поэтому геометрию файла нужно валидировать явно — иначе sender мог
// объявить маленький Size (проходит auto-pull-гейт), но огромный
// ChunkCount×ChunkSize: receiver считает offset = idx*ChunkSize и через WriteAt
// пишет многогигабайтный sparse-файл без согласия пользователя (disk-fill DoS).
const (
	MaxManifestChunkSize  = 16 * 1024 * 1024        // 16 MiB на chunk
	MaxManifestFileSize   = int64(64) << 30         // 64 GiB на файл
	MaxManifestChunkCount = 4 * 1024 * 1024         // 4M chunks (= 64 GiB при 16 KiB chunk)
)

// VerifyManifest — пересчёт hash'а от данных манифеста и сравнение с m.Sha.
// Защищает receiver от incoming manifest где sender mismatched fields.
func (m *FileManifest) Verify() error {
	want := ManifestHash(m.Chunks, m.Size, m.ChunkSize, m.Mime)
	if want != m.Sha {
		return fmt.Errorf("manifest verify: sha mismatch (want %s got %s)", want, m.Sha)
	}
	if m.ChunkCount != len(m.Chunks) {
		return fmt.Errorf("manifest verify: chunk_count=%d vs chunks=%d", m.ChunkCount, len(m.Chunks))
	}
	if m.Size <= 0 || m.ChunkSize <= 0 || m.ChunkCount <= 0 {
		return fmt.Errorf("manifest verify: bad size/chunk_size/chunk_count")
	}
	if m.ChunkSize > MaxManifestChunkSize {
		return fmt.Errorf("manifest verify: chunk_size %d превышает лимит %d", m.ChunkSize, MaxManifestChunkSize)
	}
	if m.ChunkCount > MaxManifestChunkCount {
		return fmt.Errorf("manifest verify: chunk_count %d превышает лимит %d", m.ChunkCount, MaxManifestChunkCount)
	}
	if m.Size > MaxManifestFileSize {
		return fmt.Errorf("manifest verify: size %d превышает лимит %d", m.Size, MaxManifestFileSize)
	}
	// Геометрия: последний chunk непустой и не длиннее ChunkSize, т.е.
	//   (ChunkCount-1)*ChunkSize < Size <= ChunkCount*ChunkSize.
	// int64 арифметика — оба операнда уже в пределах лимитов, переполнения нет.
	maxBytes := int64(m.ChunkCount) * int64(m.ChunkSize)
	minBytes := int64(m.ChunkCount-1) * int64(m.ChunkSize)
	if m.Size > maxBytes || m.Size <= minBytes {
		return fmt.Errorf("manifest verify: size %d не соответствует chunk_count=%d × chunk_size=%d",
			m.Size, m.ChunkCount, m.ChunkSize)
	}
	return nil
}

// ──── Manifest persistence (SQLite) ────────────────────────────────────────
//
// Все методы работают с одной *sql.DB. CAS-store не зависит от БД для
// собственно chunks — БД нужна только для files/file_chunks/chunk_blobs/
// file_downloads. Чтобы сохранить разделение, методы Manifest* принимают
// *sql.DB параметром.

// SaveManifest идемпотентно записывает manifest в files + file_chunks.
// Refcount файла не трогается — это делает caller (отдельный INSERT в
// message_files INC'ит refcount файла + его chunks).
func SaveManifest(db *sql.DB, m *FileManifest) error {
	const op = "cas.SaveManifest"
	if err := m.Verify(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer tx.Rollback()

	// Idempotency: если file row уже есть — skip insert. Chunks row тоже могут
	// быть — INSERT OR IGNORE.
	source := m.Source
	if source == "" {
		source = "cas"
	}
	var existingSha string
	err = tx.QueryRow(`SELECT sha FROM files WHERE sha = ?`, m.Sha).Scan(&existingSha)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec(
			`INSERT INTO files (sha, size, chunk_size, chunk_count, mime, width, height, refcount, created_at, source, origin_size, origin_mtime)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			m.Sha, m.Size, m.ChunkSize, m.ChunkCount, m.Mime, m.Width, m.Height, time.Now().Unix(),
			source, m.OriginSize, m.OriginMtime); err != nil {
			return fmt.Errorf("%s: insert files: %w", op, err)
		}
		for i, csha := range m.Chunks {
			if _, err := tx.Exec(
				`INSERT INTO file_chunks (file_sha, chunk_idx, chunk_sha) VALUES (?, ?, ?)`,
				m.Sha, i, csha); err != nil {
				return fmt.Errorf("%s: insert chunk: %w", op, err)
			}
		}
	} else if err != nil {
		return fmt.Errorf("%s: select: %w", op, err)
	} else if source == "streaming" {
		// Row уже есть (тот же sha прислали/отправили раньше), а теперь sender
		// готовит ОТПРАВКУ из файла по новому пути: caller следом переключит
		// local_path на него, и invariant-guard seeder'а (size+mtime) должен
		// соответствовать именно этому файлу. Раньше origin_* оставались от
		// старого файла → каждый chunk_request получал NAK «mtime mismatch»
		// (2026-09-08: копия speedtest с тем же sha, 9844 NAK подряд).
		if _, err := tx.Exec(
			`UPDATE files SET source = ?, origin_size = ?, origin_mtime = ? WHERE sha = ?`,
			source, m.OriginSize, m.OriginMtime, m.Sha); err != nil {
			return fmt.Errorf("%s: update origin: %w", op, err)
		}
	}
	return tx.Commit()
}

// LoadManifest — обратная операция. Используется sender'ом перед отправкой,
// receiver'ом для verify входящих chunks, asset bridge для отдачи vox-attach.
// Source/OriginSize/OriginMtime — sender-only поля для streaming-режима.
func LoadManifest(db *sql.DB, fileSha string) (*FileManifest, error) {
	const op = "cas.LoadManifest"
	m := &FileManifest{Sha: fileSha}
	err := db.QueryRow(
		`SELECT size, chunk_size, chunk_count, mime, width, height, source, origin_size, origin_mtime FROM files WHERE sha = ?`,
		fileSha).Scan(&m.Size, &m.ChunkSize, &m.ChunkCount, &m.Mime, &m.Width, &m.Height, &m.Source, &m.OriginSize, &m.OriginMtime)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%s: file not found", op)
		}
		return nil, fmt.Errorf("%s: select files: %w", op, err)
	}
	rows, err := db.Query(
		`SELECT chunk_sha FROM file_chunks WHERE file_sha = ? ORDER BY chunk_idx ASC`, fileSha)
	if err != nil {
		return nil, fmt.Errorf("%s: select chunks: %w", op, err)
	}
	defer rows.Close()
	chunks := make([]string, 0, m.ChunkCount)
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("%s: scan chunk: %w", op, err)
		}
		chunks = append(chunks, c)
	}
	m.Chunks = chunks
	return m, nil
}

// IncFileRefcount — атомарно +1 для files.refcount. В single-file модели
// chunk_blobs больше не используется — чанки не лежат на диске как отдельные
// блобы, source of truth = local_path в Vox Downloads.
func IncFileRefcount(db *sql.DB, fileSha string) error {
	const op = "cas.IncFileRefcount"
	res, err := db.Exec(
		`UPDATE files SET refcount = refcount + 1 WHERE sha = ?`, fileSha)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%s: file %s not found", op, fileSha[:8])
	}
	return nil
}

// DecFileRefcount — обратная IncFileRefcount. Если refcount достиг 0 —
// удаляем row из files и file_chunks, возвращаем dropped=true (caller удаляет
// local-файл, если он внутри Vox Downloads).
func DecFileRefcount(db *sql.DB, fileSha string) (dropped bool, err error) {
	const op = "cas.DecFileRefcount"
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer tx.Rollback()

	var rc int
	err = tx.QueryRow(`SELECT refcount FROM files WHERE sha = ?`, fileSha).Scan(&rc)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // already gone
	}
	if err != nil {
		return false, fmt.Errorf("%s: select: %w", op, err)
	}

	if rc > 1 {
		if _, err := tx.Exec(`UPDATE files SET refcount = refcount - 1 WHERE sha = ?`, fileSha); err != nil {
			return false, fmt.Errorf("%s: dec files: %w", op, err)
		}
		return false, tx.Commit()
	}

	// Refcount достиг 0 → удаляем row файла и его chunk-sha список.
	if _, err := tx.Exec(`DELETE FROM file_chunks WHERE file_sha = ?`, fileSha); err != nil {
		return false, fmt.Errorf("%s: delete file_chunks: %w", op, err)
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE sha = ?`, fileSha); err != nil {
		return false, fmt.Errorf("%s: delete files: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("%s: commit: %w", op, err)
	}
	return true, nil
}

// SetFileLocalPath — обновляет files.local_path. Используется sender'ом
// (после prepare — путь к оригиналу) и receiver'ом (после assembleAndStore —
// путь в Vox Downloads). Передача "" означает «нет локального файла»
// (например после удаления извне или resharing-кэш).
func SetFileLocalPath(db *sql.DB, sha, path string) error {
	_, err := db.Exec(`UPDATE files SET local_path = ? WHERE sha = ?`, path, sha)
	return err
}

// GetFileLocalPath — возвращает текущий путь или "" если row нет.
func GetFileLocalPath(db *sql.DB, sha string) string {
	var p string
	_ = db.QueryRow(`SELECT local_path FROM files WHERE sha = ?`, sha).Scan(&p)
	return p
}

// safeShortSha — для логов, чтобы не светить полную хешу.
func safeShortSha(sha string) string {
	if len(sha) < 8 {
		return sha
	}
	return strings.ToLower(sha[:8])
}

var _ = safeShortSha // экспорт для локальных пакетов upload/download (linter)
