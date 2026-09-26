package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// HashOf — sha256 hex от plaintext'а chunk'а.
func HashOf(plaintext []byte) string {
	sum := sha256.Sum256(plaintext)
	return hex.EncodeToString(sum[:])
}

// HashFileForManifest читает файл chunk-by-chunk и считает sha каждого чанка.
// Используется sender'ом: подготавливает manifest без копирования данных
// куда-либо ещё — чанки потом отдаются seeder'ом из этого же файла по offset
// (см. ReadChunkFromFile).
//
// Если файл больше maxBytes — обрывает с ошибкой (защита от случайной заливки).
func HashFileForManifest(path string, chunkSize int, maxBytes int64) (chunkShas []string, totalBytes int64, err error) {
	const op = "cas.HashFileForManifest"
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	f, ferr := os.Open(path)
	if ferr != nil {
		return nil, 0, fmt.Errorf("%s: open: %w", op, ferr)
	}
	defer f.Close()
	buf := make([]byte, chunkSize)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if maxBytes > 0 && totalBytes+int64(n) > maxBytes {
				return nil, totalBytes, fmt.Errorf("%s: file exceeds maxBytes=%d", op, maxBytes)
			}
			chunkShas = append(chunkShas, HashOf(buf[:n]))
			totalBytes += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, totalBytes, fmt.Errorf("%s: read: %w", op, rerr)
		}
	}
	return chunkShas, totalBytes, nil
}

// ReadChunkFromFile читает один chunk из файла по offset = idx*chunkSize.
// Используется seeder'ом при ответе на CHUNK_REQUEST: и для streaming-режима
// (sender отдаёт чанки оригинала юзера), и для CAS-fallback (sender пере-
// отправляет когда-то полученный файл из своего Vox Downloads).
//
// Invariant guard: если expectSize/expectMtime > 0 — сверяет fi.Size() и
// fi.ModTime().Unix() с ожидаемыми. Не совпали — ошибка, caller отдаёт NAK.
// Передача 0/0 отключает guard (caller сам сверит sha-чанка с manifest'ом).
func ReadChunkFromFile(path string, idx, chunkSize int, expectSize, expectMtime int64) ([]byte, error) {
	const op = "cas.ReadChunkFromFile"
	if chunkSize <= 0 {
		return nil, fmt.Errorf("%s: bad chunkSize", op)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: open: %w", op, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: stat: %w", op, err)
	}
	if expectSize > 0 && fi.Size() != expectSize {
		return nil, fmt.Errorf("%s: size mismatch (expect %d got %d)", op, expectSize, fi.Size())
	}
	if expectMtime > 0 && fi.ModTime().Unix() != expectMtime {
		return nil, fmt.Errorf("%s: mtime mismatch (expect %d got %d)", op, expectMtime, fi.ModTime().Unix())
	}
	offset := int64(idx) * int64(chunkSize)
	if offset >= fi.Size() {
		return nil, fmt.Errorf("%s: idx %d past EOF (offset %d size %d)", op, idx, offset, fi.Size())
	}
	want := chunkSize
	if remaining := fi.Size() - offset; int64(want) > remaining {
		want = int(remaining)
	}
	buf := make([]byte, want)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: read: %w", op, err)
	}
	return buf, nil
}
