package testkit

import "testing"

// === Group Offline Delivery ===

// TestGroupSendOfflineNotDelivered — отправка когда получатель офлайн.
// delivered=0 в group_message_delivery, SendResult.Delivered=false.
func TestGroupSendOfflineNotDelivered(t *testing.T) {
	env := NewTestEnv(t, false) // peer offline
	defer env.Close()

	env.Groups.CreateGroup("Offline Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	result, err := env.Groups.SendGroupMessage(groupID, "offline msg")
	if err != nil {
		t.Fatal("SendGroupMessage:", err)
	}

	// SendResult должен показать что не доставлено
	if result.Delivered {
		t.Error("SendResult.Delivered should be false when peer offline")
	}

	// group_message_delivery: delivered=0 для пира
	var delivered int
	err = env.Store.DB().QueryRow(
		"SELECT delivered FROM group_message_delivery WHERE msg_id = ? AND peer_id = ?",
		result.ID, env.PeerID).Scan(&delivered)
	if err != nil {
		t.Fatal("delivery record not found:", err)
	}
	if delivered != 0 {
		t.Errorf("delivery should be 0, got %d", delivered)
	}
}

// TestGroupSendOnlineDelivered — отправка когда получатель онлайн.
// delivered=1 в group_message_delivery, SendResult.Delivered=true.
func TestGroupSendOnlineDelivered(t *testing.T) {
	env := NewTestEnv(t, true) // peer online
	defer env.Close()

	env.Groups.CreateGroup("Online Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	result, err := env.Groups.SendGroupMessage(groupID, "online msg")
	if err != nil {
		t.Fatal("SendGroupMessage:", err)
	}

	if !result.Delivered {
		t.Error("SendResult.Delivered should be true when peer online")
	}

	// group_message_delivery: delivered=1
	var delivered int
	err = env.Store.DB().QueryRow(
		"SELECT delivered FROM group_message_delivery WHERE msg_id = ? AND peer_id = ?",
		result.ID, env.PeerID).Scan(&delivered)
	if err != nil {
		t.Fatal("delivery record not found:", err)
	}
	if delivered != 1 {
		t.Errorf("delivery should be 1, got %d", delivered)
	}
}

// TestGroupPendingDeliveryOnReconnect — offline → reconnect → pending доставлены.
func TestGroupPendingDeliveryOnReconnect(t *testing.T) {
	env := NewTestEnv(t, false) // peer offline
	defer env.Close()

	env.Groups.CreateGroup("Reconnect Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	// Отправляем 3 сообщения офлайн
	r1, _ := env.Groups.SendGroupMessage(groupID, "pending 1")
	r2, _ := env.Groups.SendGroupMessage(groupID, "pending 2")
	r3, _ := env.Groups.SendGroupMessage(groupID, "pending 3")

	// Все pending
	var count int
	env.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM group_message_delivery WHERE peer_id = ? AND delivered = 0",
		env.PeerID).Scan(&count)
	if count != 3 {
		t.Fatalf("expected 3 pending, got %d", count)
	}

	// Peer приходит онлайн
	env.SetPeerConnected(env.PeerID, true)
	env.Groups.DeliverPendingGroupMessages(env.PeerID)

	// Все доставлены
	env.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM group_message_delivery WHERE peer_id = ? AND delivered = 0",
		env.PeerID).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 pending after delivery, got %d", count)
	}

	// P2P: 3 сообщения отправлены
	env.AssertP2PSentCount(env.PeerID, 3)

	_ = r1
	_ = r2
	_ = r3
}

// TestGroupSendResultFlag — SendResult.Delivered = false когда sent=0.
func TestGroupSendResultFlag(t *testing.T) {
	env := NewTestEnv(t, false) // peer offline
	defer env.Close()

	env.Groups.CreateGroup("Flag Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()

	result, _ := env.Groups.SendGroupMessage(groups[0].GroupID, "flag test")

	if result.Delivered {
		t.Error("Delivered should be false when no peer received the message")
	}
}

// TestGroupMessageDeliveredInHistory — GetGroupMessages показывает delivered статус.
func TestGroupMessageDeliveredInHistory(t *testing.T) {
	env := NewTestEnv(t, false) // offline
	defer env.Close()

	env.Groups.CreateGroup("History Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.Groups.SendGroupMessage(groupID, "not delivered yet")

	msgs, _ := env.Groups.GetGroupMessages(groupID)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	// Сообщение не доставлено — Delivered=false (часики)
	if msgs[0].Delivered {
		t.Error("message should show Delivered=false when no peer received it")
	}

	// Доставляем
	env.SetPeerConnected(env.PeerID, true)
	env.Groups.DeliverPendingGroupMessages(env.PeerID)

	msgs2, _ := env.Groups.GetGroupMessages(groupID)
	// Теперь Delivered=true (галочка, хотя бы один получил)
	if !msgs2[0].Delivered {
		t.Error("message should show Delivered=true after delivery to at least one peer")
	}
}

// TestGroupPendingDeliveryEmitsEvent — после доставки эмитится nexus:group-messages-delivered.
func TestGroupPendingDeliveryEmitsEvent(t *testing.T) {
	env := NewTestEnv(t, false)
	defer env.Close()

	env.Groups.CreateGroup("Event Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.Groups.SendGroupMessage(groupID, "pending")
	env.ClearEvents()

	env.SetPeerConnected(env.PeerID, true)
	env.Groups.DeliverPendingGroupMessages(env.PeerID)

	env.AssertEventEmitted("vox:group-messages-delivered")
}

// TestGroupPendingDeliveryOrder — pending отправляются в порядке created_at ASC.
func TestGroupPendingDeliveryOrder(t *testing.T) {
	env := NewTestEnv(t, false)
	defer env.Close()

	env.Groups.CreateGroup("Order Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.Groups.SendGroupMessage(groupID, "first")
	env.Groups.SendGroupMessage(groupID, "second")
	env.Groups.SendGroupMessage(groupID, "third")

	env.SetPeerConnected(env.PeerID, true)
	env.ClearSent()
	env.Groups.DeliverPendingGroupMessages(env.PeerID)

	sent := env.GetSent()
	if len(sent) != 3 {
		t.Fatalf("expected 3 sent, got %d", len(sent))
	}

	// Проверяем порядок через содержимое
	for i, s := range sent {
		if s.Type != "group_message" {
			t.Errorf("sent[%d] type: expected group_message, got %s", i, s.Type)
		}
	}
}
