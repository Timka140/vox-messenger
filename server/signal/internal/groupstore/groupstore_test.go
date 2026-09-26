package groupstore

import (
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openStore(t *testing.T, node string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "g.db"), node, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(g, author, cmid string, ts int64) Message {
	return Message{GroupID: g, AuthorKey: author, ClientMsgID: cmid, Payload: "p-" + cmid,
		SizeBytes: 3, ServerTsMs: ts, ClientTsMs: ts}
}

func replica(g, origin string, seq int64, author, cmid string, ts int64) Message {
	m := msg(g, author, cmid, ts)
	m.OriginNode, m.Seq = origin, seq
	return m
}

func ids(ms []Message) string {
	out := ""
	for i, m := range ms {
		if i > 0 {
			out += ","
		}
		out += m.ClientMsgID
	}
	return out
}

func TestMigrateV0(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE server_groups (id TEXT PRIMARY KEY, delivery_mode TEXT NOT NULL DEFAULT 'server',
			created_at INTEGER NOT NULL, member_count INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE server_group_members (group_id TEXT NOT NULL, public_key TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0, joined_at INTEGER NOT NULL, last_seen_seq INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (group_id, public_key))`,
		`CREATE TABLE server_group_messages (group_id TEXT NOT NULL, seq INTEGER NOT NULL, author_key TEXT NOT NULL,
			client_msg_id TEXT NOT NULL, payload TEXT NOT NULL, encrypted INTEGER NOT NULL DEFAULT 0,
			size_bytes INTEGER NOT NULL, server_ts_ms INTEGER NOT NULL, client_ts_ms INTEGER NOT NULL,
			PRIMARY KEY (group_id, seq))`,
		`CREATE UNIQUE INDEX idx_messages_dedup ON server_group_messages(group_id, author_key, client_msg_id)`,
		`INSERT INTO server_groups VALUES ('g1', 'server', 1, 2)`,
		`INSERT INTO server_group_members (group_id, public_key, is_admin, joined_at) VALUES ('g1', 'a', 1, 1), ('g1', 'b', 0, 1)`,
		`INSERT INTO server_group_messages VALUES ('g1', 1, 'a', 'm1', 'x', 0, 1, 100, 100), ('g1', 2, 'b', 'm2', 'y', 0, 1, 200, 200)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path, "nodeA", quiet())
	if err != nil {
		t.Fatal(err)
	}
	msgs, more, err := s.MessagesSinceCursors("g1", nil, 10)
	if err != nil || more || ids(msgs) != "m1,m2" || msgs[0].OriginNode != "nodeA" {
		t.Fatalf("после миграции старые сообщения в журнале узла: %+v more=%v err=%v", msgs, more, err)
	}
	if ok, _ := s.IsMember("g1", "b"); !ok {
		t.Fatal("участники должны пережить миграцию")
	}
	if m, isNew, err := s.AppendMessage(msg("g1", "a", "m3", 300)); err != nil || !isNew || m.Seq != 3 {
		t.Fatalf("seq продолжает журнал: %+v new=%v err=%v", m, isNew, err)
	}
	s.Close()

	s, err = Open(path, "nodeA", quiet())
	if err != nil {
		t.Fatal("повторное открытие мигрированной БД:", err)
	}
	defer s.Close()
	if msgs, _, _ := s.MessagesSinceCursors("g1", nil, 10); len(msgs) != 3 {
		t.Fatalf("после повторного открытия %d сообщений, ожидалось 3", len(msgs))
	}
}

func TestJournalsPerOrigin(t *testing.T) {
	s := openStore(t, "A")
	if _, err := s.RegisterGroup("g", []string{"a", "b"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	m1, isNew, err := s.AppendMessage(msg("g", "a", "m1", 100))
	if err != nil || !isNew || m1.OriginNode != "A" || m1.Seq != 1 {
		t.Fatalf("своё сообщение: %+v %v %v", m1, isNew, err)
	}
	if ok, _ := s.InsertReplica(replica("g", "B", 1, "b", "b1", 150)); !ok {
		t.Fatal("реплика журнала B должна сохраниться, хотя seq=1 уже есть у A")
	}
	if ok, _ := s.InsertReplica(replica("g", "B", 1, "b", "b1", 150)); ok {
		t.Fatal("повтор реплики не должен дублироваться")
	}
	if ok, _ := s.InsertReplica(replica("g", "B", 2, "a", "m1", 160)); ok {
		t.Fatal("тот же client_msg_id автора из другого журнала — дубль")
	}
	if m2, _, _ := s.AppendMessage(msg("g", "a", "m2", 200)); m2.Seq != 2 {
		t.Fatalf("seq своего журнала не зависит от чужих: %d", m2.Seq)
	}
	if again, isNew, _ := s.AppendMessage(msg("g", "a", "m1", 999)); isNew || again.Seq != 1 || again.OriginNode != "A" {
		t.Fatalf("повтор отправки возвращает первую копию: %+v new=%v", again, isNew)
	}
	heads, _ := s.Heads()
	if heads["g"]["A"] != 2 || heads["g"]["B"] != 1 {
		t.Fatalf("головы журналов: %+v", heads)
	}
	if ok, _ := s.InsertReplica(replica("g2", "B", 1, "b", "x", 1)); !ok {
		t.Fatal("реплика в неизвестную группу должна создать группу")
	}
	if g, _ := s.GetGroup("g2"); g == nil {
		t.Fatal("группа g2 должна появиться")
	}
}

func TestSyncCursors(t *testing.T) {
	s := openStore(t, "A")
	s.RegisterGroup("g", []string{"a", "b"}, nil)
	for i, ts := range []int64{100, 300, 500} {
		s.AppendMessage(msg("g", "a", "a"+string(rune('1'+i)), ts))
	}
	s.InsertReplica(replica("g", "B", 1, "b", "b1", 200))
	s.InsertReplica(replica("g", "B", 2, "b", "b2", 400))

	got, more, _ := s.MessagesSinceCursors("g", map[string]int64{"A": 1}, 10)
	if ids(got) != "b1,a2,b2,a3" || more {
		t.Fatalf("хвосты журналов по времени: %s more=%v", ids(got), more)
	}
	got, more, _ = s.MessagesSinceCursors("g", map[string]int64{"A": 1}, 2)
	if ids(got) != "b1,a2" || !more {
		t.Fatalf("limit 2: %s more=%v", ids(got), more)
	}

	// Время внутри журнала не по порядку seq: префикс журнала сохраняется,
	// выборка не пустая.
	s2 := openStore(t, "A")
	s2.InsertReplica(replica("h", "B", 1, "b", "late", 1000))
	s2.InsertReplica(replica("h", "B", 2, "b", "early", 50))
	got, more, _ = s2.MessagesSinceCursors("h", map[string]int64{}, 1)
	if ids(got) != "late" || !more {
		t.Fatalf("префикс журнала: %s more=%v", ids(got), more)
	}
	got, _, _ = s2.MessagesSinceCursors("h", map[string]int64{"B": 1}, 1)
	if ids(got) != "early" {
		t.Fatalf("продолжение по курсору: %s", ids(got))
	}
}

func TestMembersLWW(t *testing.T) {
	s := openStore(t, "A")
	rows, err := s.RegisterGroup("g", []string{"a", "b"}, []string{"a"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("регистрация: %+v %v", rows, err)
	}
	ts := rows[0].UpdatedMs
	isMember := func(pk string) bool { ok, _ := s.IsMember("g", pk); return ok }

	if n, _ := s.ApplyMembers("g", 0, []MemberRow{{PubKey: "b", Removed: true, UpdatedMs: ts - 1}}); n != 0 || !isMember("b") {
		t.Fatal("старое удаление не должно перетирать новое добавление")
	}
	if n, _ := s.ApplyMembers("g", 0, []MemberRow{{PubKey: "b", Removed: true, UpdatedMs: ts + 10}}); n != 1 || isMember("b") {
		t.Fatal("новое удаление применяется")
	}
	if g, _ := s.GetGroup("g"); g.MemberCount != 1 {
		t.Fatalf("member_count по активным: %d", g.MemberCount)
	}
	if n, _ := s.ApplyMembers("g", 0, []MemberRow{{PubKey: "b", UpdatedMs: ts + 10}}); n != 0 || isMember("b") {
		t.Fatal("при равном времени побеждает удаление")
	}
	if n, _ := s.ApplyMembers("g", 0, []MemberRow{{PubKey: "b", UpdatedMs: ts + 20}}); n != 1 || !isMember("b") {
		t.Fatal("новое добавление возвращает участника")
	}

	row, _ := s.AddMember("g", "c", false)
	if row == nil || row.UpdatedMs <= ts+20 || !isMember("c") {
		t.Fatalf("локальное добавление новее всех прежних строк: %+v", row)
	}
	if row, _ := s.AddMember("g", "c", false); row != nil {
		t.Fatal("повторное добавление без изменений — nil")
	}
	if row, _ := s.RemoveMember("g", "c"); row == nil || !row.Removed || isMember("c") {
		t.Fatalf("удаление: %+v", row)
	}

	changed, _ := s.RegisterGroup("g", []string{"a"}, []string{"a"})
	if len(changed) != 1 || changed[0].PubKey != "b" || !changed[0].Removed {
		t.Fatalf("перерегистрация удаляет отсутствующих: %+v", changed)
	}
	full, _ := s.MembersFull("g")
	if len(full) != 3 {
		t.Fatalf("полный состав хранит удалённых: %+v", full)
	}
	if list, _ := s.ListMembers("g"); len(list) != 1 || list[0] != "a" {
		t.Fatalf("активные: %v", list)
	}

	if n, err := s.ApplyMembers("g3", 5, []MemberRow{{PubKey: "x", UpdatedMs: 1}}); n != 1 || err != nil {
		t.Fatalf("состав неизвестной группы создаёт её: %d %v", n, err)
	}
}

// Клиент зеркалит добавление всегда с is_admin=false, а заявка по ссылке
// повторяется и для уже состоящего — добавление не должно снимать админа.
func TestAddMemberKeepsAdmin(t *testing.T) {
	s := openStore(t, "n1")
	if _, err := s.RegisterGroup("g", []string{"a", "b"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	isAdmin := func(pk string) bool {
		ok, err := s.IsAdmin("g", pk)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if row, _ := s.AddMember("g", "a", false); row != nil {
		t.Fatalf("повторное добавление админа ничего не меняет: %+v", row)
	}
	if !isAdmin("a") {
		t.Fatal("повторное добавление не снимает права администратора")
	}

	// Назначение админом по-прежнему работает.
	if row, _ := s.AddMember("g", "b", true); row == nil || !row.IsAdmin || !isAdmin("b") {
		t.Fatalf("добавление с флагом админа назначает права: %+v", row)
	}
	// Вернувшийся после удаления приходит с теми правами, что прислали.
	if _, err := s.RemoveMember("g", "b"); err != nil {
		t.Fatal(err)
	}
	if row, _ := s.AddMember("g", "b", false); row == nil || row.IsAdmin || isAdmin("b") {
		t.Fatalf("удалённый возвращается без унаследованных прав: %+v", row)
	}
	// Понижение остаётся за полным составом.
	if _, err := s.RegisterGroup("g", []string{"a", "b"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	if isAdmin("a") || !isAdmin("b") {
		t.Fatal("RegisterGroup задаёт права по полному списку")
	}
}

func TestGroupsForMember(t *testing.T) {
	s := openStore(t, "n1")
	if _, err := s.RegisterGroup("g1", []string{"a", "b"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterGroup("g2", []string{"a"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GroupsForMember("a")
	if err != nil || len(got) != 2 {
		t.Fatalf("группы участника: %v %v", got, err)
	}
	if _, err := s.RemoveMember("g1", "b"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GroupsForMember("b"); len(got) != 0 {
		t.Fatalf("удалённый участник групп не имеет: %v", got)
	}
}

// Очистка по сроку хранения не должна откатывать MAX(seq) журнала: иначе
// узел выдаст занятые номера заново, и реплики с ними молча пропадут.
func TestPurgeKeepsJournalHead(t *testing.T) {
	s := openStore(t, "A")
	if _, err := s.RegisterGroup("g", []string{"x", "y"}, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	for i, cmid := range []string{"a1", "a2", "a3"} {
		if _, _, err := s.AppendMessage(msg("g", "x", cmid, int64(100+i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []Message{replica("g", "B", 1, "y", "b1", 110), replica("g", "B", 2, "y", "b2", 120)} {
		if _, err := s.InsertReplica(r); err != nil {
			t.Fatal(err)
		}
	}

	// Отсечка в будущем: под очистку попадает всё.
	n, err := s.PurgeOlderThan(time.UnixMilli(1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("удалено %d строк, ожидалось 3 (по одной строке на журнал остаётся)", n)
	}
	heads, _ := s.Heads()
	if heads["g"]["A"] != 3 || heads["g"]["B"] != 2 {
		t.Fatalf("головы журналов после очистки: %+v", heads["g"])
	}

	m, isNew, err := s.AppendMessage(msg("g", "x", "a4", 2_000_000))
	if err != nil || !isNew {
		t.Fatal(err)
	}
	if m.Seq != 4 {
		t.Fatalf("после очистки seq продолжает журнал: %d, ожидалось 4", m.Seq)
	}

	// Повторная очистка пустого по сути журнала и журнала из одной строки.
	if _, err := s.PurgeOlderThan(time.UnixMilli(3_000_000)); err != nil {
		t.Fatal(err)
	}
	if heads, _ := s.Heads(); heads["g"]["A"] != 4 || heads["g"]["B"] != 2 {
		t.Fatalf("журнал из одной строки не чистится: %+v", heads["g"])
	}
	empty := openStore(t, "C")
	if n, err := empty.PurgeOlderThan(time.UnixMilli(1_000_000)); err != nil || n != 0 {
		t.Fatalf("очистка пустого стора: %d %v", n, err)
	}
}

// Время сервера монотонно внутри журнала: иначе очистка, которая режет по
// времени, выбьет строку из середины.
func TestAppendServerTsMonotonic(t *testing.T) {
	s := openStore(t, "A")
	if _, err := s.RegisterGroup("g", []string{"x"}, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	const base = 1_000_000
	first, _, err := s.AppendMessage(msg("g", "x", "m1", base))
	if err != nil {
		t.Fatal(err)
	}
	// Параллельный отправитель посчитал время раньше, но замок взял позже.
	late, _, err := s.AppendMessage(msg("g", "x", "m2", base-10))
	if err != nil {
		t.Fatal(err)
	}
	if late.Seq != first.Seq+1 || late.ServerTsMs != first.ServerTsMs {
		t.Fatalf("время подтянуто до предыдущей строки: seq=%d ts=%d", late.Seq, late.ServerTsMs)
	}

	// Скачок часов назад дальше допуска — журнал не тянем за ним.
	far, _, err := s.AppendMessage(msg("g", "x", "m3", base-tsSkewLimit-1))
	if err != nil {
		t.Fatal(err)
	}
	if far.ServerTsMs != base-tsSkewLimit-1 {
		t.Fatalf("за пределом допуска берётся своё время: %d", far.ServerTsMs)
	}
}

// LWW при равном времени сходится с любой стороны: удаление > админ >
// участник.
func TestMembersLWWTieBreak(t *testing.T) {
	a, b := openStore(t, "A"), openStore(t, "B")
	const ts = 500
	plain := []MemberRow{{PubKey: "x", IsAdmin: false, UpdatedMs: ts}}
	admin := []MemberRow{{PubKey: "x", IsAdmin: true, UpdatedMs: ts}}
	if _, err := a.ApplyMembers("g", 1, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ApplyMembers("g", 1, admin); err != nil {
		t.Fatal(err)
	}

	// Обмен в обе стороны.
	if n, _ := a.ApplyMembers("g", 1, admin); n != 1 {
		t.Fatalf("у A при равном времени админ побеждает участника: n=%d", n)
	}
	if n, _ := b.ApplyMembers("g", 1, plain); n != 0 {
		t.Fatalf("у B участник не перетирает админа: n=%d", n)
	}
	for name, st := range map[string]*Store{"A": a, "B": b} {
		if ok, _ := st.IsAdmin("g", "x"); !ok {
			t.Fatalf("узел %s: состав сошёлся на админе", name)
		}
	}

	// Удаление по-прежнему старше всего.
	removed := []MemberRow{{PubKey: "x", Removed: true, UpdatedMs: ts}}
	if n, _ := a.ApplyMembers("g", 1, removed); n != 1 {
		t.Fatalf("удаление при равном времени побеждает админа: n=%d", n)
	}
	if n, _ := a.ApplyMembers("g", 1, admin); n != 0 {
		t.Fatalf("админ не воскрешает удалённого при равном времени: n=%d", n)
	}
}

// Пачка реплик одной транзакцией сохраняет семантику InsertReplica: дедуп
// по ключу журнала и по (автор, client_msg_id), несколько групп, пропуск
// некорректных строк.
func TestInsertReplicasBatch(t *testing.T) {
	s := openStore(t, "A")
	if _, err := s.InsertReplica(replica("g1", "B", 1, "y", "old", 100)); err != nil {
		t.Fatal(err)
	}
	batch := []Message{
		replica("g1", "B", 1, "y", "old", 100),  // дубль по ключу
		replica("g1", "B", 2, "y", "b2", 110),   // новое
		replica("g1", "B", 3, "y", "old", 120),  // дубль по (автор, client_msg_id)
		replica("g2", "C", 1, "z", "c1", 130),   // новое, другая группа
		{GroupID: "g1", OriginNode: "", Seq: 9}, // некорректное
	}
	isNew, err := s.InsertReplicas(batch)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{false, true, false, true, false}
	for i := range want {
		if isNew[i] != want[i] {
			t.Fatalf("isNew = %v, ожидалось %v", isNew, want)
		}
	}
	heads, _ := s.Heads()
	if heads["g1"]["B"] != 2 || heads["g2"]["C"] != 1 {
		t.Fatalf("головы после пачки: %+v", heads)
	}
	if g, _ := s.GetGroup("g2"); g == nil {
		t.Fatal("пачка создаёт неизвестную группу")
	}
	if isNew, err := s.InsertReplicas(nil); err != nil || len(isNew) != 0 {
		t.Fatalf("пустая пачка: %v %v", isNew, err)
	}
}
