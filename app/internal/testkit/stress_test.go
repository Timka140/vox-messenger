package testkit

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// === Stress: DM массовая отправка + offline delivery ===

func TestDMStress(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// 100 online сообщений
	onlineIDs := make([]string, 100)
	for i := 0; i < 100; i++ {
		result, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("online-%d", i))
		if err != nil {
			t.Fatalf("online send %d: %v", i, err)
		}
		if !result.Delivered {
			t.Errorf("online msg %d not delivered", i)
		}
		onlineIDs[i] = result.ID
	}

	// 100 offline сообщений
	env.SetPeerConnected(env.PeerID, false)
	env.ClearSent()

	offlineIDs := make([]string, 100)
	for i := 0; i < 100; i++ {
		result, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("offline-%d", i))
		if err != nil {
			t.Fatalf("offline send %d: %v", i, err)
		}
		if result.Delivered {
			t.Errorf("offline msg %d should not be delivered", i)
		}
		offlineIDs[i] = result.ID
	}

	// Reconnect → deliver
	env.SetPeerConnected(env.PeerID, true)
	env.Messaging.DeliverPendingMessages(env.PeerID)

	// Проверяем: все 200 delivered
	for i, id := range onlineIDs {
		env.AssertMessageDelivered(id, true)
		_ = i
	}
	for i, id := range offlineIDs {
		env.AssertMessageDelivered(id, true)
		_ = i
	}

	// 0 дубликатов
	var total, distinct int
	env.Store.DB().QueryRow("SELECT COUNT(*), COUNT(DISTINCT id) FROM messages WHERE peer_id = ?", env.PeerID).Scan(&total, &distinct)
	if total != distinct {
		t.Errorf("duplicates found: total=%d distinct=%d", total, distinct)
	}
	if total != 200 {
		t.Errorf("expected 200 messages, got %d", total)
	}

	// Порядок
	env.AssertMessageOrder(append(onlineIDs, offlineIDs...)...)
}

// === Stress: группа с несколькими пирами, partial offline ===

func TestGroupStressMultiPeer(t *testing.T) {
	env := NewTestEnv(t, true) // env.PeerID = online
	defer env.Close()

	peer2 := env.AddPeer("Peer2", true)
	peer3 := env.AddPeer("Peer3", false) // offline
	peer4 := env.AddPeer("Peer4", false) // offline

	err := env.Groups.CreateGroup("StressGroup", []string{env.PeerID, peer2, peer3, peer4}, 0)
	if err != nil {
		t.Fatal("create group:", err)
	}

	groups, _ := env.Groups.GetGroups()
	if len(groups) == 0 {
		t.Fatal("no groups")
	}
	groupID := groups[0].GroupID
	env.ClearSent()

	// Отправляем 50 сообщений
	msgIDs := make([]string, 50)
	for i := 0; i < 50; i++ {
		result, err := env.Groups.SendGroupMessage(groupID, fmt.Sprintf("gmsg-%d", i))
		if err != nil {
			t.Fatalf("group send %d: %v", i, err)
		}
		msgIDs[i] = result.ID
	}

	// Online пиры получили
	if env.SentCountTo(env.PeerID) != 50 {
		t.Errorf("peer1 sent count: expected 50, got %d", env.SentCountTo(env.PeerID))
	}
	if env.SentCountTo(peer2) != 50 {
		t.Errorf("peer2 sent count: expected 50, got %d", env.SentCountTo(peer2))
	}
	if env.SentCountTo(peer3) != 0 {
		t.Errorf("peer3 (offline) sent count: expected 0, got %d", env.SentCountTo(peer3))
	}

	// Reconnect peer3 → deliver pending
	env.SetPeerConnected(peer3, true)
	env.ClearSent()
	env.Groups.DeliverPendingGroupMessages(peer3)

	if env.SentCountTo(peer3) != 50 {
		t.Errorf("peer3 after reconnect: expected 50, got %d", env.SentCountTo(peer3))
	}

	// peer4 всё ещё offline
	if env.SentCountTo(peer4) != 0 {
		t.Errorf("peer4 (still offline) sent count: expected 0, got %d", env.SentCountTo(peer4))
	}

	// Unread: мы автор — 0 непрочитанных
	env.AssertGroupUnreadCount(groupID, 0)

	// Проверяем все сообщения в БД
	var msgCount int
	env.Store.DB().QueryRow("SELECT COUNT(*) FROM group_messages WHERE group_id = ?", groupID).Scan(&msgCount)
	if msgCount != 50 {
		t.Errorf("expected 50 group messages, got %d", msgCount)
	}
}

// === Stress: параллельный доступ к БД ===

// TestSequentialDBStress — интенсивная последовательная нагрузка на БД.
// Concurrent write stress невозможен с in-memory SQLite (cache=shared не поддерживает WAL полноценно).
// В production (файловая БД + WAL + busy_timeout) concurrent writes работают корректно.
func TestSequentialDBStress(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// 200 циклов: send → getConversations → markRead
	for i := 0; i < 200; i++ {
		_, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("seq-msg-%d", i))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}

		convs, err := env.Messaging.GetConversations()
		if err != nil {
			t.Fatalf("getconv %d: %v", i, err)
		}
		if len(convs) == 0 {
			t.Fatalf("no conversations at iteration %d", i)
		}

		if err := env.Messaging.MarkMessagesRead(env.PeerID); err != nil {
			t.Fatalf("markread %d: %v", i, err)
		}
	}

	// Все 200 сообщений в БД
	var total int
	env.Store.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE peer_id = ?", env.PeerID).Scan(&total)
	if total != 200 {
		t.Errorf("expected 200 messages, got %d", total)
	}

	// Все прочитаны
	var unread int
	env.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND read = 0",
		env.PeerID).Scan(&unread)
	// Свои сообщения всегда read=1, входящих нет → unread = 0
	if unread != 0 {
		t.Errorf("expected 0 unread, got %d", unread)
	}

	// Concurrent read test: 5 горутин читают одновременно (reads не блокируют друг друга в WAL)
	var wg sync.WaitGroup
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := env.Messaging.GetConversations()
				if err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// === Stress: граничные значения входных данных ===

func TestInputBoundary(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Пустой peerID
	_, err := env.Messaging.SendMessage("", "hello")
	if err == nil {
		t.Error("empty peerID should fail")
	}

	// Невалидный hex
	_, err = env.Messaging.SendMessage("ZZZZ_not_hex_at_all_padding_to_64_chars_0000000000000000000000", "hello")
	if err == nil {
		t.Error("invalid hex peerID should fail")
	}

	// Короткий peerID
	_, err = env.Messaging.SendMessage("abcd", "hello")
	if err == nil {
		t.Error("short peerID should fail")
	}

	// Пустое сообщение
	_, err = env.Messaging.SendMessage(env.PeerID, "")
	if err == nil {
		t.Error("empty message should fail")
	}

	// 1MB текст — должен пройти
	bigText := make([]byte, 1_000_000)
	for i := range bigText {
		bigText[i] = 'A'
	}
	_, err = env.Messaging.SendMessage(env.PeerID, string(bigText))
	if err != nil {
		t.Errorf("1MB message should succeed: %v", err)
	}

	// 1MB+1 текст — должен упасть
	tooBig := make([]byte, 1_000_001)
	for i := range tooBig {
		tooBig[i] = 'B'
	}
	_, err = env.Messaging.SendMessage(env.PeerID, string(tooBig))
	if err == nil {
		t.Error("1MB+1 message should fail")
	}

	// Группа: имя 101 символ
	longName := make([]byte, 101)
	for i := range longName {
		longName[i] = 'G'
	}
	err = env.Groups.CreateGroup(string(longName), []string{env.PeerID}, 0)
	if err == nil {
		t.Error("group name 101 chars should fail")
	}

	// Группа: имя 100 символов — OK
	okName := make([]byte, 100)
	for i := range okName {
		okName[i] = 'G'
	}
	err = env.Groups.CreateGroup(string(okName), []string{env.PeerID}, 0)
	if err != nil {
		t.Errorf("group name 100 chars should succeed: %v", err)
	}

	// EditMessage: пустой текст
	err = env.Messaging.EditMessage("some-id", env.PeerID, "", "")
	if err == nil {
		t.Error("edit with empty text should fail")
	}
}

// === Stress: reconnect storm ===

func TestReconnectStorm(t *testing.T) {
	env := NewTestEnv(t, false) // offline
	defer env.Close()

	// Отправляем 10 сообщений offline
	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		result, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("storm-%d", i))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ids[i] = result.ID
	}

	// 20 быстрых reconnect циклов
	for cycle := 0; cycle < 20; cycle++ {
		env.SetPeerConnected(env.PeerID, true)
		env.Messaging.DeliverPendingMessages(env.PeerID)
		env.SetPeerConnected(env.PeerID, false)
	}

	// Финальный reconnect
	env.SetPeerConnected(env.PeerID, true)
	env.Messaging.DeliverPendingMessages(env.PeerID)

	// Все 10 должны быть delivered
	for _, id := range ids {
		env.AssertMessageDelivered(id, true)
	}

	// Проверяем 0 дубликатов в БД
	var total, distinct int
	env.Store.DB().QueryRow("SELECT COUNT(*), COUNT(DISTINCT id) FROM messages WHERE peer_id = ?", env.PeerID).Scan(&total, &distinct)
	if total != distinct {
		t.Errorf("duplicates after storm: total=%d distinct=%d", total, distinct)
	}
}

// === Stress: утечки памяти при циклах create/delete ===

func TestMemoryStability(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Замеряем начальную память
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	for cycle := 0; cycle < 50; cycle++ {
		// Создаём группу
		err := env.Groups.CreateGroup(fmt.Sprintf("MemTest-%d", cycle), []string{env.PeerID}, 0)
		if err != nil {
			t.Fatalf("cycle %d create: %v", cycle, err)
		}

		groups, _ := env.Groups.GetGroups()
		var groupID string
		for _, g := range groups {
			if g.Name == fmt.Sprintf("MemTest-%d", cycle) {
				groupID = g.GroupID
				break
			}
		}
		if groupID == "" {
			t.Fatalf("cycle %d: group not found", cycle)
			continue
		}

		// Отправляем 5 сообщений
		for j := 0; j < 5; j++ {
			_, err := env.Groups.SendGroupMessage(groupID, fmt.Sprintf("mem-c%d-m%d", cycle, j))
			if err != nil {
				t.Fatalf("cycle %d msg %d: %v", cycle, j, err)
			}
		}

		// Удаляем группу (мы owner)
		err = env.Groups.DeleteGroupAsAdmin(groupID)
		if err != nil {
			t.Fatalf("cycle %d delete: %v", cycle, err)
		}
	}

	// Замеряем финальную память
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// Допуск: финальная ≤ 3× начальная (GC не детерминирован)
	if memAfter.Alloc > memBefore.Alloc*3 {
		t.Errorf("memory grew too much: before=%d MB, after=%d MB",
			memBefore.Alloc/1024/1024, memAfter.Alloc/1024/1024)
	}

	// Проверяем что все группы удалены
	var groupCount int
	env.Store.DB().QueryRow("SELECT COUNT(*) FROM groups").Scan(&groupCount)
	if groupCount != 0 {
		t.Errorf("expected 0 groups after cleanup, got %d", groupCount)
	}
}
