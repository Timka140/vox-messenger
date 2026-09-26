// Package files — chunk-addressable передача файлов поверх P2P DataChannel.
//
// Архитектура:
//
//	Sender (upload):
//	  1. PrepareAttachments → юзер выбирает в picker → SendFilesByPath
//	  2. Для каждого файла: stream с диска → CAS (chunk-by-chunk) → manifest
//	  3. INSERT messages + message_files
//	  4. Для каждого online member: encode FILE_MANIFEST → отправить через
//	     "attachments" DC (бинарный)
//	  5. На входящие CHUNK_REQUEST → читаем chunks из CAS, отвечаем CHUNK_DATA.
//
//	Receiver (download):
//	  1. Принимаем FILE_MANIFEST → SaveManifest + INSERT messages + message_files
//	  2. INSERT file_downloads (per-file, per-source) с empty bitmap
//	  3. Pull-loop: missing chunks → CHUNK_REQUEST → получаем CHUNK_DATA →
//	     verify sha → CAS.PutChunk → set bit → persist bitmap
//	  4. При полноте: emit vox:file-received, юзер видит готовый bubble
//
//	При reconnect: при старте смотрим file_downloads.status='active' → restart pull.
//
// Файлы пакета:
//
//	service.go         — этот файл, FilesService struct + Wails-методы
//	bitmap.go          — bitmap для received chunks
//	upload.go          — sender pipeline
//	download.go        — receiver pull-loop
//	seeder.go          — отвечает на входящие CHUNK_REQUEST
//	dispatch.go        — диспатч входящих 0x10..0x14 фреймов
//	state.go           — внутренние in-memory структуры активных передач
//	events.go          — emit'ы для UI
package files

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"sync/atomic"

	"vox_app/internal/session"
)

// SendResult — то что фронт получает обратно от SendFilesByPath. Совместимо
// с существующим services.SendResult чтобы upload-state на фронте работал.
type SendResult struct {
	ID        string `json:"id"`
	Delivered bool   `json:"delivered"`
}

// FileItem — один файл в SendFilesByPath. Совместимо с AttachByPathInput.
type FileItem struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Compress bool   `json:"compress"`
}

// NetworkOps — узкий интерфейс для отправки. Реальная реализация —
// services.NetworkService (свернули чтобы не тащить весь network в files).
type NetworkOps interface {
	IsP2PConnected(peerID string) bool
	IsAttachDCOpen(peerID string) bool
	SendAttachChunk(peerID string, data []byte) error
	SendP2PData(peerID string, data []byte) error
	// EnsureFilesPCReady — ленивый подъём files PeerConnection и ожидание
	// открытия attachments DC. Вызывается перед первым SendAttachChunk
	// (sender → manifest, receiver → chunk request).
	EnsureFilesPCReady(ctx context.Context, peerID string) error
	// AttachBufferedAmount — байты в SCTP-буфере attachments DC (0 если нет).
	// Только для `file transfer stats`.
	AttachBufferedAmount(peerID string) uint64
}

// MessagingOps — узкий интерфейс к MessagingService для эмиттов фронту
// и проверки является ли msg attachment'ом для legacy-смешивания.
type MessagingOps interface {
	EmitEvent(eventName string, data any)
}

// GroupsOps — узкий интерфейс к GroupsService для определения членов группы.
type GroupsOps interface {
	GetGroupMemberIDs(groupID string) []string
}

// FilesService — Wails-сервис для path-based передачи файлов через CAS.
//
// Один экземпляр на приложение. Создаётся в main.go после CAS+NetworkService,
// wire'ится через Set*Ops. Регистрируется как Wails Service.
type FilesService struct {
	log     *slog.Logger
	sess    *session.Session
	db      *sql.DB
	network NetworkOps
	msg     MessagingOps
	groups  GroupsOps

	mu       sync.Mutex
	uploads  map[string]*uploadState     // msgID → активная отправка
	downloads map[string]*downloadState  // file_sha → активный pull
	pendingApprovals map[string]*pendingApproval // legacy approval flow (не используется в manual-mode)
	manualSources    map[string]*manualSource    // file_sha → откуда качать когда юзер нажмёт «Скачать»

	// manifestInFlight — (msgID|peer), для которых sendManifestsToPeer уже
	// ждёт attach DC / шлёт manifest. TopicP2PReady и TopicAttachReady могут
	// сработать в пределах одного ожидания (30 с) — без guard'а manifest
	// уходил дважды. Под s.mu.
	manifestInFlight map[string]bool

	// Счётчики для `file transfer stats` (см. stats.go). Только atomic.
	stats        transferStats
	statsRunning atomic.Bool
}

// New создаёт сервис с зависимостями. Wire-операции (network, msg, groups)
// инжектируются позже через Set* — избегаем circular dep.
func New(log *slog.Logger, sess *session.Session, db *sql.DB) *FilesService {
	return &FilesService{
		log:              log,
		sess:             sess,
		db:               db,
		uploads:          make(map[string]*uploadState),
		downloads:        make(map[string]*downloadState),
		pendingApprovals: make(map[string]*pendingApproval),
		manualSources:    make(map[string]*manualSource),
		manifestInFlight: make(map[string]bool),
	}
}

// SetNetwork — wire NetworkService после его создания.
func (s *FilesService) SetNetwork(n NetworkOps) { s.network = n }

// SetMessaging — wire MessagingService для emitEvent.
func (s *FilesService) SetMessaging(m MessagingOps) { s.msg = m }

// SetGroups — wire GroupsService для group-member resolution.
func (s *FilesService) SetGroups(g GroupsOps) { s.groups = g }
