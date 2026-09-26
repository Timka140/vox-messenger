// Package groupstore — SQLite backend серверного relay групп.
//
// Каждый узел кластера ведёт свой журнал сообщений группы: ключ сообщения
// (group_id, origin_node, seq), seq выдаёт только узел-источник. Журналы
// других узлов хранятся репликами. Состав группы — LWW по участнику:
// побеждает больший updated_ms, при равенстве удаление
// (docs/cluster-contract.md §7).
//
// Используется pure-Go драйвер modernc.org/sqlite чтобы избежать CGO.
package groupstore

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion — PRAGMA user_version актуальной схемы.
// 0 — одна последовательность seq на группу; 1 — журнал на узел + LWW состав.
const schemaVersion = 1

// Store — хранилище групп и сообщений.
type Store struct {
	db     *sql.DB
	log    *slog.Logger
	nodeID string

	// seqMu — seq своего журнала и вставка реплик: SELECT MAX + INSERT
	// подвержены гонке при параллельных отправителях.
	seqMu sync.Mutex
	// memMu — LWW-изменения состава (чтение текущей строки + запись).
	memMu sync.Mutex
}

// Group — регистрация группы на сервере.
type Group struct {
	ID           string
	DeliveryMode string // "p2p" или "server"
	CreatedAt    time.Time
	MemberCount  int
}

// GroupInfo — группа для состояния узла.
type GroupInfo struct {
	ID        string
	CreatedMs int64
}

// MemberRow — строка состава группы (LWW).
type MemberRow struct {
	PubKey    string
	IsAdmin   bool
	Removed   bool
	UpdatedMs int64
}

// Message — сохранённое сообщение группы.
type Message struct {
	GroupID     string
	OriginNode  string
	Seq         int64
	AuthorKey   string
	ClientMsgID string
	Payload     string
	Encrypted   bool
	SizeBytes   int
	ServerTsMs  int64
	ClientTsMs  int64
}

// Open открывает / создаёт БД и накатывает схему. nodeID — узел, чей журнал
// ведёт этот сервер (hex pubkey сигналки); старые сообщения попадают в него.
func Open(path, nodeID string, log *slog.Logger) (*Store, error) {
	if nodeID == "" {
		return nil, errors.New("groupstore: node id required")
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{db: db, log: log, nodeID: nodeID}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// NodeID — узел своего журнала.
func (s *Store) NodeID() string { return s.nodeID }

// Close закрывает БД.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	// Базовая схема версии 0 — для чистой БД и старых БД одинаково, дальше
	// поверх неё накатывается migrateV1.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS server_groups (
			id TEXT PRIMARY KEY,
			delivery_mode TEXT NOT NULL DEFAULT 'server',
			created_at INTEGER NOT NULL,
			member_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS server_group_members (
			group_id TEXT NOT NULL,
			public_key TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			joined_at INTEGER NOT NULL,
			last_seen_seq INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (group_id, public_key),
			FOREIGN KEY (group_id) REFERENCES server_groups(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_members_pubkey ON server_group_members(public_key)`,
		`CREATE TABLE IF NOT EXISTS server_group_messages (
			group_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			author_key TEXT NOT NULL,
			client_msg_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			encrypted INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL,
			server_ts_ms INTEGER NOT NULL,
			client_ts_ms INTEGER NOT NULL,
			PRIMARY KEY (group_id, seq),
			FOREIGN KEY (group_id) REFERENCES server_groups(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_ts ON server_group_messages(server_ts_ms)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_dedup ON server_group_messages(group_id, author_key, client_msg_id)`,
	}

	var ver int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if ver >= schemaVersion {
		return nil
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt, err)
		}
	}
	return s.migrateV1()
}

// migrateV1 — журнал на узел и LWW состав. Таблицу сообщений приходится
// перестроить: SQLite не меняет первичный ключ. Старые сообщения уходят в
// журнал этого узла.
func (s *Store) migrateV1() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	steps := []struct {
		q    string
		args []any
	}{
		{`CREATE TABLE server_group_messages_v1 (
			group_id TEXT NOT NULL,
			origin_node TEXT NOT NULL,
			seq INTEGER NOT NULL,
			author_key TEXT NOT NULL,
			client_msg_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			encrypted INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL,
			server_ts_ms INTEGER NOT NULL,
			client_ts_ms INTEGER NOT NULL,
			PRIMARY KEY (group_id, origin_node, seq),
			FOREIGN KEY (group_id) REFERENCES server_groups(id) ON DELETE CASCADE
		)`, nil},
		{`INSERT INTO server_group_messages_v1
			(group_id, origin_node, seq, author_key, client_msg_id, payload, encrypted, size_bytes, server_ts_ms, client_ts_ms)
			SELECT group_id, ?, seq, author_key, client_msg_id, payload, encrypted, size_bytes, server_ts_ms, client_ts_ms
			FROM server_group_messages`, []any{s.nodeID}},
		{`DROP TABLE server_group_messages`, nil},
		{`ALTER TABLE server_group_messages_v1 RENAME TO server_group_messages`, nil},
		{`CREATE INDEX IF NOT EXISTS idx_messages_ts ON server_group_messages(server_ts_ms)`, nil},
		{`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_dedup ON server_group_messages(group_id, author_key, client_msg_id)`, nil},
		{`ALTER TABLE server_group_members ADD COLUMN updated_ms INTEGER NOT NULL DEFAULT 0`, nil},
		{`ALTER TABLE server_group_members ADD COLUMN removed INTEGER NOT NULL DEFAULT 0`, nil},
		{fmt.Sprintf("PRAGMA user_version = %d", schemaVersion), nil},
	}
	for _, st := range steps {
		if _, err := tx.Exec(st.q, st.args...); err != nil {
			return fmt.Errorf("migrate v1: %w (query %.60q)", err, st.q)
		}
	}
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM server_group_messages`).Scan(&n)
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.log != nil {
		s.log.Info("groupstore: схема v1 (журнал на узел)", slog.Int("messages_migrated", n))
	}
	return nil
}

func nowMs() int64 { return time.Now().UTC().UnixMilli() }

// --- группы и состав ---

func ensureGroupTx(tx *sql.Tx, groupID string, createdMs int64) error {
	if createdMs <= 0 {
		createdMs = nowMs()
	}
	_, err := tx.Exec(`
		INSERT INTO server_groups (id, delivery_mode, created_at, member_count)
		VALUES (?, 'server', ?, 0)
		ON CONFLICT(id) DO NOTHING
	`, groupID, createdMs)
	return err
}

func membersTx(tx *sql.Tx, groupID string) (map[string]MemberRow, error) {
	rows, err := tx.Query(`
		SELECT public_key, is_admin, removed, updated_ms
		FROM server_group_members WHERE group_id = ?
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]MemberRow)
	for rows.Next() {
		var r MemberRow
		var admin, removed int
		if err := rows.Scan(&r.PubKey, &admin, &removed, &r.UpdatedMs); err != nil {
			return nil, err
		}
		r.IsAdmin, r.Removed = admin == 1, removed == 1
		out[r.PubKey] = r
	}
	return out, rows.Err()
}

// memberRank — порядок значений строки состава при равном времени:
// удаление > админ > участник. Полный порядок на всех значениях делает LWW
// сходящимся с любой стороны. Равное время здесь не редкость: localTs даёт
// max(updated_ms)+1, и после одного изменения оба узла считают для
// следующего ровно одно и то же время.
func memberRank(removed, admin bool) int {
	switch {
	case removed:
		return 2
	case admin:
		return 1
	}
	return 0
}

// applyMemberTx — LWW: строка применяется, если она новее текущей; при
// равном времени — если её значение старше по memberRank. Возвращает true,
// если строка записана.
func applyMemberTx(tx *sql.Tx, groupID string, r MemberRow) (bool, error) {
	var curTs int64
	var curRemoved, curAdmin int
	err := tx.QueryRow(`
		SELECT updated_ms, removed, is_admin FROM server_group_members
		WHERE group_id = ? AND public_key = ?
	`, groupID, r.PubKey).Scan(&curTs, &curRemoved, &curAdmin)
	// У надгробия флаг админа не хранится: иначе он мог бы разойтись между
	// узлами при равном времени, где ранг удаления одинаков.
	admin, removed := boolInt(r.IsAdmin && !r.Removed), boolInt(r.Removed)
	if errors.Is(err, sql.ErrNoRows) {
		_, err := tx.Exec(`
			INSERT INTO server_group_members
				(group_id, public_key, is_admin, joined_at, last_seen_seq, updated_ms, removed)
			VALUES (?, ?, ?, ?, 0, ?, ?)
		`, groupID, r.PubKey, admin, nowMs(), r.UpdatedMs, removed)
		return err == nil, err
	}
	if err != nil {
		return false, err
	}
	if r.UpdatedMs < curTs ||
		(r.UpdatedMs == curTs && memberRank(r.Removed, r.IsAdmin) <= memberRank(curRemoved == 1, curAdmin == 1)) {
		return false, nil
	}
	_, err = tx.Exec(`
		UPDATE server_group_members SET is_admin = ?, removed = ?, updated_ms = ?
		WHERE group_id = ? AND public_key = ?
	`, admin, removed, r.UpdatedMs, groupID, r.PubKey)
	return err == nil, err
}

func recountTx(tx *sql.Tx, groupID string) error {
	_, err := tx.Exec(`
		UPDATE server_groups SET member_count =
			(SELECT COUNT(*) FROM server_group_members WHERE group_id = ? AND removed = 0)
		WHERE id = ?
	`, groupID, groupID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// localTs — время локального изменения: не раньше последнего изменения
// группы, чтобы своё действие гарантированно победило своё же прошлое.
func localTs(cur map[string]MemberRow) int64 {
	ts := nowMs()
	for _, r := range cur {
		if r.UpdatedMs >= ts {
			ts = r.UpdatedMs + 1
		}
	}
	return ts
}

// RegisterGroup создаёт группу или задаёт её полный состав: перечисленные
// участники активны, прежние отсутствующие — удалены. Возвращает изменённые
// строки для репликации на другие узлы.
func (s *Store) RegisterGroup(groupID string, members []string, admins []string) ([]MemberRow, error) {
	if groupID == "" || len(members) == 0 {
		return nil, errors.New("group_id and members required")
	}
	adminSet := make(map[string]bool, len(admins))
	for _, a := range admins {
		adminSet[a] = true
	}

	s.memMu.Lock()
	defer s.memMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := ensureGroupTx(tx, groupID, 0); err != nil {
		return nil, fmt.Errorf("insert group: %w", err)
	}
	cur, err := membersTx(tx, groupID)
	if err != nil {
		return nil, err
	}
	ts := localTs(cur)

	var changed []MemberRow
	inList := make(map[string]bool, len(members))
	for _, pk := range members {
		if pk == "" || inList[pk] {
			continue
		}
		inList[pk] = true
		if c, ok := cur[pk]; ok && !c.Removed && c.IsAdmin == adminSet[pk] {
			continue // уже в этом состоянии
		}
		r := MemberRow{PubKey: pk, IsAdmin: adminSet[pk], UpdatedMs: ts}
		ok, err := applyMemberTx(tx, groupID, r)
		if err != nil {
			return nil, fmt.Errorf("member %s: %w", pk, err)
		}
		if ok {
			changed = append(changed, r)
		}
	}
	for pk, c := range cur {
		if inList[pk] || c.Removed {
			continue
		}
		r := MemberRow{PubKey: pk, IsAdmin: c.IsAdmin, Removed: true, UpdatedMs: ts}
		ok, err := applyMemberTx(tx, groupID, r)
		if err != nil {
			return nil, fmt.Errorf("remove %s: %w", pk, err)
		}
		if ok {
			changed = append(changed, r)
		}
	}
	if err := recountTx(tx, groupID); err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}

// setMember — локальное добавление / удаление одного участника.
func (s *Store) setMember(groupID, pubKey string, isAdmin, removed bool) (*MemberRow, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := ensureGroupTx(tx, groupID, 0); err != nil {
		return nil, err
	}
	cur, err := membersTx(tx, groupID)
	if err != nil {
		return nil, err
	}
	// Добавление не понижает права: операции «разжаловать» в протоколе нет,
	// а клиент зеркалит добавление всегда с is_admin=false. Законное
	// понижение делает RegisterGroup, задающий полный состав.
	if c, ok := cur[pubKey]; ok && !removed && !c.Removed && c.IsAdmin {
		isAdmin = true
	}
	if c, ok := cur[pubKey]; (ok && c.Removed == removed && (removed || c.IsAdmin == isAdmin)) || (!ok && removed) {
		return nil, tx.Commit() // уже в этом состоянии
	}
	r := MemberRow{PubKey: pubKey, IsAdmin: isAdmin && !removed, Removed: removed, UpdatedMs: localTs(cur)}
	ok, err := applyMemberTx(tx, groupID, r)
	if err != nil {
		return nil, err
	}
	if err := recountTx(tx, groupID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// AddMember добавляет участника. Возвращает строку для репликации или nil,
// если участник уже был в этом состоянии.
func (s *Store) AddMember(groupID, pubKey string, isAdmin bool) (*MemberRow, error) {
	return s.setMember(groupID, pubKey, isAdmin, false)
}

// RemoveMember удаляет участника (строка остаётся с removed=1 для LWW).
func (s *Store) RemoveMember(groupID, pubKey string) (*MemberRow, error) {
	return s.setMember(groupID, pubKey, false, true)
}

// ApplyMembers — строки состава с другого узла (LWW). Создаёт группу, если
// её здесь ещё нет. Возвращает число применённых строк.
func (s *Store) ApplyMembers(groupID string, createdMs int64, rows []MemberRow) (int, error) {
	if groupID == "" {
		return 0, errors.New("group_id required")
	}
	s.memMu.Lock()
	defer s.memMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := ensureGroupTx(tx, groupID, createdMs); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.PubKey == "" {
			continue
		}
		ok, err := applyMemberTx(tx, groupID, r)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	if err := recountTx(tx, groupID); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// MembersFull — все строки состава, включая удалённых (для состояния узла).
func (s *Store) MembersFull(groupID string) ([]MemberRow, error) {
	rows, err := s.db.Query(`
		SELECT public_key, is_admin, removed, updated_ms
		FROM server_group_members WHERE group_id = ?
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberRow
	for rows.Next() {
		var r MemberRow
		var admin, removed int
		if err := rows.Scan(&r.PubKey, &admin, &removed, &r.UpdatedMs); err != nil {
			return nil, err
		}
		r.IsAdmin, r.Removed = admin == 1, removed == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllGroups — все группы узла.
func (s *Store) AllGroups() ([]GroupInfo, error) {
	rows, err := s.db.Query(`SELECT id, created_at FROM server_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupInfo
	for rows.Next() {
		var g GroupInfo
		if err := rows.Scan(&g.ID, &g.CreatedMs); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetGroup возвращает группу по ID (или nil если не найдена).
func (s *Store) GetGroup(groupID string) (*Group, error) {
	var g Group
	var createdMs int64
	err := s.db.QueryRow(`
		SELECT id, delivery_mode, created_at, member_count
		FROM server_groups WHERE id = ?
	`, groupID).Scan(&g.ID, &g.DeliveryMode, &createdMs, &g.MemberCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt = time.UnixMilli(createdMs).UTC()
	return &g, nil
}

// IsMember проверяет, является ли pubKey активным участником группы.
func (s *Store) IsMember(groupID, pubKey string) (bool, error) {
	var dummy int
	err := s.db.QueryRow(`
		SELECT 1 FROM server_group_members
		WHERE group_id = ? AND public_key = ? AND removed = 0
	`, groupID, pubKey).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// IsAdmin проверяет админ ли pubKey в группе.
func (s *Store) IsAdmin(groupID, pubKey string) (bool, error) {
	var isAdmin int
	err := s.db.QueryRow(`
		SELECT is_admin FROM server_group_members
		WHERE group_id = ? AND public_key = ? AND removed = 0
	`, groupID, pubKey).Scan(&isAdmin)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isAdmin == 1, nil
}

// ListMembers возвращает список активных участников группы.
func (s *Store) ListMembers(groupID string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT public_key FROM server_group_members WHERE group_id = ? AND removed = 0
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// GroupsForMember возвращает группы, в которых участник состоит. Нужен, чтобы
// дослать подключившемуся клиенту delivery_mode его групп: узел мог узнать о
// группе по репликации, пока клиент был офлайн.
func (s *Store) GroupsForMember(pubKey string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT group_id FROM server_group_members
		WHERE public_key = ? AND removed = 0
	`, pubKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// --- сообщения ---

const msgCols = `group_id, origin_node, seq, author_key, client_msg_id, payload, encrypted,
	size_bytes, server_ts_ms, client_ts_ms`

type rowScanner interface{ Scan(dest ...any) error }

func scanMessage(r rowScanner) (Message, error) {
	var m Message
	var enc int
	err := r.Scan(&m.GroupID, &m.OriginNode, &m.Seq, &m.AuthorKey, &m.ClientMsgID, &m.Payload,
		&enc, &m.SizeBytes, &m.ServerTsMs, &m.ClientTsMs)
	m.Encrypted = enc == 1
	return m, err
}

func (s *Store) queryMessages(q string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FindMessage — сохранённое сообщение автора по client_msg_id (ключ дедупа).
func (s *Store) FindMessage(groupID, authorKey, clientMsgID string) (Message, bool, error) {
	m, err := scanMessage(s.db.QueryRow(`
		SELECT `+msgCols+` FROM server_group_messages
		WHERE group_id = ? AND author_key = ? AND client_msg_id = ?
	`, groupID, authorKey, clientMsgID))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, err
	}
	return m, true, nil
}

// tsSkewLimit — насколько можно тянуть server_ts_ms вперёд ради монотонности
// журнала. Дальше берём текущее время: иначе один скачок часов вперёд
// отравил бы журнал навсегда и его строки никогда не попали бы под очистку.
const tsSkewLimit = int64(5 * 60 * 1000)

// AppendMessage назначает seq и server_ts_ms в журнале этого узла и сохраняет
// сообщение. Если ClientMsgID уже встречался для (group_id, author_key) —
// возвращает существующее сообщение и isNew=false (повтор отправки).
//
// Время вызывающий считает до этого замка, поэтому два параллельных
// отправителя могут прийти с обратным порядком времени. Под замком оно
// подтягивается до времени предыдущей строки: порядок seq в журнале должен
// совпадать с порядком server_ts_ms, иначе очистка по сроку хранения, которая
// режет по времени, выбьет строку из середины журнала и оставит дыру.
func (s *Store) AppendMessage(msg Message) (Message, bool, error) {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()

	existing, err := scanMessage(s.db.QueryRow(`
		SELECT `+msgCols+` FROM server_group_messages
		WHERE group_id = ? AND author_key = ? AND client_msg_id = ?
	`, msg.GroupID, msg.AuthorKey, msg.ClientMsgID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, fmt.Errorf("dedup check: %w", err)
	}

	// Последняя строка журнала: и номер, и время одним поиском по индексу.
	var lastSeq, lastTs int64
	err = s.db.QueryRow(`
		SELECT seq, server_ts_ms FROM server_group_messages
		WHERE group_id = ? AND origin_node = ? ORDER BY seq DESC LIMIT 1
	`, msg.GroupID, s.nodeID).Scan(&lastSeq, &lastTs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, fmt.Errorf("last seq: %w", err)
	}
	msg.OriginNode = s.nodeID
	msg.Seq = lastSeq + 1
	if msg.ServerTsMs <= 0 {
		msg.ServerTsMs = nowMs()
	}
	if lastTs > msg.ServerTsMs && lastTs-msg.ServerTsMs < tsSkewLimit {
		msg.ServerTsMs = lastTs
	}

	if _, err := s.db.Exec(`
		INSERT INTO server_group_messages (`+msgCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, msg.GroupID, msg.OriginNode, msg.Seq, msg.AuthorKey, msg.ClientMsgID, msg.Payload,
		boolInt(msg.Encrypted), msg.SizeBytes, msg.ServerTsMs, msg.ClientTsMs); err != nil {
		return Message{}, false, fmt.Errorf("insert message: %w", err)
	}
	return msg, true, nil
}

// InsertReplica сохраняет сообщение журнала другого узла. Повтор (тот же
// ключ или тот же client_msg_id автора) игнорируется. Создаёт группу, если
// её здесь ещё нет. Возвращает true, если сообщение новое.
func (s *Store) InsertReplica(msg Message) (bool, error) {
	if msg.GroupID == "" || msg.OriginNode == "" || msg.Seq <= 0 {
		return false, errors.New("replica: group_id, origin_node, seq required")
	}
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if _, err := s.db.Exec(`
		INSERT INTO server_groups (id, delivery_mode, created_at, member_count)
		VALUES (?, 'server', ?, 0) ON CONFLICT(id) DO NOTHING
	`, msg.GroupID, nowMs()); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO server_group_messages (`+msgCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, msg.GroupID, msg.OriginNode, msg.Seq, msg.AuthorKey, msg.ClientMsgID, msg.Payload,
		boolInt(msg.Encrypted), msg.SizeBytes, msg.ServerTsMs, msg.ClientTsMs)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertReplicas — пачка сообщений других узлов одной транзакцией. Догон в
// несколько тысяч сообщений построчными автокоммитами — тысячи fsync прямо в
// read loop линка, за которыми ждёт сигналинг звонков. isNew[i] — сообщение
// msgs[i] новое; некорректные строки пропускаются (false). Если транзакция
// падает целиком, пачка разбирается построчно: одна плохая строка не должна
// стопорить весь догон на каждом цикле сверки.
func (s *Store) InsertReplicas(msgs []Message) ([]bool, error) {
	isNew := make([]bool, len(msgs))
	if len(msgs) == 0 {
		return isNew, nil
	}
	if err := s.insertReplicasTx(msgs, isNew); err == nil {
		return isNew, nil
	}
	for i := range isNew {
		isNew[i] = false
	}
	var firstErr error
	for i, m := range msgs {
		if !validReplica(m) {
			continue
		}
		ok, err := s.InsertReplica(m)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		isNew[i] = ok
	}
	return isNew, firstErr
}

func validReplica(m Message) bool {
	return m.GroupID != "" && m.OriginNode != "" && m.Seq > 0
}

func (s *Store) insertReplicasTx(msgs []Message, isNew []bool) error {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ensured := make(map[string]bool)
	for i, m := range msgs {
		if !validReplica(m) {
			continue
		}
		if !ensured[m.GroupID] {
			if err := ensureGroupTx(tx, m.GroupID, 0); err != nil {
				return err
			}
			ensured[m.GroupID] = true
		}
		res, err := tx.Exec(`
			INSERT OR IGNORE INTO server_group_messages (`+msgCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, m.GroupID, m.OriginNode, m.Seq, m.AuthorKey, m.ClientMsgID, m.Payload,
			boolInt(m.Encrypted), m.SizeBytes, m.ServerTsMs, m.ClientTsMs)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		isNew[i] = n > 0
	}
	return tx.Commit()
}

// Heads — максимальный seq по каждому журналу каждой группы.
func (s *Store) Heads() (map[string]map[string]int64, error) {
	rows, err := s.db.Query(`
		SELECT group_id, origin_node, MAX(seq) FROM server_group_messages
		GROUP BY group_id, origin_node
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]int64)
	for rows.Next() {
		var g, o string
		var seq int64
		if err := rows.Scan(&g, &o, &seq); err != nil {
			return nil, err
		}
		if out[g] == nil {
			out[g] = make(map[string]int64)
		}
		out[g][o] = seq
	}
	return out, rows.Err()
}

// MessagesAfter — сообщения журнала origin группы с seq > afterSeq.
func (s *Store) MessagesAfter(groupID, origin string, afterSeq int64, limit int) ([]Message, error) {
	return s.queryMessages(`
		SELECT `+msgCols+` FROM server_group_messages
		WHERE group_id = ? AND origin_node = ? AND seq > ?
		ORDER BY seq ASC LIMIT ?
	`, groupID, origin, afterSeq, limit)
}

// MessagesSinceCursors — хвосты всех журналов группы после курсоров
// {origin: seq} (отсутствующий журнал — с нуля), не больше limit.
// Журналы сливаются: на каждом шаге берётся очередное по seq сообщение того
// журнала, у которого оно раньше по времени сервера. Поэтому из каждого
// журнала приходит непрерывный префикс хвоста — курсор клиента можно двигать
// до последнего полученного seq, и выборка всегда продвигается.
func (s *Store) MessagesSinceCursors(groupID string, cursors map[string]int64, limit int) ([]Message, bool, error) {
	if limit <= 0 {
		limit = 100
	}
	origins, err := s.origins(groupID)
	if err != nil {
		return nil, false, err
	}

	hasMore := false
	lists := make([][]Message, 0, len(origins))
	for _, o := range origins {
		msgs, err := s.MessagesAfter(groupID, o, cursors[o], limit+1)
		if err != nil {
			return nil, false, err
		}
		if len(msgs) > limit {
			hasMore = true
			msgs = msgs[:limit]
		}
		if len(msgs) > 0 {
			lists = append(lists, msgs)
		}
	}

	out := make([]Message, 0, limit)
	idx := make([]int, len(lists))
	for len(out) < limit {
		best := -1
		for i, l := range lists {
			if idx[i] >= len(l) {
				continue
			}
			if best < 0 || before(l[idx[i]], lists[best][idx[best]]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		out = append(out, lists[best][idx[best]])
		idx[best]++
	}
	for i, l := range lists {
		if idx[i] < len(l) {
			hasMore = true
		}
	}
	return out, hasMore, nil
}

// before — порядок слияния журналов: время сервера, затем узел.
func before(a, b Message) bool {
	if a.ServerTsMs != b.ServerTsMs {
		return a.ServerTsMs < b.ServerTsMs
	}
	return a.OriginNode < b.OriginNode
}

func (s *Store) origins(groupID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT origin_node FROM server_group_messages WHERE group_id = ?`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CurrentSeq возвращает максимальный seq журнала этого узла (0 если пусто).
func (s *Store) CurrentSeq(groupID string) (int64, error) {
	var maxSeq int64
	err := s.db.QueryRow(`
		SELECT COALESCE(MAX(seq), 0) FROM server_group_messages WHERE group_id = ? AND origin_node = ?
	`, groupID, s.nodeID).Scan(&maxSeq)
	return maxSeq, err
}

// PurgeOlderThan удаляет сообщения старше указанного времени, оставляя
// последнюю строку каждого журнала: seq выдаётся как MAX(seq)+1, и если
// журнал вычистить целиком, узел начнёт выдавать занятые номера заново.
// Реплика с таким номером у соседа и у клиентов молча отбрасывается по
// ключу — ни ошибки, ни лога. Цена — по одному старому сообщению на журнал
// сверх срока хранения.
// Возвращает количество удалённых строк.
func (s *Store) PurgeOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM server_group_messages AS m
		WHERE m.server_ts_ms < ?
		  AND m.seq < (SELECT MAX(m2.seq) FROM server_group_messages AS m2
		               WHERE m2.group_id = m.group_id AND m2.origin_node = m.origin_node)
	`, cutoff.UTC().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
