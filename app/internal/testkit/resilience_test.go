package testkit

import (
	"encoding/json"
	"fmt"
	"vox_app/internal/services"
	"sync/atomic"
	"testing"
	"time"
)

// === Resilience: sender failure mid-delivery ===

func TestSenderFailureMidDelivery(t *testing.T) {
	env := NewTestEnv(t, false) // offline
	defer env.Close()

	// Отправляем 10 сообщений offline
	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		result, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("pending-%d", i))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ids[i] = result.ID
	}

	// Внедряем sender, который падает после 5 успешных отправок
	var sendCount atomic.Int32
	env.SetCustomSender(func(pid string, data []byte) error {
		n := sendCount.Add(1)
		if n > 5 {
			return fmt.Errorf("connection reset by peer")
		}
		return nil // success
	})

	// Reconnect + deliver — часть доставится, часть нет
	env.SetPeerConnected(env.PeerID, true)
	env.Messaging.DeliverPendingMessages(env.PeerID)

	// Считаем сколько доставлено
	var delivered int
	for _, id := range ids {
		var d int
		env.Store.DB().QueryRow("SELECT delivered FROM messages WHERE id = ?", id).Scan(&d)
		if d == 1 {
			delivered++
		}
	}

	if delivered == 0 {
		t.Error("expected some messages delivered before failure")
	}
	if delivered == 10 {
		t.Error("expected some messages NOT delivered after failure")
	}
	t.Logf("partial delivery: %d/10 delivered", delivered)

	// Восстанавливаем sender и дожимаем остаток
	env.SetCustomSender(nil)
	env.Messaging.DeliverPendingMessages(env.PeerID)

	for _, id := range ids {
		env.AssertMessageDelivered(id, true)
	}
}

// === Resilience: slow sender (задержка на отправку) ===

func TestSenderSlowResponse(t *testing.T) {
	env := NewTestEnv(t, false) // offline
	defer env.Close()

	// Отправляем 20 сообщений offline
	ids := make([]string, 20)
	for i := 0; i < 20; i++ {
		result, err := env.Messaging.SendMessage(env.PeerID, fmt.Sprintf("slow-%d", i))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ids[i] = result.ID
	}

	// Sender с задержкой 10ms (суммарно ~200ms на 20 сообщений)
	env.SetCustomSender(func(pid string, data []byte) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	env.SetPeerConnected(env.PeerID, true)
	start := time.Now()
	env.Messaging.DeliverPendingMessages(env.PeerID)
	elapsed := time.Since(start)

	// Все доставлены
	for _, id := range ids {
		env.AssertMessageDelivered(id, true)
	}

	// Должно завершиться за разумное время (<5s)
	if elapsed > 5*time.Second {
		t.Errorf("slow delivery took too long: %v", elapsed)
	}
	t.Logf("slow delivery: 20 messages in %v", elapsed)

	// Восстанавливаем
	env.SetCustomSender(nil)
}

// === Resilience: partial group delivery failure ===

func TestGroupPartialDeliveryFailure(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	peer2 := env.AddPeer("Peer2", true)
	peer3 := env.AddPeer("Peer3", true)

	err := env.Groups.CreateGroup("PartialFail", []string{env.PeerID, peer2, peer3}, 0)
	if err != nil {
		t.Fatal("create group:", err)
	}

	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	// Внедряем sender: peer3 начинает "падать" после 5 сообщений
	var msgCount atomic.Int32
	failingPeer := peer3
	env.SetCustomSender(func(pid string, data []byte) error {
		if pid == failingPeer {
			n := msgCount.Add(1)
			if n > 5 {
				return fmt.Errorf("peer %s disconnected", pid[:16])
			}
		}
		// Записываем в sent для подсчёта
		var msg struct{ Type string `json:"type"` }
		json.Unmarshal(data, &msg)
		env.mu.Lock()
		env.sent = append(env.sent, SentMessage{To: pid, Data: data, Type: msg.Type})
		env.mu.Unlock()
		return nil
	})

	env.ClearSent()

	// Отправляем 10 сообщений
	for i := 0; i < 10; i++ {
		_, err := env.Groups.SendGroupMessage(groupID, fmt.Sprintf("partial-%d", i))
		if err != nil {
			t.Fatalf("group send %d: %v", i, err)
		}
	}

	// env.PeerID и peer2 должны получить все 10
	sentToPeer1 := env.SentCountTo(env.PeerID)
	sentToPeer2 := env.SentCountTo(peer2)
	sentToPeer3 := env.SentCountTo(peer3)

	if sentToPeer1 != 10 {
		t.Errorf("peer1 received %d, expected 10", sentToPeer1)
	}
	if sentToPeer2 != 10 {
		t.Errorf("peer2 received %d, expected 10", sentToPeer2)
	}
	// peer3 получил 5 (до failure)
	if sentToPeer3 != 5 {
		t.Errorf("peer3 received %d, expected 5", sentToPeer3)
	}

	// Восстанавливаем
	env.SetCustomSender(nil)
}

// === Resilience: rapid group leave/rejoin during messaging ===

func TestRapidGroupJoinLeave(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	err := env.Groups.CreateGroup("RapidJoinLeave", []string{env.PeerID}, 0)
	if err != nil {
		t.Fatal("create group:", err)
	}
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	sentBefore := 0

	for cycle := 0; cycle < 5; cycle++ {
		// Send while active
		_, err := env.Groups.SendGroupMessage(groupID, fmt.Sprintf("before-leave-%d", cycle))
		if err != nil {
			t.Fatalf("cycle %d send before leave: %v", cycle, err)
		}
		sentBefore++

		// Leave
		env.Groups.LeaveGroup(groupID)

		// Send while left — should fail
		_, err = env.Groups.SendGroupMessage(groupID, fmt.Sprintf("while-left-%d", cycle))
		if err == nil {
			t.Errorf("cycle %d: send should fail while left", cycle)
		}

		// Rejoin
		env.Groups.RejoinGroup(groupID)

		// Send after rejoin
		_, err = env.Groups.SendGroupMessage(groupID, fmt.Sprintf("after-rejoin-%d", cycle))
		if err != nil {
			t.Fatalf("cycle %d send after rejoin: %v", cycle, err)
		}
		sentBefore++
	}

	// Проверяем что все успешные сообщения в БД
	var msgCount int
	env.Store.DB().QueryRow("SELECT COUNT(*) FROM group_messages WHERE group_id = ?", groupID).Scan(&msgCount)
	if msgCount != sentBefore {
		t.Errorf("expected %d messages, got %d", sentBefore, msgCount)
	}
}

// === Resilience: message deduplication ===

func TestMessageDeduplication(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Отправляем одно и то же сообщение дважды через P2P (имитация сетевого дубликата)
	msgID := "dedup-test-id-12345"
	now := time.Now().Unix()
	p2pMsg := services.P2PMessage{
		Type:    "message",
		ID:      msgID,
		Content: "original message",
		Time:    now,
	}
	data, _ := json.Marshal(p2pMsg)

	// Первый раз
	env.Network.TestTriggerP2PMessage(env.PeerID, data)

	// Второй раз — дубликат
	env.Network.TestTriggerP2PMessage(env.PeerID, data)

	// Должна быть ровно 1 запись
	var count int
	env.Store.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", msgID).Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 message, got %d (dedup failed)", count)
	}

	// Event должен был emit-иться только 1 раз
	events := env.GetEvents()
	msgEvents := 0
	for _, ev := range events {
		if ev.Name == "vox:message" {
			msgEvents++
		}
	}
	if msgEvents != 1 {
		t.Errorf("expected 1 nexus:message event, got %d", msgEvents)
	}
}

// === Resilience: large group fanout (20 participants) ===

func TestLargeGroupFanout(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Создаём 19 дополнительных пиров (10 online, 9 offline + env.PeerID online = 10+10)
	onlinePeers := []string{env.PeerID}
	offlinePeers := make([]string, 0)

	for i := 0; i < 9; i++ {
		pid := env.AddPeer(fmt.Sprintf("Online-%d", i), true)
		onlinePeers = append(onlinePeers, pid)
	}
	for i := 0; i < 10; i++ {
		pid := env.AddPeer(fmt.Sprintf("Offline-%d", i), false)
		offlinePeers = append(offlinePeers, pid)
	}

	allPeers := append(append([]string{}, onlinePeers...), offlinePeers...)
	err := env.Groups.CreateGroup("LargeGroup", allPeers, 0)
	if err != nil {
		t.Fatal("create group:", err)
	}

	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearSent()

	// Отправляем 30 сообщений
	for i := 0; i < 30; i++ {
		_, err := env.Groups.SendGroupMessage(groupID, fmt.Sprintf("large-%d", i))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Online пиры получили все 30
	for _, pid := range onlinePeers {
		count := env.SentCountTo(pid)
		if count != 30 {
			t.Errorf("online peer %s received %d, expected 30", safeShortTest(pid), count)
		}
	}

	// Offline пиры получили 0
	for _, pid := range offlinePeers {
		count := env.SentCountTo(pid)
		if count != 0 {
			t.Errorf("offline peer %s received %d, expected 0", safeShortTest(pid), count)
		}
	}

	// Reconnect всех offline
	env.ClearSent()
	for _, pid := range offlinePeers {
		env.SetPeerConnected(pid, true)
		env.Groups.DeliverPendingGroupMessages(pid)
	}

	// Все offline получили свои 30
	for _, pid := range offlinePeers {
		count := env.SentCountTo(pid)
		if count != 30 {
			t.Errorf("reconnected peer %s received %d, expected 30", safeShortTest(pid), count)
		}
	}

	t.Logf("large fanout: 20 peers × 30 messages = %d total deliveries verified",
		len(onlinePeers)*30+len(offlinePeers)*30)
}
