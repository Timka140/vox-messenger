package files

import (
	"os"
	"sync"
	"time"

	"vox_app/internal/services/files/wire"
)

// uploadState — sender-side track активной отправки одного сообщения. Хранит
// per-peer статус: какие peers уже подтвердили, какие в процессе. Создаётся
// в SendFilesByPath, удаляется когда все peers либо confirmed либо declined.
type uploadState struct {
	mu        sync.Mutex
	msgID     string
	groupID   string                  // "" для DM
	peers     map[string]*peerUpload  // peerID → состояние
	startedAt time.Time
	totalBytes int64
}

type peerUpload struct {
	chunksSent int           // сколько CHUNK_DATA уже отправили этому peer'у
	declined   bool          // peer прислал FILE_DECLINE
	completed  bool          // peer полностью забрал (bitmap.Complete на нашей стороне нет — мы не следим, ориентируемся на closed-loop через ack-future)
	lastReqAt  time.Time     // последний CHUNK_REQUEST от этого peer'а (для timeout-detection)
}

// downloadState — receiver-side track активной загрузки одного файла от
// одного source-peer'а. Привязан к (file_sha, source_peer) — ключ в БД.
//
// Bitmap кешируется в памяти, чтобы не парсить из БД на каждый принятый chunk.
// Persist в БД делается batch'ем (раз в 50 chunks или раз в 2 сек).
type downloadState struct {
	mu          sync.Mutex
	fileSha     string
	sourcePeer  string
	manifest    *wire.FileManifestEntry
	bitmap      *Bitmap
	inFlight    map[int]bool // chunk_idx → отправлен запрос, ждём ответа
	// wake — буфер 1: onChunkData/onChunkNak будят pullLoop сразу, как только
	// в окне освободилось место. Раньше loop опрашивал sleep'ом 50/100 мс.
	wake        chan struct{}
	// deferred — chunk_idx → раньше этого времени не перезапрашивать
	// (после NAK). Без паузы событийный цикл перезапрашивал NAK'нутый чанк
	// каждые ~100 мс — 9844 NAK за пару минут (2026-09-08 15:04).
	deferred map[int]time.Time
	nakCount int // всего NAK по этому download — для give-up
	pendingMsgs map[string]bool // msgID-список (один файл может прийти в несколько msg'ов через forward)
	lastActive  time.Time
	persistTimer *time.Timer
	dirty       bool // true если bitmap изменился, но ещё не записан в БД
	cancel      chan struct{}
	cancelOnce  sync.Once // close(cancel) гоняется между finish/cancel/inactive — закрываем ровно один раз
	nakLogged   bool // true если первый NAK от source уже залогирован на Warn

	// Single-file storage: чанки пишутся напрямую в один файл по offset
	// (idx*chunkSize). На complete .vox-partial → rename в finalPath.
	partialFile *os.File // открытый дескриптор .vox-partial для WriteAt
	partialPath string   // финальный путь + ".vox-partial"
	finalPath   string   // целевой путь в Vox Downloads
}

// wakePull будит pullLoop (неблокирующе). Вызывать после любого изменения
// inFlight/bitmap, которое может открыть место в окне.
func (st *downloadState) wakePull() {
	if st.wake == nil {
		return
	}
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// closeCancel закрывает st.cancel ровно один раз. Терминальные пути
// (finishDownload, CancelFileDownload, markStateInactive) могут гоняться
// друг с другом — прямой close даёт panic: close of closed channel.
// Заодно останавливает отложенный persistTimer, чтобы он не откатил
// финальный статус в БД обратно в "active".
func (st *downloadState) closeCancel() {
	st.mu.Lock()
	if st.persistTimer != nil {
		st.persistTimer.Stop()
		st.persistTimer = nil
	}
	st.mu.Unlock()
	st.cancelOnce.Do(func() { close(st.cancel) })
}
