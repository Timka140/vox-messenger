package storage

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// migration — одна версионированная миграция схемы. Порядок в массиве
// `migrations` должен соответствовать version по возрастанию, а version —
// быть уникальным и не переиспользоваться (новые миграции только в конец).
type migration struct {
	version int
	sql     string
}

func (s *Storage) migrate() error {
	const op = "storage.migrate"

	// Baseline-схема: идемпотентна (CREATE TABLE IF NOT EXISTS), всегда
	// выполняется. Служит для чистого старта новой БД.
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("%s: schema: %w", op, err)
	}

	// Таблица учёта применённых миграций. Создаётся до любых миграций.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("%s: schema_migrations: %w", op, err)
	}

	// Читаем уже применённые версии.
	applied := make(map[int]bool)
	rows, err := s.db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("%s: read applied: %w", op, err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("%s: scan applied: %w", op, err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("%s: rows: %w", op, err)
	}
	rows.Close()

	// Применяем по порядку, каждую в своей транзакции.
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return fmt.Errorf("%s: migration %d: %w", op, m.version, err)
		}
	}

	return nil
}

// applyMigration выполняет одну миграцию в транзакции и, в случае успеха,
// записывает её версию в schema_migrations. Legacy-случай: если ошибка —
// "duplicate column" / "already exists" (т.е. миграция была накатана ещё до
// появления schema_migrations), помечаем её применённой БЕЗ повторного Exec.
func (s *Storage) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // no-op после Commit

	if _, err := tx.Exec(m.sql); err != nil {
		// Legacy: БД уже имеет эту колонку/таблицу — миграция была применена
		// в старой версии приложения, до появления schema_migrations.
		// Откатываем транзакцию и пишем пометку в отдельной транзакции.
		if isLegacyAlreadyApplied(err) {
			_ = tx.Rollback()
			if _, err := s.db.Exec(
				`INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
				m.version, time.Now().Unix()); err != nil {
				return fmt.Errorf("mark legacy: %w", err)
			}
			log.Printf("[INFO] migration %d: legacy schema, marked as applied", m.version)
			return nil
		}
		return fmt.Errorf("exec: %w (query: %s)", err, truncateQuery(m.sql))
	}

	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, time.Now().Unix()); err != nil {
		return fmt.Errorf("record: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	log.Printf("[INFO] migration %d applied", m.version)
	return nil
}

func isLegacyAlreadyApplied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "duplicate column name") ||
		strings.Contains(msg, "already exists")
}

func truncateQuery(q string) string {
	q = strings.TrimSpace(q)
	if len(q) > 80 {
		return q[:80] + "..."
	}
	return q
}

// migrations — версионированные инкрементальные изменения схемы.
// Правила:
//   - НИКОГДА не меняйте существующую запись (version+sql) — только добавляйте новую в конец.
//   - version монотонно возрастает и уникален.
//   - Idempotent SQL (IF NOT EXISTS) предпочтителен, но необязателен: legacy-fallback
//     обработает "duplicate column name" для старых БД без schema_migrations.
var migrations = []migration{
	{1, "ALTER TABLE groups ADD COLUMN type INTEGER NOT NULL DEFAULT 0"},
	{2, "ALTER TABLE group_members ADD COLUMN last_read_at INTEGER NOT NULL DEFAULT 0"},
	{3, "ALTER TABLE messages ADD COLUMN delivered INTEGER NOT NULL DEFAULT 1"},
	{4, "ALTER TABLE group_messages ADD COLUMN edited INTEGER NOT NULL DEFAULT 0"},
	{5, "ALTER TABLE group_members ADD COLUMN status TEXT NOT NULL DEFAULT 'active'"},
	{6, "ALTER TABLE messages ADD COLUMN audio_data BLOB"},
	{7, "ALTER TABLE messages ADD COLUMN duration INTEGER NOT NULL DEFAULT 0"},
	{8, "ALTER TABLE messages ADD COLUMN waveform TEXT NOT NULL DEFAULT ''"},
	{9, "ALTER TABLE group_messages ADD COLUMN audio_data BLOB"},
	{10, "ALTER TABLE group_messages ADD COLUMN duration INTEGER NOT NULL DEFAULT 0"},
	{11, "ALTER TABLE group_messages ADD COLUMN waveform TEXT NOT NULL DEFAULT ''"},
	{12, "ALTER TABLE groups ADD COLUMN voice_enabled INTEGER NOT NULL DEFAULT 1"},
	// Звонки
	{13, "CREATE TABLE IF NOT EXISTS calls (id TEXT PRIMARY KEY, creator_id TEXT NOT NULL, type TEXT NOT NULL DEFAULT 'dm', status TEXT NOT NULL DEFAULT 'ringing', group_id TEXT, created_at INTEGER NOT NULL, ended_at INTEGER)"},
	{14, "CREATE TABLE IF NOT EXISTS call_participants (call_id TEXT NOT NULL, peer_id TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'ringing', joined_at INTEGER, left_at INTEGER, PRIMARY KEY (call_id, peer_id))"},
	{15, "CREATE INDEX IF NOT EXISTS idx_calls_created ON calls(created_at)"},
	// Индексы для оптимизации частых запросов
	{16, "CREATE INDEX IF NOT EXISTS idx_contacts_status ON contacts(status)"},
	{17, "CREATE INDEX IF NOT EXISTS idx_fr_status_peer ON friend_requests(status, peer_id, direction)"},
	{18, "CREATE INDEX IF NOT EXISTS idx_group_members_status ON group_members(group_id, status)"},
	// Server relay для больших групп: per-group режим доставки + серверный seq/ts
	{19, "ALTER TABLE groups ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT 'p2p'"},
	{20, "ALTER TABLE groups ADD COLUMN last_seen_seq INTEGER NOT NULL DEFAULT 0"},
	{21, "ALTER TABLE group_messages ADD COLUMN seq INTEGER NOT NULL DEFAULT 0"},
	{22, "ALTER TABLE group_messages ADD COLUMN server_ts_ms INTEGER NOT NULL DEFAULT 0"},
	{23, "CREATE UNIQUE INDEX IF NOT EXISTS idx_group_msg_seq ON group_messages(group_id, seq) WHERE seq > 0"},
	// Mutual auth: pubkey сигнального сервера (TOFU + защита от MITM).
	// Пустая строка для legacy-записей; заполняется либо при backfill из
	// known_servers.go, либо TOFU-записью при первом успешном handshake.
	{24, "ALTER TABLE signal_servers ADD COLUMN pubkey TEXT NOT NULL DEFAULT ''"},
	// Meshtastic plaintext-сообщения через TEXT_MESSAGE_APP=1. Совместимо
	// с любым стандартным Meshtastic-клиентом в эфире — не encrypted на
	// уровне Vox (только канальное AES Meshtastic). Хранятся отдельно от
	// обычных messages: schema разная (src — nodeNum, не Vox pubkey),
	// retention — пользователь может чистить руками.
	{25, "CREATE TABLE IF NOT EXISTS mesh_texts (id INTEGER PRIMARY KEY AUTOINCREMENT, msg_id INTEGER NOT NULL, src_node_num INTEGER NOT NULL, src_node_id TEXT NOT NULL, src_long_name TEXT NOT NULL DEFAULT '', direction TEXT NOT NULL, text TEXT NOT NULL, rx_snr REAL NOT NULL DEFAULT 0, rx_rssi INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, read INTEGER NOT NULL DEFAULT 0)"},
	{26, "CREATE UNIQUE INDEX IF NOT EXISTS idx_mesh_texts_dedup ON mesh_texts(msg_id, src_node_num) WHERE direction = 'incoming'"},
	{27, "CREATE INDEX IF NOT EXISTS idx_mesh_texts_created ON mesh_texts(created_at)"},
	// Mesh-node ID mapping for E2E encrypted DM through LoRa (M6.2). 0 = unset.
	// Reverse lookup at receive time: src_node_num → contact → friend pubkey.
	{28, "ALTER TABLE contacts ADD COLUMN meshtastic_nodenum INTEGER NOT NULL DEFAULT 0"},
	{29, "CREATE INDEX IF NOT EXISTS idx_contacts_meshnode ON contacts(meshtastic_nodenum) WHERE meshtastic_nodenum != 0"},
	// Track destination of outgoing mesh texts so per-node chat filtering works.
	// 0 = legacy entries (pre-rework) treated as broadcast. 0xFFFFFFFF = explicit broadcast.
	// For incoming texts we store our own localNode → harmless extra metadata.
	{30, "ALTER TABLE mesh_texts ADD COLUMN dst_node_num INTEGER NOT NULL DEFAULT 0"},
	{31, "CREATE INDEX IF NOT EXISTS idx_mesh_texts_dst ON mesh_texts(dst_node_num) WHERE dst_node_num != 0"},
	// Attachments: deduplicated blob registry. Один файл в БД = одна row,
	// независимо от того сколько message_attachments на него ссылаются.
	// path — относительный к dataDir/attachments/ (формат "<sha[:2]>/<sha>.bin").
	// refcount считается на стороне приложения через INSERT/DELETE message_attachments.
	{32, "CREATE TABLE IF NOT EXISTS attachments (id TEXT PRIMARY KEY, sha256 TEXT NOT NULL UNIQUE, mime TEXT NOT NULL, size INTEGER NOT NULL, width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, path TEXT NOT NULL, refcount INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)"},
	{33, "CREATE INDEX IF NOT EXISTS idx_attachments_sha ON attachments(sha256)"},
	// message_attachments: many-to-many между messages/group_messages и attachments.
	// Ровно одно из (message_id, group_msg_id) заполнено. ord — порядок в alben.
	// kind ("photo"/"file") и name (оригинальное имя) хранятся per-link, потому что
	// один и тот же blob может быть переслан несколько раз с разными именами.
	{34, "CREATE TABLE IF NOT EXISTS message_attachments (id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT, group_msg_id TEXT, attachment_id TEXT NOT NULL, ord INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'file')"},
	{35, "CREATE INDEX IF NOT EXISTS idx_msg_atts_msg ON message_attachments(message_id) WHERE message_id IS NOT NULL"},
	{36, "CREATE INDEX IF NOT EXISTS idx_msg_atts_grpmsg ON message_attachments(group_msg_id) WHERE group_msg_id IS NOT NULL"},
	{37, "CREATE INDEX IF NOT EXISTS idx_msg_atts_att ON message_attachments(attachment_id)"},
	// ─── CAS (chunk-addressable storage) для надёжной передачи больших файлов ───
	// Параллельная схема к attachments/message_attachments (которые остаются для legacy):
	//   files          — file_sha → метаданные (size, chunk_size, chunk_count, mime, dims)
	//   file_chunks    — file_sha → ordered list chunk_sha (для восстановления)
	//   chunk_blobs    — chunk_sha → refcount (для GC), физически на диске в chunks/<c2>/<chunk_sha>.chunk
	//   message_files  — message/group_message → file_sha (replacement для message_attachments)
	//   file_downloads — receiver-side persistent state: bitmap полученных chunks для resume
	{38, "CREATE TABLE IF NOT EXISTS files (sha TEXT PRIMARY KEY, size INTEGER NOT NULL, chunk_size INTEGER NOT NULL, chunk_count INTEGER NOT NULL, mime TEXT NOT NULL DEFAULT '', width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0, refcount INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)"},
	{39, "CREATE TABLE IF NOT EXISTS file_chunks (file_sha TEXT NOT NULL, chunk_idx INTEGER NOT NULL, chunk_sha TEXT NOT NULL, PRIMARY KEY (file_sha, chunk_idx))"},
	{40, "CREATE INDEX IF NOT EXISTS idx_file_chunks_chunk_sha ON file_chunks(chunk_sha)"},
	{41, "CREATE TABLE IF NOT EXISTS chunk_blobs (sha TEXT PRIMARY KEY, size INTEGER NOT NULL, refcount INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)"},
	{42, "CREATE TABLE IF NOT EXISTS message_files (id INTEGER PRIMARY KEY AUTOINCREMENT, message_id TEXT, group_msg_id TEXT, file_sha TEXT NOT NULL, ord INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'file')"},
	{43, "CREATE INDEX IF NOT EXISTS idx_msg_files_msg ON message_files(message_id) WHERE message_id IS NOT NULL"},
	{44, "CREATE INDEX IF NOT EXISTS idx_msg_files_grpmsg ON message_files(group_msg_id) WHERE group_msg_id IS NOT NULL"},
	{45, "CREATE INDEX IF NOT EXISTS idx_msg_files_sha ON message_files(file_sha)"},
	// file_downloads — bitmap текущего прогресса. status: 'pending', 'active', 'complete', 'failed'.
	// chunks_received счётчик нужен чтобы быстро понять полнота без распаковки bitmap.
	{46, "CREATE TABLE IF NOT EXISTS file_downloads (file_sha TEXT NOT NULL, source_peer TEXT NOT NULL, received_bitmap BLOB NOT NULL, chunks_received INTEGER NOT NULL DEFAULT 0, last_active_at INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'pending', PRIMARY KEY (file_sha, source_peer))"},
	{47, "CREATE INDEX IF NOT EXISTS idx_file_downloads_status ON file_downloads(status, last_active_at)"},
	// local_path — где готовый файл лежит в файловой системе пользователя
	// (~/Downloads/Vox Downloads/<name> для receiver; путь к оригиналу для sender).
	// Пустая строка = только chunks в CAS (in-flight скачка или re-share-кэш).
	{48, "ALTER TABLE files ADD COLUMN local_path TEXT NOT NULL DEFAULT ''"},
	// Streaming-from-original режим (sender-side, файлы ≥ streamingThreshold).
	// source='cas' (default) — чанки на диске в chunks/.
	// source='streaming' — чанков нет, читаем по запросу из local_path.
	// origin_size + origin_mtime — invariant guard: если оригинал изменили или
	// заменили — отдаём CHUNK_NAK (NakRemoved), receiver отменяет передачу.
	{49, "ALTER TABLE files ADD COLUMN source TEXT NOT NULL DEFAULT 'cas'"},
	{50, "ALTER TABLE files ADD COLUMN origin_size INTEGER NOT NULL DEFAULT 0"},
	{51, "ALTER TABLE files ADD COLUMN origin_mtime INTEGER NOT NULL DEFAULT 0"},
	// Single-file модель: receiver пишет приходящие чанки напрямую в .vox-partial
	// в Vox Downloads через WriteAt по offset. partial_path — путь к этому файлу,
	// чтобы resume после перезапуска приложения мог переоткрыть тот же файл и не
	// перекачивать с нуля. На complete делаем os.Rename(.vox-partial → final).
	{52, "ALTER TABLE file_downloads ADD COLUMN partial_path TEXT NOT NULL DEFAULT ''"},
	// Ответ с цитатой и «переслано от» (фаза 3b редизайна): JSON ReplyRef /
	// ForwardRef или пустая строка. Колонки общие для всех типов сообщений.
	{53, "ALTER TABLE messages ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''"},
	{54, "ALTER TABLE messages ADD COLUMN forwarded_from TEXT NOT NULL DEFAULT ''"},
	{55, "ALTER TABLE group_messages ADD COLUMN reply_to TEXT NOT NULL DEFAULT ''"},
	{56, "ALTER TABLE group_messages ADD COLUMN forwarded_from TEXT NOT NULL DEFAULT ''"},
	// Профили пиров (фаза 5a редизайна): ник и фото. Отдельно от contacts —
	// у не-друзей из групп строки в contacts нет, а ник и фото им тоже нужны.
	// Своя строка (мой peerId) тоже здесь. avatar — JPEG 256×256 ≤ 40 КБ.
	{57, "CREATE TABLE IF NOT EXISTS peer_profiles (peer_id TEXT PRIMARY KEY, nickname TEXT NOT NULL DEFAULT '', avatar_hash TEXT NOT NULL DEFAULT '', avatar BLOB, updated_at INTEGER NOT NULL DEFAULT 0)"},
	// Фото группы: хеш и JPEG, рассылает владелец (group_avatar).
	{58, "ALTER TABLE groups ADD COLUMN avatar_hash TEXT NOT NULL DEFAULT ''"},
	{59, "ALTER TABLE groups ADD COLUMN avatar BLOB"},
	// Ссылка-приглашение в группу (фаза 5d): nonce текущей ссылки и её срок.
	// Новый nonce («Сбросить») делает старую ссылку недействительной.
	{60, "ALTER TABLE groups ADD COLUMN invite_nonce TEXT NOT NULL DEFAULT ''"},
	{61, "ALTER TABLE groups ADD COLUMN invite_expires INTEGER NOT NULL DEFAULT 0"},
	// Исключённые участники: исключение удаляет строку group_members, и без
	// этой таблицы исключённый вернулся бы по той же ссылке.
	{62, "CREATE TABLE IF NOT EXISTS group_bans (group_id TEXT NOT NULL, peer_id TEXT NOT NULL, banned_at INTEGER NOT NULL, PRIMARY KEY (group_id, peer_id))"},
	// Блокировка пользователя (фаза 7b): отдельно от contacts — INSERT OR
	// REPLACE ... 'accepted' в друзьях перезаписал бы статус, а выборки
	// status='accepted' потеряли бы заблокированного друга.
	{63, "CREATE TABLE IF NOT EXISTS blocked_peers (peer_id TEXT PRIMARY KEY, nickname TEXT NOT NULL DEFAULT '', blocked_at INTEGER NOT NULL)"},
	// Откуда запись сигнального сервера: builtin (KnownSignalServers),
	// manual (добавил пользователь), cluster (server_list от узла, заменяется
	// целиком при каждом новом списке).
	{64, "ALTER TABLE signal_servers ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'"},
	// Групповой relay кластера (журнал на узел): seq уникален только в паре
	// группа + узел-источник, поэтому старый индекс (group_id, seq) потерял бы
	// сообщение другого узла с тем же seq.
	{65, "ALTER TABLE group_messages ADD COLUMN origin_node TEXT NOT NULL DEFAULT ''"},
	{66, "DROP INDEX IF EXISTS idx_group_msg_seq"},
	{67, "CREATE UNIQUE INDEX IF NOT EXISTS idx_group_msg_origin_seq ON group_messages(group_id, origin_node, seq) WHERE seq > 0"},
	// Курсоры group_sync по журналам и свои сообщения, ждущие подтверждения.
	{68, "CREATE TABLE IF NOT EXISTS group_relay_cursors (group_id TEXT NOT NULL, origin_node TEXT NOT NULL, seq INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (group_id, origin_node))"},
	{69, "CREATE TABLE IF NOT EXISTS group_relay_pending (msg_id TEXT PRIMARY KEY, group_id TEXT NOT NULL, created_at INTEGER NOT NULL)"},
	// Курсор сменил смысл: был «максимум увиденного seq», стал «граница
	// непрерывно полученного». Старые значения стоят впереди реально
	// полученного, поэтому пропущенная середина сама бы не вернулась — сброс
	// заставляет первый догон перекачать хранимую историю (дедуп по id).
	{70, "UPDATE group_relay_cursors SET seq = 0"},
	// Код окончательного отказа сервера для своего сообщения в группе
	// (пусто — не отказано). Отказ приходит асинхронно, часто при закрытом
	// чате, и без флага после перезапуска сообщение висело бы с часиками.
	{71, "ALTER TABLE group_messages ADD COLUMN send_failed TEXT NOT NULL DEFAULT ''"},
}

const schema = `
-- Идентичность пользователя (зашифрованный приватный ключ)
CREATE TABLE IF NOT EXISTS identity (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    private_key BLOB    NOT NULL,
    public_key  BLOB    NOT NULL,
    created_at  INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

-- Настройки (nickname, язык, тема и т.д.)
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Контакты / друзья
CREATE TABLE IF NOT EXISTS contacts (
    peer_id    TEXT PRIMARY KEY,
    nickname   TEXT    NOT NULL DEFAULT '',
    public_key BLOB,
    status     TEXT    NOT NULL DEFAULT 'pending',  -- pending, accepted, blocked
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

-- Заявки в друзья
CREATE TABLE IF NOT EXISTS friend_requests (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    peer_id    TEXT    NOT NULL,
    direction  TEXT    NOT NULL,  -- incoming, outgoing
    status     TEXT    NOT NULL DEFAULT 'pending',  -- pending, accepted, rejected
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

-- Личные сообщения
CREATE TABLE IF NOT EXISTS messages (
    id         TEXT    PRIMARY KEY,
    peer_id    TEXT    NOT NULL,
    sender_id  TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    type       TEXT    NOT NULL DEFAULT 'text',  -- text, voice
    read       INTEGER NOT NULL DEFAULT 0,
    edited     INTEGER NOT NULL DEFAULT 0,
    deleted    INTEGER NOT NULL DEFAULT 0,
    delivered  INTEGER NOT NULL DEFAULT 1,  -- 0 = ожидает, 1 = доставлено
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    FOREIGN KEY (peer_id) REFERENCES contacts(peer_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_peer    ON messages(peer_id, created_at);
CREATE INDEX IF NOT EXISTS idx_messages_unread  ON messages(peer_id, read) WHERE read = 0;

-- Группы (каждая группа имеет текстовый чат + голосовой канал)
CREATE TABLE IF NOT EXISTS groups (
    group_id   TEXT PRIMARY KEY,
    name       TEXT    NOT NULL,
    owner_id   TEXT    NOT NULL,
    type       INTEGER NOT NULL DEFAULT 0,  -- 0 = текстовая группа, 1 = голосовой канал
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);

-- Участники группы
CREATE TABLE IF NOT EXISTS group_members (
    group_id     TEXT NOT NULL,
    peer_id      TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'member',  -- admin, member
    joined_at    INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    last_read_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (group_id, peer_id),
    FOREIGN KEY (group_id) REFERENCES groups(group_id)
);

-- Сообщения группы
CREATE TABLE IF NOT EXISTS group_messages (
    id         TEXT    PRIMARY KEY,
    group_id   TEXT    NOT NULL,
    sender_id  TEXT    NOT NULL,
    content    TEXT    NOT NULL,
    type       TEXT    NOT NULL DEFAULT 'text',
    deleted    INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT (strftime('%s','now')),
    FOREIGN KEY (group_id) REFERENCES groups(group_id)
);
CREATE INDEX IF NOT EXISTS idx_group_messages ON group_messages(group_id, created_at);

-- Доставка групповых сообщений (per-recipient tracking)
CREATE TABLE IF NOT EXISTS group_message_delivery (
    msg_id    TEXT    NOT NULL,
    peer_id   TEXT    NOT NULL,
    delivered INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (msg_id, peer_id)
);
CREATE INDEX IF NOT EXISTS idx_gmd_pending ON group_message_delivery(peer_id, delivered) WHERE delivered = 0;

-- Очередь pending групповых действий для офлайн пиров
CREATE TABLE IF NOT EXISTS pending_group_actions (
    id         TEXT    NOT NULL,
    group_id   TEXT    NOT NULL,
    peer_id    TEXT    NOT NULL,
    type       TEXT    NOT NULL,   -- invite, leave, rejoin, kick, rename, delete, member_added
    payload    TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (id, peer_id)
);
CREATE INDEX IF NOT EXISTS idx_pga_peer ON pending_group_actions(peer_id);

-- Сигнальные серверы
CREATE TABLE IF NOT EXISTS signal_servers (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    address  TEXT    NOT NULL UNIQUE,
    pubkey   TEXT    NOT NULL DEFAULT '', -- hex Ed25519 pubkey сервера; '' = TOFU при первом подключении
    added_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
`
