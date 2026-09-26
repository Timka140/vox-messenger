package testkit

import (
	"strings"
	"testing"
)

// === Groups: Создание и участники ===

func TestGroupCreate(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	err := env.Groups.CreateGroup("Test Group", []string{env.PeerID}, 0)
	if err != nil {
		t.Fatal("CreateGroup:", err)
	}

	groups, err := env.Groups.GetGroups()
	if err != nil {
		t.Fatal("GetGroups:", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].Name != "Test Group" {
		t.Errorf("group name: expected 'Test Group', got '%s'", groups[0].Name)
	}
	if groups[0].Members != 2 {
		t.Errorf("member count: expected 2, got %d", groups[0].Members)
	}
	if groups[0].CreatorID != env.MyID {
		t.Error("creatorId should be myID")
	}
}

func TestGroupCreateSendsInvite(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Invite Test", []string{env.PeerID}, 0)

	env.AssertP2PSent(env.PeerID, "group_invite")
}

func TestGroupGetMembers(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Members Test", []string{env.PeerID}, 0)

	groups, _ := env.Groups.GetGroups()
	members, err := env.Groups.GetGroupMembers(groups[0].GroupID)
	if err != nil {
		t.Fatal("GetGroupMembers:", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}

	// Создатель должен быть admin
	foundAdmin := false
	for _, m := range members {
		if m.PeerID == env.MyID && m.Role == "admin" {
			foundAdmin = true
		}
	}
	if !foundAdmin {
		t.Error("creator should have role 'admin'")
	}
}

// === Groups: Сообщения ===

func TestGroupSendMessage(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Msg Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.ClearSent()

	result, err := env.Groups.SendGroupMessage(groupID, "hello group")
	if err != nil {
		t.Fatal("SendGroupMessage:", err)
	}
	if result.ID == "" {
		t.Error("result.ID is empty")
	}

	// P2P: отправлено участнику
	env.AssertP2PSent(env.PeerID, "group_message")

	// БД: сообщение сохранено
	env.AssertGroupMessageExists(result.ID)
}

func TestGroupMessageDelivered(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Delivered Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.Groups.SendGroupMessage(groupID, "test msg")

	msgs, err := env.Groups.GetGroupMessages(groupID)
	if err != nil {
		t.Fatal("GetGroupMessages:", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !msgs[0].Delivered {
		t.Error("group message should always have Delivered=true")
	}
}

func TestGroupIncomingMessage(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Incoming Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	msgID := env.SendGroupFromPeer(groupID, "hello from peer")

	env.AssertGroupMessageExists(msgID)
	env.AssertEventEmitted("vox:group-message")
}

func TestGroupMessageOrder(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Order Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	r1, _ := env.Groups.SendGroupMessage(groupID, "first")
	r2, _ := env.Groups.SendGroupMessage(groupID, "second")
	r3, _ := env.Groups.SendGroupMessage(groupID, "third")

	msgs, _ := env.Groups.GetGroupMessages(groupID)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	// GetGroupMessages возвращает от старых к новым
	if msgs[0].ID != r1.ID || msgs[1].ID != r2.ID || msgs[2].ID != r3.ID {
		t.Error("messages not in created order")
	}
}

// === Groups: Непрочитанные ===

func TestGroupUnreadCount(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Unread Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.SendGroupFromPeer(groupID, "msg1")
	env.SendGroupFromPeer(groupID, "msg2")

	env.AssertGroupUnreadCount(groupID, 2)
}

func TestGroupUnreadResetOnRead(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Unread Reset", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.SendGroupFromPeer(groupID, "msg1")
	env.SendGroupFromPeer(groupID, "msg2")

	env.AssertGroupUnreadCount(groupID, 2)

	env.Groups.MarkGroupRead(groupID)

	env.AssertGroupUnreadCount(groupID, 0)
}

func TestGroupUnreadOwnMessagesIgnored(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Own Unread", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.Groups.SendGroupMessage(groupID, "my msg 1")
	env.Groups.SendGroupMessage(groupID, "my msg 2")

	env.AssertGroupUnreadCount(groupID, 0)
}

// === Groups: Управление ===

func TestGroupKickMember(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Kick Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	env.AssertGroupMemberCount(groupID, 2)
	env.ClearSent()

	err := env.Groups.KickGroupMember(groupID, env.PeerID)
	if err != nil {
		t.Fatal("KickGroupMember:", err)
	}

	env.AssertGroupMemberNotExists(groupID, env.PeerID)
	env.AssertGroupMemberCount(groupID, 1)
	env.AssertP2PSent(env.PeerID, "group_kick")
}

func TestGroupKickNonAdminFails(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Создаём группу где пир — создатель (мы не admin)
	// Имитируем: создаём группу от имени пира (вставляем вручную)
	groupID := "test-group-non-admin"
	env.Store.DB().Exec("INSERT INTO groups (group_id, name, owner_id, type) VALUES (?, 'NonAdmin', ?, 0)", groupID, env.PeerID)
	env.Store.DB().Exec("INSERT INTO group_members (group_id, peer_id, role) VALUES (?, ?, 'member')", groupID, env.MyID)
	env.Store.DB().Exec("INSERT INTO group_members (group_id, peer_id, role) VALUES (?, ?, 'admin')", groupID, env.PeerID)

	err := env.Groups.KickGroupMember(groupID, env.PeerID)
	if err == nil {
		t.Error("KickGroupMember should fail for non-admin")
	}
}

func TestGroupLeave(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Leave Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearSent()

	env.Groups.LeaveGroup(groupID)

	// LeaveGroup помечает status='left' (не удаляет — можно вернуться)
	var status string
	env.Store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, env.MyID).Scan(&status)
	if status != "left" {
		t.Errorf("expected status 'left', got '%s'", status)
	}
	env.AssertP2PSent(env.PeerID, "group_leave")
}

func TestGroupDelete(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Delete Test", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearSent()

	err := env.Groups.DeleteGroupAsAdmin(groupID)
	if err != nil {
		t.Fatal("DeleteGroupAsAdmin:", err)
	}

	// Группа удалена
	groups2, _ := env.Groups.GetGroups()
	if len(groups2) != 0 {
		t.Errorf("expected 0 groups after delete, got %d", len(groups2))
	}

	env.AssertP2PSent(env.PeerID, "group_delete")
}

func TestGroupDeleteNonOwnerFails(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	groupID := "test-group-not-mine"
	env.Store.DB().Exec("INSERT INTO groups (group_id, name, owner_id, type) VALUES (?, 'NotMine', ?, 0)", groupID, env.PeerID)
	env.Store.DB().Exec("INSERT INTO group_members (group_id, peer_id, role) VALUES (?, ?, 'member')", groupID, env.MyID)

	err := env.Groups.DeleteGroupAsAdmin(groupID)
	if err == nil {
		t.Error("DeleteGroupAsAdmin should fail for non-owner")
	}
}

func TestGroupRename(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Old Name", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearSent()

	err := env.Groups.RenameGroup(groupID, "New Name")
	if err != nil {
		t.Fatal("RenameGroup:", err)
	}

	groups2, _ := env.Groups.GetGroups()
	if groups2[0].Name != "New Name" {
		t.Errorf("expected 'New Name', got '%s'", groups2[0].Name)
	}

	env.AssertP2PSent(env.PeerID, "group_rename")
}

// === Groups: Invite/Kick handling (входящие P2P) ===

func TestGroupInviteCreatesGroup(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Content для group_invite — это JSON-строка (не вложенный объект)
	invitePayload := `{"groupId":"invited-group","name":"Invited","creatorId":"` + env.PeerID + `","members":["` + env.PeerID + `","` + env.MyID + `"],"type":0}`
	// Экранируем JSON внутри строки
	escapedPayload := `"` + strings.ReplaceAll(strings.ReplaceAll(invitePayload, `\`, `\\`), `"`, `\"`) + `"`
	env.Network.TestTriggerP2PMessage(env.PeerID, []byte(`{"type":"group_invite","id":"invited-group","groupId":"invited-group","content":`+escapedPayload+`,"time":1234567890}`))

	groups, _ := env.Groups.GetGroups()
	if len(groups) != 1 {
		t.Fatalf("expected 1 group after invite, got %d", len(groups))
	}
	if groups[0].Name != "Invited" {
		t.Errorf("group name: expected 'Invited', got '%s'", groups[0].Name)
	}

	env.AssertGroupMemberCount("invited-group", 2)
	env.AssertEventEmitted("vox:group-invite")
}

func TestGroupKickRemovesSelf(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	// Создаём группу (мы в ней)
	env.Groups.CreateGroup("Kick Me", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	// Отправитель kick'а должен быть админом — иначе команда игнорируется
	// (авторизация входящих group_* команд). Делаем env.PeerID админом.
	if _, err := env.Store.DB().Exec(
		"UPDATE group_members SET role = 'admin' WHERE group_id = ? AND peer_id = ?",
		groupID, env.PeerID); err != nil {
		t.Fatalf("setup admin: %v", err)
	}

	env.Network.TestTriggerP2PMessage(env.PeerID, []byte(`{"type":"group_kick","id":"kick1","groupId":"`+groupID+`","time":1234567890}`))

	env.AssertGroupMemberNotExists(groupID, env.MyID)
	env.AssertEventEmitted("vox:group-kicked")
}

// TestGroupKickFromNonAdminIgnored — kick от рядового участника не должен
// удалять группу локально (griefing-защита).
func TestGroupKickFromNonAdminIgnored(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Keep Me", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearEvents()

	// env.PeerID — обычный member (не админ). Kick должен быть проигнорирован.
	env.Network.TestTriggerP2PMessage(env.PeerID, []byte(`{"type":"group_kick","id":"kick1","groupId":"`+groupID+`","time":1234567890}`))

	env.AssertGroupMemberExists(groupID, env.MyID)
}

func TestGroupDeleteRemovesAll(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Delete Me", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID

	// group_delete принимается только от владельца группы. Делаем env.PeerID
	// владельцем.
	if _, err := env.Store.DB().Exec(
		"UPDATE groups SET owner_id = ? WHERE group_id = ?", env.PeerID, groupID); err != nil {
		t.Fatalf("setup owner: %v", err)
	}
	env.ClearEvents()

	env.Network.TestTriggerP2PMessage(env.PeerID, []byte(`{"type":"group_delete","id":"del1","groupId":"`+groupID+`","time":1234567890}`))

	groups2, _ := env.Groups.GetGroups()
	if len(groups2) != 0 {
		t.Errorf("expected 0 groups after delete, got %d", len(groups2))
	}
	env.AssertEventEmitted("vox:group-deleted")
}

// TestGroupDeleteFromNonOwnerIgnored — delete от не-владельца игнорируется.
func TestGroupDeleteFromNonOwnerIgnored(t *testing.T) {
	env := NewTestEnv(t, true)
	defer env.Close()

	env.Groups.CreateGroup("Keep Me", []string{env.PeerID}, 0)
	groups, _ := env.Groups.GetGroups()
	groupID := groups[0].GroupID
	env.ClearEvents()

	// env.PeerID — не владелец (владелец — локальный env.MyID). Delete игнор.
	env.Network.TestTriggerP2PMessage(env.PeerID, []byte(`{"type":"group_delete","id":"del1","groupId":"`+groupID+`","time":1234567890}`))

	groups2, _ := env.Groups.GetGroups()
	if len(groups2) != 1 {
		t.Errorf("expected group to survive non-owner delete, got %d groups", len(groups2))
	}
}
