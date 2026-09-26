package services

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vox_app/internal/network"
	"vox_app/internal/session"
	"vox_app/internal/storage"
)

func newRelayTestGroups(t *testing.T) (*GroupsService, *storage.Storage, string) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "relay.db"), bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sess := session.New()
	if err := sess.Set(priv, pub); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := NewGroupsService(log, sess, store, NewNetworkService(log, sess, store))
	me, _ := sess.PeerID()
	if _, err := store.DB().Exec(
		"INSERT INTO groups (group_id, name, owner_id, type, created_at, delivery_mode) VALUES ('G', 'Тест', ?, 0, ?, 'server')",
		me, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	return g, store, me
}

// relayNew — живой fan-out от сервера.
func relayNew(g *GroupsService, p network.GroupMessageNewPayload) {
	raw, _ := json.Marshal(p)
	g.handleGroupMessageNew(&network.Message{Type: network.TypeGroupMessageNew, Payload: raw}, false)
}

// relaySyncPage — страница ответа group_sync.
func relaySyncPage(g *GroupsService, groupID string, hasMore bool, msgs ...network.GroupMessageNewPayload) {
	raw, _ := json.Marshal(network.GroupSyncResultPayload{
		GroupID:  groupID,
		Messages: msgs,
		HasMore:  hasMore,
	})
	g.handleGroupSyncResult(&network.Message{Type: network.TypeGroupSyncResult, Payload: raw})
}

func TestRelaySameSeqDifferentOrigins(t *testing.T) {
	g, store, _ := newRelayTestGroups(t)
	author := strings.Repeat("b", 64)
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 1, AuthorKey: author, ClientMsgID: "m1", Payload: "раз", ServerTsMs: 1000, OriginNode: "n1"})
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 1, AuthorKey: author, ClientMsgID: "m2", Payload: "два", ServerTsMs: 2000, OriginNode: "n2"})

	var n int
	store.DB().QueryRow("SELECT COUNT(*) FROM group_messages WHERE group_id = 'G'").Scan(&n)
	if n != 2 {
		t.Fatalf("сообщения с одинаковым seq из разных журналов: %d строк, ожидалось 2", n)
	}
	if c := g.relayCursors("G"); c["n1"] != 1 || c["n2"] != 1 || len(c) != 2 {
		t.Fatalf("курсоры по журналам: %+v", c)
	}
}

func TestRelayOwnCopyAcks(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	db := store.DB()
	if _, err := db.Exec(
		"INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at) VALUES ('own1', 'G', ?, 'моё', 'text', ?)",
		me, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := g.addPendingRelay("G", "own1"); err != nil {
		t.Fatal(err)
	}

	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 5, AuthorKey: me, ClientMsgID: "own1", Payload: "моё", ServerTsMs: 1, OriginNode: "n1"})
	var seq int64
	var origin string
	db.QueryRow("SELECT seq, origin_node FROM group_messages WHERE id = 'own1'").Scan(&seq, &origin)
	if seq != 5 || origin != "n1" {
		t.Fatalf("своя копия проставляет seq и журнал: seq=%d origin=%q", seq, origin)
	}
	var pending int
	db.QueryRow("SELECT COUNT(*) FROM group_relay_pending").Scan(&pending)
	if pending != 0 {
		t.Fatal("подтверждённое сообщение не должно ждать повтора")
	}
	// Курсор — граница непрерывно полученного, а не максимум увиденного:
	// seq=5 при курсоре 0 его не двигает, 1..5 добёрет group_sync.
	if c := g.relayCursors("G")["n1"]; c != 0 {
		t.Fatalf("живое сообщение с разрывом не двигает курсор: %d", c)
	}

	// Копия того же сообщения из другого журнала (повтор ушёл на другой
	// узел) не перетирает первую.
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 9, AuthorKey: me, ClientMsgID: "own1", Payload: "моё", ServerTsMs: 2, OriginNode: "n2"})
	db.QueryRow("SELECT seq, origin_node FROM group_messages WHERE id = 'own1'").Scan(&seq, &origin)
	if seq != 5 || origin != "n1" {
		t.Fatalf("повторная копия не перетирает: seq=%d origin=%q", seq, origin)
	}
}

// Сама находка обзора: живое сообщение во время догона не должно уносить
// курсор за неполученную середину — иначе она не запросится больше никогда.
func TestSyncBacklogSurvivesLiveMessage(t *testing.T) {
	g, _, _ := newRelayTestGroups(t)
	author := strings.Repeat("b", 64)
	page := func(seq int64, id string) network.GroupMessageNewPayload {
		return network.GroupMessageNewPayload{GroupID: "G", Seq: seq, AuthorKey: author, ClientMsgID: id, Payload: "p", ServerTsMs: seq * 10, OriginNode: "n1"}
	}
	relaySyncPage(g, "G", true, page(1, "s1"), page(2, "s2"), page(3, "s3"))
	if c := g.relayCursors("G")["n1"]; c != 3 {
		t.Fatalf("страница догона двигает курсор до своего максимума: %d", c)
	}

	// Эхо своего неподтверждённого сообщения из того же журнала.
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 50, AuthorKey: author, ClientMsgID: "live", Payload: "живое", ServerTsMs: 500, OriginNode: "n1"})
	if c := g.relayCursors("G")["n1"]; c != 3 {
		t.Fatalf("живое сообщение не должно уносить курсор за середину: %d", c)
	}
}

func TestLiveCursorAdvancesOnlyContiguously(t *testing.T) {
	g, _, _ := newRelayTestGroups(t)
	author := strings.Repeat("b", 64)
	live := func(seq int64, id string) {
		relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: seq, AuthorKey: author, ClientMsgID: id, Payload: "p", ServerTsMs: seq * 10, OriginNode: "n1"})
	}
	live(1, "a")
	if c := g.relayCursors("G")["n1"]; c != 1 {
		t.Fatalf("первое сообщение журнала двигает курсор: %d", c)
	}
	live(3, "c")
	if c := g.relayCursors("G")["n1"]; c != 1 {
		t.Fatalf("разрыв курсор не двигает: %d", c)
	}
	live(2, "b")
	if c := g.relayCursors("G")["n1"]; c != 2 {
		t.Fatalf("закрытие разрыва двигает курсор: %d", c)
	}
}

// Последняя страница догона (has_more=false) тоже двигает курсор, иначе её
// перекачивали бы при каждом переподключении.
func TestSyncLastPageAdvancesCursor(t *testing.T) {
	g, _, _ := newRelayTestGroups(t)
	author := strings.Repeat("b", 64)
	relaySyncPage(g, "G", false, network.GroupMessageNewPayload{
		GroupID: "G", Seq: 7, AuthorKey: author, ClientMsgID: "last", Payload: "p", ServerTsMs: 70, OriginNode: "n1",
	})
	if c := g.relayCursors("G")["n1"]; c != 7 {
		t.Fatalf("последняя страница двигает курсор: %d", c)
	}
}

// Если сервер выдал seq повторно (после очистки по retention), строку молча
// отбрасывает уникальный индекс — курсор двигать нельзя.
func TestDroppedInsertKeepsCursor(t *testing.T) {
	g, store, _ := newRelayTestGroups(t)
	author := strings.Repeat("b", 64)
	if _, err := store.DB().Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at, seq, origin_node)
		 VALUES ('old', 'G', ?, 'старое', 'text', ?, 1, 'n1')`,
		author, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 1, AuthorKey: author, ClientMsgID: "new", Payload: "новое", ServerTsMs: 10, OriginNode: "n1"})

	var n int
	store.DB().QueryRow("SELECT COUNT(*) FROM group_messages WHERE group_id = 'G'").Scan(&n)
	if n != 1 {
		t.Fatalf("строка с занятым seq не вставляется: %d строк", n)
	}
	if c := g.relayCursors("G")["n1"]; c != 0 {
		t.Fatalf("несохранённое сообщение курсор не двигает: %d", c)
	}
}

func TestResendPendingOfflineKeepsFresh(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	db := store.DB()
	db.Exec("INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at) VALUES ('p1', 'G', ?, 'ждёт', 'text', ?)", me, time.Now().Unix())
	g.addPendingRelay("G", "p1")
	db.Exec("INSERT INTO group_relay_pending (msg_id, group_id, created_at) VALUES ('old', 'G', ?)", time.Now().Add(-8*24*time.Hour).Unix())

	g.resendPendingRelay() // сигналки нет — повтор ждёт переподключения

	var ids []string
	rows, _ := db.Query("SELECT msg_id FROM group_relay_pending ORDER BY msg_id")
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if strings.Join(ids, ",") != "p1" {
		t.Fatalf("свежее остаётся ждать, старше 7 дней забывается: %v", ids)
	}
}

// ownPending — своё неподтверждённое сообщение в группе G, ждущее повтора.
func ownPending(t *testing.T, g *GroupsService, store *storage.Storage, me, id, content string) {
	t.Helper()
	if _, err := store.DB().Exec(
		"INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at) VALUES (?, 'G', ?, ?, 'text', ?)",
		id, me, content, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := g.addPendingRelay("G", id); err != nil {
		t.Fatal(err)
	}
}

func pendingCount(store *storage.Storage, id string) int {
	var n int
	store.DB().QueryRow("SELECT COUNT(*) FROM group_relay_pending WHERE msg_id = ?", id).Scan(&n)
	return n
}

func sendFailedOf(store *storage.Storage, id string) string {
	var code string
	store.DB().QueryRow("SELECT send_failed FROM group_messages WHERE id = ?", id).Scan(&code)
	return code
}

// Удалённое до подтверждения сообщение не уходит повтором: иначе группа
// получила бы уже удалённое.
func TestRetryRelayDropsDeleted(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	ownPending(t, g, store, me, "del1", "секрет")
	store.DB().Exec("UPDATE group_messages SET deleted = 1 WHERE id = 'del1'")

	g.retryRelay("G", "del1", 0)
	if pendingCount(store, "del1") != 0 {
		t.Fatal("удалённое сообщение снимается с повтора")
	}
}

// Строка в очереди больше лимитов сервера (клиент 0078, правка) снимается с
// повтора и помечается: такой кадр сервер рвёт вместе с соединением.
func TestRetryRelayOversizedMarksFailed(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	ownPending(t, g, store, me, "big1", strings.Repeat("я", relayMaxBodyBytes)) // 2 байта на символ

	g.retryRelay("G", "big1", 0)
	if pendingCount(store, "big1") != 0 || sendFailedOf(store, "big1") != relayErrTooLarge {
		t.Fatalf("оверсайз: pending=%d failed=%q", pendingCount(store, "big1"), sendFailedOf(store, "big1"))
	}
}

// Не влезающее сообщение не создаёт строку и не уходит в сеть. Лимит кадра
// проверяется отдельно: «<» в JSON — шесть байт.
func TestSendViaRelayTooLarge(t *testing.T) {
	g, store, _ := newRelayTestGroups(t)
	for name, text := range map[string]string{
		"тело": strings.Repeat("x", relayMaxBodyBytes+1),
		"кадр": strings.Repeat("<", 20*1024),
	} {
		_, err := g.sendViaRelay("G", text, SendOptions{})
		if !errors.Is(err, ErrRelayTooLarge) {
			t.Fatalf("%s: ожидалась ErrRelayTooLarge, получено %v", name, err)
		}
	}
	var n int
	store.DB().QueryRow("SELECT COUNT(*) FROM group_messages").Scan(&n)
	if n != 0 {
		t.Fatalf("строк не создано, а их %d", n)
	}
}

// Окончательный отказ снимает сообщение с повтора и помечает его; временный
// повтор не трогает.
func TestRelaySendErrorClassifies(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	ownPending(t, g, store, me, "rl", "a")
	ownPending(t, g, store, me, "nm", "b")

	g.HandleRelaySendError("G", "rl", "rate_limited")
	if pendingCount(store, "rl") != 1 || sendFailedOf(store, "rl") != "" {
		t.Fatal("rate_limited — временный отказ, повтор продолжается")
	}
	g.HandleRelaySendError("G", "nm", relayErrNotMember)
	if pendingCount(store, "nm") != 0 || sendFailedOf(store, "nm") != relayErrNotMember {
		t.Fatal("not_group_member снимает с повтора и помечает")
	}
	// Запоздавший отказ уже подтверждённого сообщения ничего не помечает.
	store.DB().Exec("UPDATE group_messages SET send_failed = '' WHERE id = 'nm'")
	g.HandleRelaySendError("G", "nm", relayErrNotMember)
	if sendFailedOf(store, "nm") != "" {
		t.Fatal("отказ без ожидающего подтверждения не помечает")
	}
}

// Своя копия, пришедшая после отказа (другой узел, синк), сбрасывает флаг.
func TestOwnCopyClearsSendFailed(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	ownPending(t, g, store, me, "f1", "x")
	g.HandleRelaySendError("G", "f1", relayErrNoGroup)
	relayNew(g, network.GroupMessageNewPayload{GroupID: "G", Seq: 1, AuthorKey: me, ClientMsgID: "f1", Payload: "x", ServerTsMs: 1, OriginNode: "n1"})
	if sendFailedOf(store, "f1") != "" {
		t.Fatal("своя копия после отказа сбрасывает send_failed")
	}
}

// Повтор руками — тот же id, снова в очереди; не влезающее не повторяется.
func TestRetryGroupRelayMessage(t *testing.T) {
	g, store, me := newRelayTestGroups(t)
	ownPending(t, g, store, me, "r1", "x")
	g.HandleRelaySendError("G", "r1", relayErrNotMember)

	if err := g.RetryGroupRelayMessage("r1"); err != nil {
		t.Fatal(err)
	}
	if sendFailedOf(store, "r1") != "" || pendingCount(store, "r1") != 1 {
		t.Fatalf("повтор руками: failed=%q pending=%d", sendFailedOf(store, "r1"), pendingCount(store, "r1"))
	}
	if err := g.RetryGroupRelayMessage("r1"); err == nil {
		t.Fatal("неотказанное сообщение повторять нечего")
	}

	ownPending(t, g, store, me, "r2", strings.Repeat("я", relayMaxBodyBytes))
	g.retryRelay("G", "r2", 0) // пометит как слишком длинное
	if err := g.RetryGroupRelayMessage("r2"); !errors.Is(err, ErrRelayTooLarge) {
		t.Fatalf("слишком длинное не повторяется: %v", err)
	}
}

// Подсказка без соединения с сигналкой не падает.
func TestSyncHintWithoutClient(t *testing.T) {
	g, _, _ := newRelayTestGroups(t)
	raw, _ := json.Marshal(network.GroupSyncHintPayload{GroupID: "G"})
	g.HandleSignalMessage(&network.Message{Type: network.TypeGroupSyncHint, Payload: raw})
}
