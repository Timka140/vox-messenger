package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// HandleVoiceP2P — диспетчер входящих voice control-сообщений по DataChannel.
//
// Обрабатывает 4 типа:
//   - voice_join   — пир входит в голосовой канал (с ack-handshake и gossip)
//   - voice_leave  — пир выходит
//   - voice_status — обновление mute/deafen статуса
//   - voice_members — gossip-список участников канала (для not-friends discovery)
//
// Все обработчики держат s.mu целиком. Внутри них выполняются исходящие
// SendP2PData, что в теории опасно для дедлоков (sync send из-под voice lock),
// но peermgr.SendData не берёт voice mu, поэтому безопасно.
func (s *VoiceService) HandleVoiceP2P(fromPubKey string, msg *P2PMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Не обрабатываем voice сигналы от самого себя
	myID, _ := s.sess.PeerID()
	if fromPubKey == myID {
		return
	}

	switch msg.Type {
	case "voice_join":
		s.handleVoiceJoinLocked(fromPubKey, myID, msg)
	case "voice_leave":
		s.handleVoiceLeaveLocked(fromPubKey, msg)
	case "voice_status":
		s.handleVoiceStatusLocked(fromPubKey, msg)
	case "voice_members":
		s.handleVoiceMembersLocked(fromPubKey, myID, msg)
	}
}

// --- voice_join ---
//
// Сложнейший из четырёх. Делает:
//   1. Запись пира в глобальный channelPeers[groupID]
//   2. Если это активный канал у нас — добавляем в participants
//   3. На первый JOIN (не ack) отправляем ack + gossip voice_members
//      новому пиру, а также fan-out нового пира остальным участникам
//   4. Отправляем свой mute/deafen статус если muted/deafened
//   5. Эмитим nexus:voice-join событие во фронтенд (с никнеймом)
func (s *VoiceService) handleVoiceJoinLocked(fromPubKey, myID string, msg *P2PMessage) {
	s.log.Info("🎧 VOICE_JOIN P2P",
		slog.String("from", safeShort(fromPubKey)),
		slog.String("groupID", msg.GroupID),
		slog.String("activeGroupID", s.activeGroupID),
		slog.Bool("match", msg.GroupID == s.activeGroupID))

	// Глобальный трекинг — всегда запоминаем кто в каком канале
	if s.channelPeers[msg.GroupID] == nil {
		s.channelPeers[msg.GroupID] = make(map[string]bool)
	}
	seenInChannel := s.channelPeers[msg.GroupID][fromPubKey]
	s.channelPeers[msg.GroupID][fromPubKey] = true

	wasKnown := joinWasKnown(seenInChannel, s.participants[fromPubKey], msg.Content)
	if msg.GroupID == s.activeGroupID {
		alreadyKnown := s.participants[fromPubKey]
		s.participants[fromPubKey] = true
		s.log.Info("🎧 PEER VOICE JOIN",
			slog.String("peer", safeShort(fromPubKey)),
			slog.String("group", safeShort8(msg.GroupID)),
			slog.Bool("alreadyKnown", alreadyKnown),
			slog.Bool("isAck", msg.Content == "ack"),
			slog.Int("participants_now", len(s.participants)))

		if !alreadyKnown && msg.Content != "ack" {
			s.sendVoiceJoinAckLocked(fromPubKey, myID, msg.GroupID)
		}

		// Отправляем свой mute/deafen статус новому участнику
		if s.muted || s.deafened {
			statusMsg := P2PMessage{
				Type:    "voice_status",
				GroupID: msg.GroupID,
				Content: fmt.Sprintf(`{"muted":%t,"deafened":%t}`, s.muted, s.deafened),
			}
			statusData, _ := json.Marshal(statusMsg)
			s.network.SendP2PData(fromPubKey, statusData)
		}
	}

	// Nickname: приоритет — из самого P2P сообщения (для не-друзей),
	// иначе из contacts/кеша через peerNickname.
	nick := msg.Nickname
	if nick != "" {
		s.network.CachePeerNickname(fromPubKey, nick)
	} else {
		nick = s.network.PeerNickname(fromPubKey)
	}
	s.network.emitEvent("vox:voice-join", map[string]any{
		"groupId":  msg.GroupID,
		"peerId":   fromPubKey,
		"nickname": nick,
		"wasKnown": wasKnown,
	})

	// SFU: обновить hash map + проверить нужно ли переключиться
	s.updateSFUPeerHash(fromPubKey)
	s.checkSFUSwitchLocked()
}

// sendVoiceJoinAckLocked — отправка ack и gossip-распространение списка
// участников при первом получении JOIN от нового пира. Вызывается из
// handleVoiceJoinLocked в ветке !alreadyKnown && !isAck.
func (s *VoiceService) sendVoiceJoinAckLocked(fromPubKey, myID, groupID string) {
	ack := P2PMessage{Type: "voice_join", GroupID: groupID, Content: "ack"}
	data, _ := json.Marshal(ack)
	if err := s.network.SendP2PData(fromPubKey, data); err != nil {
		s.log.Warn("ack send error", slog.String("error", err.Error()))
	} else {
		s.log.Info("🎧 ACK SENT to", slog.String("peer", safeShort(fromPubKey)))
	}

	// Gossip: отправляем полный список участников канала новому пиру
	var memberList []string
	memberList = append(memberList, myID)
	for pid := range s.channelPeers[groupID] {
		if pid != fromPubKey && pid != myID {
			memberList = append(memberList, pid)
		}
	}
	if len(memberList) > 0 {
		membersJSON, _ := json.Marshal(memberList)
		membersMsg := P2PMessage{
			Type:    "voice_members",
			GroupID: groupID,
			Content: string(membersJSON),
		}
		mdata, _ := json.Marshal(membersMsg)
		s.network.SendP2PData(fromPubKey, mdata)
		s.log.Info("🎧 voice_members SENT",
			slog.String("to", safeShort(fromPubKey)),
			slog.Int("count", len(memberList)))
	}

	// Fan-out: уведомляем остальных участников канала о новом пире.
	// Гейтинг от штормов: внутри !alreadyKnown && !isAck — только реально новые пиры.
	// voice_members receiver не генерирует исходящий gossip — нет амплификации.
	var fullList []string
	fullList = append(fullList, myID)
	for pid := range s.channelPeers[groupID] {
		if pid != myID {
			fullList = append(fullList, pid)
		}
	}
	if len(fullList) > 1 {
		fullJSON, _ := json.Marshal(fullList)
		fanoutMsg := P2PMessage{
			Type:    "voice_members",
			GroupID: groupID,
			Content: string(fullJSON),
		}
		fdata, _ := json.Marshal(fanoutMsg)
		for pid := range s.channelPeers[groupID] {
			if pid == myID || pid == fromPubKey {
				continue // себе и новичку уже отправили
			}
			if err := s.network.SendP2PData(pid, fdata); err != nil {
				continue
			}
			s.log.Info("🎧 voice_members FAN-OUT",
				slog.String("to", safeShort(pid)),
				slog.String("newPeer", safeShort(fromPubKey)),
				slog.Int("count", len(fullList)))
		}
	}
}

// --- voice_leave ---

func (s *VoiceService) handleVoiceLeaveLocked(fromPubKey string, msg *P2PMessage) {
	delete(s.participants, fromPubKey)
	delete(s.peerSpeaking, fromPubKey)
	delete(s.speakDecay, fromPubKey)
	if peers, ok := s.channelPeers[msg.GroupID]; ok {
		delete(peers, fromPubKey)
		if len(peers) == 0 {
			delete(s.channelPeers, msg.GroupID)
		}
	}
	leaveNick := s.network.PeerNickname(fromPubKey)
	s.log.Info("🎧 PEER VOICE LEAVE",
		slog.String("peer", safeShort(fromPubKey)),
		slog.String("group", safeShort8(msg.GroupID)))
	s.network.emitEvent("vox:voice-leave", map[string]string{
		"groupId":  msg.GroupID,
		"peerId":   fromPubKey,
		"nickname": leaveNick,
	})

	// SFU: удалить из hash map + проверить нужно ли переключиться обратно на mesh
	s.removeSFUPeerHash(fromPubKey)
	s.checkSFUSwitchLocked()
}

// joinWasKnown — voice_join от пира, уже известного в этом канале (повтор,
// re-announce, ack), а не новый вход. Без seenInChannel повторы join в чужой
// канал (participants — только мой) каждый раз выглядели новым входом, и фронт
// показывал «зашёл в канал» раз в 3 с. voice_leave убирает пира из channelPeers,
// так что вход после выхода — снова новый.
func joinWasKnown(seenInChannel, participant bool, content string) bool {
	return seenInChannel || participant || content == "ack"
}

// --- voice_status ---

func (s *VoiceService) handleVoiceStatusLocked(fromPubKey string, msg *P2PMessage) {
	s.log.Debug("🎧 VOICE_STATUS",
		slog.String("from", safeShort(fromPubKey)))
	s.network.emitEvent("vox:voice-status", map[string]any{
		"groupId": msg.GroupID,
		"peerId":  fromPubKey,
		"status":  msg.Content,
	})
}

// --- voice_members ---
//
// Gossip: обнаружение участников канала, к которым у нас ещё нет P2P.
// Получив список — добавляем каждого в channelPeers и инициируем P2P
// (если канал у нас активный, а пира нет в participants).
func (s *VoiceService) handleVoiceMembersLocked(fromPubKey, myID string, msg *P2PMessage) {
	var memberIDs []string
	if err := json.Unmarshal([]byte(msg.Content), &memberIDs); err != nil {
		s.log.Warn("invalid voice_members payload", slog.String("error", err.Error()))
		return
	}
	s.log.Info("🎧 voice_members RECEIVED",
		slog.String("from", safeShort(fromPubKey)),
		slog.String("group", safeShort8(msg.GroupID)),
		slog.Int("count", len(memberIDs)))
	for _, pid := range memberIDs {
		if pid == myID {
			continue
		}
		if s.channelPeers[msg.GroupID] == nil {
			s.channelPeers[msg.GroupID] = make(map[string]bool)
		}
		s.channelPeers[msg.GroupID][pid] = true
		if msg.GroupID == s.activeGroupID {
			if !s.participants[pid] {
				s.participants[pid] = true
				s.log.Info("🎧 discovered via gossip",
					slog.String("peer", safeShort(pid)))
			}
			go s.network.ConnectToGroupPeer(pid)
		}
	}
}
