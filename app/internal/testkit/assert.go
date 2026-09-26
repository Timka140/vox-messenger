package testkit

import "testing"

func safeShortTest(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func safeShort8Test(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// --- DB assertions ---

func (e *TestEnv) AssertMessageDelivered(msgID string, expected bool) {
	e.T.Helper()
	var delivered int
	err := e.Store.DB().QueryRow("SELECT delivered FROM messages WHERE id = ?", msgID).Scan(&delivered)
	if err != nil {
		e.T.Fatalf("AssertMessageDelivered(%s): query error: %v", msgID, err)
	}
	actual := delivered == 1
	if actual != expected {
		e.T.Errorf("AssertMessageDelivered(%s): expected %v, got %v (delivered=%d)", msgID, expected, actual, delivered)
	}
}

func (e *TestEnv) AssertMessageRead(msgID string, expected bool) {
	e.T.Helper()
	var read int
	err := e.Store.DB().QueryRow("SELECT read FROM messages WHERE id = ?", msgID).Scan(&read)
	if err != nil {
		e.T.Fatalf("AssertMessageRead(%s): query error: %v", msgID, err)
	}
	actual := read == 1
	if actual != expected {
		e.T.Errorf("AssertMessageRead(%s): expected %v, got %v", msgID, expected, actual)
	}
}

func (e *TestEnv) AssertMessageExists(msgID string) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", msgID).Scan(&count)
	if count == 0 {
		e.T.Errorf("AssertMessageExists(%s): message not found", msgID)
	}
}

func (e *TestEnv) AssertMessageNotExists(msgID string) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE id = ?", msgID).Scan(&count)
	if count != 0 {
		e.T.Errorf("AssertMessageNotExists(%s): message still exists", msgID)
	}
}

func (e *TestEnv) AssertUnreadCount(peerID string, expected int) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND sender_id != ? AND read = 0 AND deleted = 0",
		peerID, e.MyID).Scan(&count)
	if count != expected {
		e.T.Errorf("AssertUnreadCount(%s): expected %d, got %d", safeShortTest(peerID), expected, count)
	}
}

func (e *TestEnv) AssertGroupUnreadCount(groupID string, expected int) {
	e.T.Helper()
	groups, err := e.Groups.GetGroups()
	if err != nil {
		e.T.Fatalf("AssertGroupUnreadCount: GetGroups error: %v", err)
	}
	for _, g := range groups {
		if g.GroupID == groupID {
			if g.Unread != expected {
				e.T.Errorf("AssertGroupUnreadCount(%s): expected %d, got %d", safeShort8Test(groupID), expected, g.Unread)
			}
			return
		}
	}
	e.T.Errorf("AssertGroupUnreadCount(%s): group not found", safeShort8Test(groupID))
}

func (e *TestEnv) AssertGroupMemberCount(groupID string, expected int) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM group_members WHERE group_id = ?", groupID).Scan(&count)
	if count != expected {
		e.T.Errorf("AssertGroupMemberCount(%s): expected %d, got %d", safeShort8Test(groupID), expected, count)
	}
}

func (e *TestEnv) AssertGroupMemberExists(groupID, peerID string) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM group_members WHERE group_id = ? AND peer_id = ?", groupID, peerID).Scan(&count)
	if count == 0 {
		e.T.Errorf("AssertGroupMemberExists(%s, %s): not found", safeShort8Test(groupID), safeShortTest(peerID))
	}
}

func (e *TestEnv) AssertGroupMemberNotExists(groupID, peerID string) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM group_members WHERE group_id = ? AND peer_id = ?", groupID, peerID).Scan(&count)
	if count != 0 {
		e.T.Errorf("AssertGroupMemberNotExists(%s, %s): still exists", safeShort8Test(groupID), safeShortTest(peerID))
	}
}

func (e *TestEnv) AssertGroupMessageExists(msgID string) {
	e.T.Helper()
	var count int
	e.Store.DB().QueryRow("SELECT COUNT(*) FROM group_messages WHERE id = ?", msgID).Scan(&count)
	if count == 0 {
		e.T.Errorf("AssertGroupMessageExists(%s): not found", msgID)
	}
}

// --- P2P assertions ---

func (e *TestEnv) AssertP2PSent(peerID string, msgType string) {
	e.T.Helper()
	for _, s := range e.GetSent() {
		if s.To == peerID && s.Type == msgType {
			return
		}
	}
	e.T.Errorf("AssertP2PSent(%s, %s): not found in %d sent messages", safeShortTest(peerID), msgType, len(e.GetSent()))
}

func (e *TestEnv) AssertP2PNotSent(peerID string) {
	e.T.Helper()
	for _, s := range e.GetSent() {
		if s.To == peerID {
			e.T.Errorf("AssertP2PNotSent(%s): found sent message type=%s", safeShortTest(peerID), s.Type)
			return
		}
	}
}

func (e *TestEnv) AssertP2PSentCount(peerID string, expected int) {
	e.T.Helper()
	count := 0
	for _, s := range e.GetSent() {
		if s.To == peerID {
			count++
		}
	}
	if count != expected {
		e.T.Errorf("AssertP2PSentCount(%s): expected %d, got %d", safeShortTest(peerID), expected, count)
	}
}

// --- Event assertions ---

func (e *TestEnv) AssertEventEmitted(eventName string) {
	e.T.Helper()
	for _, ev := range e.GetEvents() {
		if ev.Name == eventName {
			return
		}
	}
	e.T.Errorf("AssertEventEmitted(%s): not found in %d events", eventName, len(e.GetEvents()))
}

func (e *TestEnv) AssertEventNotEmitted(eventName string) {
	e.T.Helper()
	for _, ev := range e.GetEvents() {
		if ev.Name == eventName {
			e.T.Errorf("AssertEventNotEmitted(%s): event was emitted", eventName)
			return
		}
	}
}

// --- Message order ---

func (e *TestEnv) AssertMessageOrder(msgIDs ...string) {
	e.T.Helper()
	var prevTime int64
	for i, id := range msgIDs {
		var createdAt int64
		err := e.Store.DB().QueryRow("SELECT created_at FROM messages WHERE id = ?", id).Scan(&createdAt)
		if err != nil {
			e.T.Fatalf("AssertMessageOrder: msg %s not found", id)
		}
		if i > 0 && createdAt < prevTime {
			e.T.Errorf("AssertMessageOrder: msg[%d]=%s (t=%d) before msg[%d]=%s (t=%d)",
				i, id, createdAt, i-1, msgIDs[i-1], prevTime)
		}
		prevTime = createdAt
	}
}

// --- Pending count ---

func (e *TestEnv) AssertPendingCount(t *testing.T, peerID string, expected int) {
	t.Helper()
	var count int
	e.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE peer_id = ? AND sender_id = ? AND delivered = 0 AND deleted = 0",
		peerID, e.MyID).Scan(&count)
	if count != expected {
		t.Errorf("AssertPendingCount(%s): expected %d, got %d", safeShortTest(peerID), expected, count)
	}
}
