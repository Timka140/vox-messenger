// Package cas — chunk-addressable метаданные для файлов.
//
// Идея: файл разбивается на chunks фиксированного размера (по дефолту 64 KiB
// plaintext). Каждый chunk адресуется sha256(plaintext); список этих хешей
// хранится в БД (`file_chunks`). file_sha = sha256(concat(chunk_shas)).
//
// Single-file модель (передача файлов):
//   - Receiver: чанки пишутся напрямую через WriteAt в .vox-tmp/<file>.partial,
//     по complete делается atomic os.Rename в Vox Downloads.
//   - Sender: всегда читает оригинал (или собранный файл) по offset через
//     ReadChunkFromFile и хеширует на лету для verify.
//
// Преимущества над монолитным BlobStore:
//   - Resume: receiver знает свои bitmap → запрашивает только missing chunks
//   - Verify per-chunk: битый chunk виден сразу, не после Finalize 40 GB файла
//   - Swarm: receiver может качать разные chunks у разных peers параллельно
//
// БД-сторона (см. storage/migrate.go):
//
//	files          — file_sha, size, chunk_size, chunk_count, mime, dims, refcount, local_path, source/origin_size/origin_mtime
//	file_chunks    — file_sha → ordered chunk_sha[] (для verify входящих чанков)
//	file_downloads — receiver-side bitmap прогресса + partial_path (для resume)
//
// История: раньше пакет включал Store с шифрованием at-rest и chunk_blobs-
// табличкой для refcount blob'ов на диске; в single-file модели всё это не
// нужно — данные живут в одном файле в Vox Downloads, без дополнительных
// копий и refcount-арифметики на уровне chunks.
package cas

import "errors"

// DefaultChunkSize — 64 KiB plaintext. Маленький chunk нужен чтобы fine-grained
// interleaving с другими сообщениями на attachments DC: при 1 MiB один chunk
// дренится 100-200 ms даже на хорошей связи, и любой запрос/ack за ним стоит
// в очереди. 64 KiB дренится <50 ms.
//
// Файлы шлются по отдельному PeerConnection (FilesPC) — voice не страдает.
// Но resume granularity и manifest size зависят именно от chunk size: на 1 GB
// файл = 16384 chunks, на 40 GB ≈ 650k. Всё ещё терпимо для SQLite.
const DefaultChunkSize = 64 * 1024

// ErrChunkNotFound — возвращается когда chunk не удалось получить (файл утерян
// или sha не совпала). Сохранилось для legacy сигнатур.
var ErrChunkNotFound = errors.New("chunk not found")
