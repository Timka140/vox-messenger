package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// GetGroupMembers возвращает список участников группы
func (s *GroupsService) GetGroupMembers(groupID string) ([]GroupMember, error) {
	const op = "GroupsService.GetGroupMembers"

	rows, err := s.store.DB().Query(
		`SELECT gm.peer_id, gm.role, COALESCE(NULLIF(c.nickname, ''), NULLIF(pp.nickname, ''), '')
		 FROM group_members gm
		 LEFT JOIN contacts c ON c.peer_id = gm.peer_id
		 LEFT JOIN peer_profiles pp ON pp.peer_id = gm.peer_id
		 WHERE gm.group_id = ? AND gm.status = 'active'
		 ORDER BY gm.role, gm.joined_at`, groupID)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()

	myID, _ := s.sess.PeerID()
	myNickname := s.network.MyNickname()

	var members []GroupMember
	for rows.Next() {
		var m GroupMember
		if err := rows.Scan(&m.PeerID, &m.Role, &m.Nickname); err != nil {
			continue
		}
		if m.Nickname == "" {
			if m.PeerID == myID && myNickname != "" {
				m.Nickname = myNickname
			} else {
				m.Nickname = safeShort(m.PeerID)
			}
		}
		m.Online = m.PeerID == myID || s.network.IsP2PConnected(m.PeerID)
		members = append(members, m)
	}

	if members == nil {
		members = []GroupMember{}
	}
	return members, rows.Err()
}

// AddMemberToGroup добавляет участника в группу (только admin)
func (s *GroupsService) AddMemberToGroup(groupID, peerID string) error {
	return s.addMember(groupID, peerID, false)
}

// addMember — общее тело ручного добавления и вступления по ссылке. viaLink:
// строка вышедшего участника возвращается в active (INSERT OR IGNORE оставил
// бы её 'left', и сообщения не пошли бы). Ручное добавление снимает бан.
func (s *GroupsService) addMember(groupID, peerID string, viaLink bool) error {
	const op = "GroupsService.AddMemberToGroup"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Проверяем права admin
	var role string
	err = s.store.DB().QueryRow(
		"SELECT role FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&role)
	if err != nil || role != "admin" {
		return fmt.Errorf("%s: нет прав администратора", op)
	}
	if !viaLink {
		// Владелец добавил исключённого вручную — бан больше не действует.
		s.store.DB().Exec("DELETE FROM group_bans WHERE group_id = ? AND peer_id = ?", groupID, peerID)
	}

	// Проверяем лимит участников
	var memberCount int
	if err := s.store.DB().QueryRow("SELECT COUNT(*) FROM group_members WHERE group_id = ?", groupID).Scan(&memberCount); err != nil {
		return fmt.Errorf("%s: count members: %w", op, err)
	}
	if memberCount >= maxGroupMembers {
		return fmt.Errorf("%s: группа заполнена (макс %d участников)", op, maxGroupMembers)
	}

	now := time.Now().Unix()
	insert := `INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at, last_read_at) VALUES (?, ?, 'member', ?, ?)`
	if viaLink {
		insert = `INSERT INTO group_members (group_id, peer_id, role, joined_at, last_read_at, status) VALUES (?, ?, 'member', ?, ?, 'active')
			 ON CONFLICT(group_id, peer_id) DO UPDATE SET status = 'active'`
	}
	_, err = s.store.DB().Exec(insert, groupID, peerID, now, now)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Получаем данные группы для приглашения
	var groupName, creatorID, avatarHash string
	var groupType int
	if err := s.store.DB().QueryRow(
		"SELECT name, owner_id, type, avatar_hash FROM groups WHERE group_id = ?",
		groupID).Scan(&groupName, &creatorID, &groupType, &avatarHash); err != nil {
		return fmt.Errorf("%s: get group info: %w", op, err)
	}

	members := s.GetGroupMemberIDs(groupID)

	invite := GroupInvitePayload{
		GroupID:    groupID,
		Name:       groupName,
		CreatorID:  creatorID,
		Members:    members,
		Type:       groupType,
		AvatarHash: avatarHash,
	}
	inviteJSON, err := json.Marshal(invite)
	if err != nil {
		s.log.Error("не удалось сериализовать invite", slog.String("error", err.Error()))
		return nil // участник уже добавлен, invite отправится при reconnect
	}

	// Отправляем invite новому участнику (с очередью для офлайн)
	sent := false
	if s.network.IsP2PConnected(peerID) {
		p2pMsg := P2PMessage{
			Type:    "group_invite",
			ID:      groupID,
			GroupID: groupID,
			Content: string(inviteJSON),
			Time:    now,
		}
		data := s.marshalP2P(p2pMsg)
		if data != nil {
			if err := s.network.SendP2PData(peerID, data); err == nil {
				sent = true
			}
		}
	}
	if !sent {
		s.persistGroupAction(groupID+"-add-"+safeShort8(peerID), groupID, peerID, "group_invite", string(inviteJSON), now)
	}

	// Уведомляем существующих участников о новом члене (кроме нового и себя)
	s.broadcastGroupActionWithPayload("member_added", groupID, myID, members, peerID)

	s.network.emitEvent("vox:group-member-added", map[string]string{
		"groupId": groupID,
		"peerId":  peerID,
	})

	s.log.Info("участник добавлен в группу",
		slog.String("group", groupID),
		slog.String("peer", safeShort(peerID)))

	// Если группа на сервере — зеркалим добавление. Роль берём из локальной
	// записи: заявка по ссылке повторяется и для уже состоящего, а сервер
	// применяет присланный флаг — с жёстким false он снимал бы админа.
	var mirrorRole string
	_ = s.store.DB().QueryRow(
		"SELECT role FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, peerID).Scan(&mirrorRole)
	s.MirrorMemberChange(groupID, peerID, true, mirrorRole == "admin")

	return nil
}

// LeaveGroup — выход из группы
func (s *GroupsService) LeaveGroup(groupID string) error {
	const op = "GroupsService.LeaveGroup"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Уведомляем ВСЕХ участников (включая left — чтобы знали)
	members := s.getAllGroupMemberIDs(groupID)
	s.broadcastGroupAction("group_leave", groupID, myID, members)

	// Помечаем как покинувшего (не удаляем — можно вернуться)
	if _, err := s.store.DB().Exec(
		"UPDATE group_members SET status = 'left' WHERE group_id = ? AND peer_id = ?",
		groupID, myID); err != nil {
		return fmt.Errorf("%s: update status: %w", op, err)
	}

	s.network.emitEvent("vox:group-member-left", map[string]string{"groupId": groupID})
	s.log.Info("вышел из группы", slog.String("group", groupID))

	// Если группа на сервере — зеркалим удаление себя.
	s.MirrorMemberChange(groupID, myID, false, false)

	return nil
}

// RejoinGroup — вернуться в покинутую группу
func (s *GroupsService) RejoinGroup(groupID string) error {
	const op = "GroupsService.RejoinGroup"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Проверяем что статус = 'left'
	var status string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&status)
	if err != nil || status != "left" {
		return fmt.Errorf("%s: невозможно вернуться в группу", op)
	}

	now := time.Now().Unix()
	if _, err := s.store.DB().Exec(
		"UPDATE group_members SET status = 'active', last_read_at = ? WHERE group_id = ? AND peer_id = ?",
		now, groupID, myID); err != nil {
		return fmt.Errorf("%s: update status: %w", op, err)
	}

	// Уведомляем ВСЕХ участников что вернулся (включая left)
	members := s.getAllGroupMemberIDs(groupID)
	s.broadcastGroupAction("group_rejoin", groupID, myID, members)

	s.network.emitEvent("vox:group-member-joined", map[string]string{"groupId": groupID})
	s.log.Info("вернулся в группу", slog.String("group", groupID))
	return nil
}

// DeleteGroupAsAdmin — удаление группы создателем
func (s *GroupsService) DeleteGroupAsAdmin(groupID string) error {
	const op = "GroupsService.DeleteGroupAsAdmin"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	var ownerID string
	err = s.store.DB().QueryRow(
		"SELECT owner_id FROM groups WHERE group_id = ?",
		groupID).Scan(&ownerID)
	if err != nil || ownerID != myID {
		return fmt.Errorf("%s: только создатель может удалить группу", op)
	}

	// Уведомляем ВСЕХ участников (включая left) — с очередью для офлайн
	members := s.getAllGroupMemberIDs(groupID)
	s.broadcastGroupAction("group_delete", groupID, myID, members)

	// Удаляем все данные группы локально в транзакции
	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: begin tx: %w", op, err)
	}
	defer tx.Rollback() // no-op после успешного Commit
	deleteQueries := []string{
		"DELETE FROM group_message_delivery WHERE msg_id IN (SELECT id FROM group_messages WHERE group_id = ?)",
		"DELETE FROM group_messages WHERE group_id = ?",
		"DELETE FROM group_members WHERE group_id = ?",
		"DELETE FROM groups WHERE group_id = ?",
	}
	for _, q := range deleteQueries {
		if _, err := tx.Exec(q, groupID); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	s.network.emitEvent("vox:group-deleted", map[string]string{"groupId": groupID})
	s.log.Info("группа удалена", slog.String("group", groupID))
	return nil
}

// DeleteLeftGroup — удаление покинутой группы из локального списка
func (s *GroupsService) DeleteLeftGroup(groupID string) error {
	const op = "GroupsService.DeleteLeftGroup"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Проверяем что пользователь покинул группу
	var status string
	err = s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&status)
	if err != nil || status != "left" {
		return fmt.Errorf("%s: можно удалить только покинутую группу", op)
	}

	// Удаляем все данные группы локально
	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("%s: begin tx: %w", op, err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM group_message_delivery WHERE msg_id IN (SELECT id FROM group_messages WHERE group_id = ?)",
		"DELETE FROM group_messages WHERE group_id = ?",
		"DELETE FROM group_members WHERE group_id = ?",
		"DELETE FROM groups WHERE group_id = ?",
	} {
		if _, err := tx.Exec(q, groupID); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}

	s.network.emitEvent("vox:group-deleted", map[string]string{"groupId": groupID})
	s.log.Info("покинутая группа удалена из списка", slog.String("group", groupID))
	return nil
}

// KickGroupMember — исключение участника из группы (admin)
func (s *GroupsService) KickGroupMember(groupID, peerID string) error {
	const op = "GroupsService.KickGroupMember"

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}

	var role string
	s.store.DB().QueryRow(
		"SELECT role FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, myID).Scan(&role)
	if role != "admin" {
		return fmt.Errorf("%s: нет прав администратора", op)
	}

	// Удаляем участника
	s.store.DB().Exec(
		"DELETE FROM group_members WHERE group_id = ? AND peer_id = ?",
		groupID, peerID)

	// Уведомляем кикнутого через P2P (с очередью для офлайн)
	kickID := uuid.New().String()
	now := time.Now().Unix()

	// Бан: без него исключённый вернулся бы по той же ссылке-приглашению.
	s.store.DB().Exec("INSERT OR IGNORE INTO group_bans (group_id, peer_id, banned_at) VALUES (?, ?, ?)", groupID, peerID, now)
	sent := false
	if s.network.IsP2PConnected(peerID) {
		p2pMsg := P2PMessage{
			Type:    "group_kick",
			ID:      kickID,
			GroupID: groupID,
			Time:    now,
		}
		data := s.marshalP2P(p2pMsg)
		if data != nil {
			if err := s.network.SendP2PData(peerID, data); err == nil {
				sent = true
			}
		}
	}
	if !sent {
		s.persistGroupAction(kickID, groupID, peerID, "group_kick", "", now)
	}

	s.network.emitEvent("vox:group-kicked", map[string]string{
		"groupId": groupID,
		"peerId":  peerID,
	})
	s.log.Info("участник исключён",
		slog.String("group", groupID),
		slog.String("peer", safeShort(peerID)))

	// Если группа на сервере — зеркалим удаление.
	s.MirrorMemberChange(groupID, peerID, false, false)

	return nil
}

// getAllGroupMemberIDs возвращает ВСЕХ участников (включая left) — для broadcast leave/rejoin/delete
func (s *GroupsService) getAllGroupMemberIDs(groupID string) []string {
	rows, err := s.store.DB().Query(
		"SELECT peer_id FROM group_members WHERE group_id = ?", groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var peerID string
		if err := rows.Scan(&peerID); err != nil {
			continue
		}
		members = append(members, peerID)
	}
	return members
}

// GetGroupMemberIDs возвращает список активных peer_id участников группы
func (s *GroupsService) GetGroupMemberIDs(groupID string) []string {
	rows, err := s.store.DB().Query(
		"SELECT peer_id FROM group_members WHERE group_id = ? AND status = 'active'", groupID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var peerID string
		if err := rows.Scan(&peerID); err != nil {
			continue
		}
		members = append(members, peerID)
	}
	return members
}

// OnPeerOnlineForGroups — при peer_online проверяет общие группы и инициирует P2P.
// Вызывается из NetworkService через callback.
func (s *GroupsService) OnPeerOnlineForGroups(peerID string) {
	myID, err := s.sess.PeerID()
	if err != nil {
		return
	}
	if peerID == myID {
		return
	}
	var count int
	err = s.store.DB().QueryRow(`
		SELECT COUNT(*) FROM group_members gm1
		JOIN group_members gm2 ON gm1.group_id = gm2.group_id
		WHERE gm1.peer_id = ? AND gm2.peer_id = ?
		AND gm1.status = 'active' AND gm2.status = 'active'`,
		myID, peerID).Scan(&count)
	if err != nil || count == 0 {
		return
	}
	s.log.Info("🔗 peer online в общей группе, auto-connect",
		slog.String("peer", safeShort(peerID)),
		slog.Int("common_groups", count))
	s.network.ConnectToGroupPeer(peerID)
}
