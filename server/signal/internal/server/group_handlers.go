package server

import (
	"encoding/json"
	"log/slog"
	"time"

	"messenger_signal/internal/groupstore"
	"messenger_signal/internal/hub"
	"messenger_signal/pkg/protocol"
)

// Максимальное расхождение клиентского и серверного времени при котором доверяем клиенту.
// Выше порога — подменяем client_ts_ms на server_ts_ms.
const clientClockDriftLimit = 5 * time.Minute

// maxCatchUpMessages — предел сообщений, которые узел досылает соседу за
// один обмен состоянием (остальное клиенты догонят через group_sync).
const maxCatchUpMessages = 20000

// groupsEnabled возвращает true если серверный group relay настроен.
func (s *Server) groupsEnabled() bool {
	return s.groups != nil
}

// handleGroupRegister — клиент регистрирует группу на сервере.
// Используется при переходе группы через порог P2PThreshold или при создании группы >порога.
// Регистрация идемпотентна — повторный вызов задаёт полный состав.
func (s *Server) handleGroupRegister(peer *hub.Peer, msg *protocol.Message) {
	if !s.groupsEnabled() {
		s.sendError(peer, protocol.ErrInvalidMsg, "group relay disabled")
		return
	}
	var p protocol.GroupRegisterPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "invalid group_register payload")
		return
	}
	if p.GroupID == "" || len(p.Members) == 0 {
		s.sendError(peer, protocol.ErrInvalidMsg, "group_id and members required")
		return
	}

	// Регистрирующий должен быть участником группы (обычно — админ).
	senderIsMember := false
	for _, m := range p.Members {
		if m == peer.PublicKey {
			senderIsMember = true
			break
		}
	}
	if !senderIsMember {
		s.sendError(peer, protocol.ErrNotGroupMember, "sender not in members list")
		return
	}

	// Полный состав задаёт только админ уже известной группы. Иначе рядовой
	// участник со старым локальным составом молча выкинул бы людей и снял
	// админов, и это разъехалось бы по кластеру: RegisterGroup авторитетен, а
	// sendViaRelay зовёт регистрацию при каждой отправке в режиме не-server.
	known, err := s.groups.GetGroup(p.GroupID)
	if err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "group lookup failed")
		return
	}
	authoritative := known == nil
	if !authoritative {
		member, err := s.groups.IsMember(p.GroupID, peer.PublicKey)
		if err != nil {
			s.sendError(peer, protocol.ErrInvalidMsg, "group lookup failed")
			return
		}
		if !member {
			s.sendError(peer, protocol.ErrNotGroupMember, "sender not in group")
			return
		}
		admin, err := s.groups.IsAdmin(p.GroupID, peer.PublicKey)
		if err != nil {
			s.sendError(peer, protocol.ErrInvalidMsg, "group lookup failed")
			return
		}
		authoritative = admin
	}

	var changed []groupstore.MemberRow
	if authoritative {
		changed, err = s.groups.RegisterGroup(p.GroupID, p.Members, p.Admins)
		if err != nil {
			s.log.Warn("group register failed",
				slog.String("group_id", p.GroupID),
				slog.String("error", err.Error()))
			s.sendError(peer, protocol.ErrInvalidMsg, "register failed")
			return
		}
	}

	g, err := s.groups.GetGroup(p.GroupID)
	if err != nil || g == nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "group lookup failed")
		return
	}
	s.replicateMembers(g, changed)

	currentSeq, _ := s.groups.CurrentSeq(p.GroupID)

	okPayload, _ := json.Marshal(protocol.GroupRegisterOKPayload{
		GroupID:     p.GroupID,
		CurrentSeq:  currentSeq,
		MemberCount: g.MemberCount,
	})
	okMsg, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeGroupRegisterOK,
		Payload: okPayload,
	})
	_ = peer.Send(okMsg)

	// Уведомляем всех онлайн-участников о смене delivery_mode на server.
	s.announceServerMode(p.GroupID)

	s.log.Info("📦 group registered",
		slog.String("group_id", p.GroupID),
		slog.Int("members", len(p.Members)),
		slog.Int("admins", len(p.Admins)),
		slog.Int("changed", len(changed)),
		slog.Bool("authoritative", authoritative))
}

// announceServerMode рассылает group_delivery_mode участникам группы,
// подключённым к этому узлу. Состав берём из стора, а не из присланного
// клиентом списка. Каждый узел объявляет режим сам — так же, как сам делает
// fan-out реплик сообщений.
func (s *Server) announceServerMode(groupID string) {
	keys, err := s.groups.ListMembers(groupID)
	if err != nil {
		s.log.Warn("delivery mode announce failed",
			slog.String("group_id", groupID),
			slog.String("error", err.Error()))
		return
	}
	if len(keys) == 0 {
		return
	}
	s.hub.BroadcastToKeys(keys, deliveryModeMsg(groupID))
}

// pushServerGroupModes досылает подключившемуся клиенту delivery_mode по всем
// его группам. Без этого участник, который был офлайн в момент регистрации
// группы (или сидел на другом узле), остаётся в p2p и не догоняет историю:
// SyncAllServerGroups выбирает только группы с delivery_mode='server'.
func (s *Server) pushServerGroupModes(peer *hub.Peer) {
	if !s.groupsEnabled() {
		return
	}
	ids, err := s.groups.GroupsForMember(peer.PublicKey)
	if err != nil {
		s.log.Warn("group modes push failed",
			slog.String("peer", peer.NickTag()),
			slog.String("error", err.Error()))
		return
	}
	for _, id := range ids {
		if err := peer.Send(deliveryModeMsg(id)); err != nil {
			return
		}
	}
}

// deliveryModeMsg — готовое group_delivery_mode{server} для группы.
func deliveryModeMsg(groupID string) []byte {
	payload, _ := json.Marshal(protocol.GroupDeliveryModePayload{
		GroupID: groupID,
		Mode:    protocol.DeliveryModeServer,
	})
	data, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeGroupDeliveryMode,
		Payload: payload,
	})
	return data
}

// handleGroupMemberChange — админ добавляет или удаляет участника.
// add=true → add_member, add=false → remove_member.
func (s *Server) handleGroupMemberChange(peer *hub.Peer, msg *protocol.Message, add bool) {
	if !s.groupsEnabled() {
		s.sendError(peer, protocol.ErrInvalidMsg, "group relay disabled")
		return
	}
	var p protocol.GroupMemberChangePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "invalid member_change payload")
		return
	}
	if p.GroupID == "" || p.PublicKey == "" {
		s.sendError(peer, protocol.ErrInvalidMsg, "group_id and public_key required")
		return
	}

	isAdmin, err := s.groups.IsAdmin(p.GroupID, peer.PublicKey)
	if err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "admin check failed")
		return
	}
	if !isAdmin {
		s.sendError(peer, protocol.ErrNotGroupAdmin, "only admin can change membership")
		return
	}

	var row *groupstore.MemberRow
	if add {
		row, err = s.groups.AddMember(p.GroupID, p.PublicKey, p.IsAdmin)
	} else {
		row, err = s.groups.RemoveMember(p.GroupID, p.PublicKey)
	}
	if err != nil {
		s.log.Warn("group member change failed",
			slog.String("group_id", p.GroupID),
			slog.Bool("add", add),
			slog.String("error", err.Error()))
		s.sendError(peer, protocol.ErrInvalidMsg, "member change failed")
		return
	}
	if row != nil {
		if g, _ := s.groups.GetGroup(p.GroupID); g != nil {
			s.replicateMembers(g, []groupstore.MemberRow{*row})
		}
	}
}

// handleGroupMessageSend — клиент отправляет сообщение в группу.
// Путь: размер → rate limit → membership → e2e policy → append → fan-out →
// репликация на другие узлы.
func (s *Server) handleGroupMessageSend(peer *hub.Peer, msg *protocol.Message) {
	if !s.groupsEnabled() {
		s.sendError(peer, protocol.ErrInvalidMsg, "group relay disabled")
		return
	}

	// Размер кадра заранее не проверяем: он меряет JSON с экранированием
	// (< уходит шестью байтами) и зря резал бы допустимые сообщения, а цену
	// разбора и так ограничивает лимит чтения websocket.
	var p protocol.GroupMessageSendPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "invalid group_message_send payload")
		return
	}
	if p.GroupID == "" || p.ClientMsgID == "" {
		s.sendError(peer, protocol.ErrInvalidMsg, "group_id and client_msg_id required")
		return
	}
	// Дальше каждый отказ несёт id сообщения: клиент снимет его с повтора.
	reject := func(code, message string) {
		s.sendGroupSendError(peer, code, message, p.GroupID, p.ClientMsgID)
	}
	if len(p.Payload) > s.groupsCfg.MaxMsgBytes {
		reject(protocol.ErrMessageTooLarge, "message body exceeds limit")
		return
	}

	// Rate limit ДО любых БД-обращений.
	if !s.rate.Allow(p.GroupID, peer.PublicKey) {
		reject(protocol.ErrRateLimited, "too many messages, slow down")
		return
	}

	// Membership — может ли пир писать в эту группу.
	ok, err := s.groups.IsMember(p.GroupID, peer.PublicKey)
	if err != nil {
		reject(protocol.ErrInvalidMsg, "membership check failed")
		return
	}
	if !ok {
		// Повтор уже принятого сообщения после удаления автора из группы:
		// остальные его получили, поэтому отвечаем сохранённой копией, а не
		// отказом, иначе у автора оно пометилось бы неотправленным.
		if stored, found, _ := s.groups.FindMessage(p.GroupID, peer.PublicKey, p.ClientMsgID); found {
			payload, _ := json.Marshal(groupMsgPayload(stored))
			data, _ := json.Marshal(protocol.Message{Type: protocol.TypeGroupMessageNew, Payload: payload})
			_ = peer.Send(data)
			return
		}
		reject(protocol.ErrNotGroupMember, "not a member of this group")
		return
	}

	g, err := s.groups.GetGroup(p.GroupID)
	if err != nil {
		reject(protocol.ErrInvalidMsg, "group lookup failed")
		return
	}
	if g == nil {
		reject(protocol.ErrGroupNotFound, "group not found")
		return
	}

	// E2E policy: если группа >= E2EThreshold, шифрованные сообщения не принимаются.
	// Причина: сервер должен мочь сжимать для TTL-хранения на диске.
	if g.MemberCount >= s.groupsCfg.E2EThreshold && p.Encrypted {
		reject(protocol.ErrInvalidMsg, "e2e not allowed for groups this size")
		return
	}

	// Hybrid clock: берём client_ts_ms если в пределах 5 минут от серверного,
	// иначе используем server_ts_ms (клиент с кривыми часами). Стор под
	// своим замком подтягивает server_ts_ms до монотонного по журналу.
	serverTs := time.Now().UTC().UnixMilli()
	clientTs := p.ClientTsMs
	if clientTs == 0 || absInt64(clientTs-serverTs) > int64(clientClockDriftLimit/time.Millisecond) {
		clientTs = serverTs
	}

	// Запись, рассылка своим и реплика соседям — под замком группы. Иначе
	// два параллельных отправителя кладут A:11 на линк раньше A:10: голова
	// журнала у соседа уходит выше неприехавшего seq, а догон сравнивает
	// только головы, и пропуск остаётся навсегда. Порядок захвата
	// sendLock → seqMu (в сторе) → hub.
	lock := s.groupSendLock(p.GroupID)
	lock.Lock()
	stored, isNew, err := s.groups.AppendMessage(groupstore.Message{
		GroupID:     p.GroupID,
		AuthorKey:   peer.PublicKey,
		ClientMsgID: p.ClientMsgID,
		Payload:     p.Payload,
		Encrypted:   p.Encrypted,
		SizeBytes:   len(p.Payload),
		ServerTsMs:  serverTs,
		ClientTsMs:  clientTs,
	})
	if err != nil {
		lock.Unlock()
		s.log.Warn("append group message failed",
			slog.String("group_id", p.GroupID),
			slog.String("error", err.Error()))
		reject(protocol.ErrInvalidMsg, "persist failed")
		return
	}

	// Fan-out всем участникам группы, включая отправителя: его копия — это
	// подтверждение (seq и origin_node). Повтор отправки получает ту же копию.
	members, delivered := s.fanOutGroupMessage(stored)
	if isNew && s.cluster != nil {
		s.cluster.BroadcastGroupMsg(toNodeGrpMsg(stored))
	}
	lock.Unlock()

	s.log.Info("📢 group fan-out",
		slog.String("group_id", p.GroupID),
		slog.Int64("seq", stored.Seq),
		slog.Bool("new", isNew),
		slog.String("from", peer.NickTag()),
		slog.Int("members", members),
		slog.Int("online", delivered),
		slog.Int("size", stored.SizeBytes))
}

// handleGroupSync — клиент просит пропущенные сообщения (при реконнекте, догон истории).
func (s *Server) handleGroupSync(peer *hub.Peer, msg *protocol.Message) {
	if !s.groupsEnabled() {
		s.sendError(peer, protocol.ErrInvalidMsg, "group relay disabled")
		return
	}
	var p protocol.GroupSyncPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "invalid group_sync payload")
		return
	}
	if p.GroupID == "" {
		s.sendError(peer, protocol.ErrInvalidMsg, "group_id required")
		return
	}

	ok, err := s.groups.IsMember(p.GroupID, peer.PublicKey)
	if err != nil || !ok {
		s.sendError(peer, protocol.ErrNotGroupMember, "not a member of this group")
		return
	}

	limit := p.Limit
	if limit <= 0 || limit > s.groupsCfg.MaxSyncLimit {
		limit = s.groupsCfg.MaxSyncLimit
	}

	// Курсоры обязательны. Клиент до Э3 слал since_seq — курсор одного
	// журнала, и при нескольких узлах крутил синхронизацию бесконечно;
	// совместимость с ним не нужна (контракт §0). Ошибка, а не пустой
	// ответ: пустой врал бы, что синхронизировать нечего.
	if p.Cursors == nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "cursors required")
		return
	}

	stored, hasMore, err := s.groups.MessagesSinceCursors(p.GroupID, p.Cursors, limit)
	if err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "sync failed")
		return
	}

	result := make([]protocol.GroupMessageNewPayload, 0, len(stored))
	for _, m := range stored {
		result = append(result, groupMsgPayload(m))
	}

	respPayload, _ := json.Marshal(protocol.GroupSyncResultPayload{
		GroupID:  p.GroupID,
		Messages: result,
		HasMore:  hasMore,
	})
	resp, _ := json.Marshal(protocol.Message{
		Type:    protocol.TypeGroupSyncResult,
		Payload: respPayload,
	})
	_ = peer.Send(resp)

	s.log.Debug("group sync",
		slog.String("group_id", p.GroupID),
		slog.String("peer", peer.NickTag()),
		slog.Int("returned", len(result)),
		slog.Bool("has_more", hasMore))
}

// fanOutGroupMessage — group_message_new подключённым к этому узлу
// участникам группы. Возвращает число участников и сколько получили.
func (s *Server) fanOutGroupMessage(m groupstore.Message) (members, delivered int) {
	keys, err := s.groups.ListMembers(m.GroupID)
	if err != nil {
		s.log.Warn("list members failed",
			slog.String("group_id", m.GroupID),
			slog.String("error", err.Error()))
		return 0, 0
	}
	payload, _ := json.Marshal(groupMsgPayload(m))
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeGroupMessageNew, Payload: payload})
	return len(keys), s.hub.BroadcastToKeys(keys, data)
}

// replicateMembers — изменения состава группы на другие узлы кластера.
func (s *Server) replicateMembers(g *groupstore.Group, rows []groupstore.MemberRow) {
	if s.cluster == nil || g == nil || len(rows) == 0 {
		return
	}
	s.cluster.BroadcastGroupMembers(protocol.NodeGrpMembersPayload{
		GroupID:   g.ID,
		CreatedMs: g.CreatedAt.UnixMilli(),
		Members:   toNodeMembers(rows),
	})
}

// --- репликация журналов (cluster.GroupHandler) ---

// ApplyGroupMsgs — сообщения журналов других узлов: сохранить одной
// транзакцией и донести до своих подключённых участников. Возвращает число
// новых.
//
// live — живая реплика (node_grp_msg): рассылаем поштучно, как своё. Догон
// (node_grp_msgs) может нести тысячи сообщений: поштучная рассылка переполнила
// бы очередь пира (256), и хаб порвал бы клиенту соединение. Поэтому при
// догоне участникам уходит одна отложенная подсказка на группу, а сообщения
// клиент забирает сам постраничным group_sync. Молча не рассылать нельзя:
// онлайн-клиент двигает курсор только непрерывно и без подсказки ждал бы
// переподключения.
func (s *Server) ApplyGroupMsgs(msgs []protocol.NodeGrpMsg, live bool) int {
	if s.groups == nil || len(msgs) == 0 {
		return 0
	}
	stored := make([]groupstore.Message, len(msgs))
	for i, nm := range msgs {
		stored[i] = fromNodeGrpMsg(nm)
	}
	isNew, err := s.groups.InsertReplicas(stored)
	if err != nil {
		s.log.Warn("group replica insert failed",
			slog.Int("batch", len(msgs)),
			slog.String("error", err.Error()))
	}
	n := 0
	hinted := make(map[string]bool)
	for i, m := range stored {
		if !isNew[i] {
			continue
		}
		n++
		if live {
			s.fanOutGroupMessage(m)
		} else if !hinted[m.GroupID] {
			hinted[m.GroupID] = true
			s.scheduleSyncHint(m.GroupID)
		}
	}
	return n
}

// syncHintDelay — сколько ждать тишины в догоне группы перед подсказкой.
// Догон идёт пачками; подсказка на каждую запустила бы у клиента столько же
// параллельных цепочек синхронизации. Переменная — чтобы тесты не ждали.
var syncHintDelay = 500 * time.Millisecond

// scheduleSyncHint — отложенная подсказка group_sync участникам группы;
// каждая новая пачка догона отодвигает её.
func (s *Server) scheduleSyncHint(groupID string) {
	s.hintMu.Lock()
	defer s.hintMu.Unlock()
	if t, ok := s.hintTimers[groupID]; ok {
		t.Reset(syncHintDelay)
		return
	}
	if s.hintTimers == nil {
		s.hintTimers = make(map[string]*time.Timer)
	}
	s.hintTimers[groupID] = time.AfterFunc(syncHintDelay, func() {
		s.hintMu.Lock()
		delete(s.hintTimers, groupID)
		s.hintMu.Unlock()
		s.sendSyncHint(groupID)
	})
}

func (s *Server) sendSyncHint(groupID string) {
	keys, err := s.groups.ListMembers(groupID)
	if err != nil {
		s.log.Warn("sync hint: list members failed",
			slog.String("group_id", groupID),
			slog.String("error", err.Error()))
		return
	}
	payload, _ := json.Marshal(protocol.GroupSyncHintPayload{GroupID: groupID})
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeGroupSyncHint, Payload: payload})
	s.hub.BroadcastToKeys(keys, data)
}

// ApplyGroupMembers — строки состава с другого узла (LWW).
func (s *Server) ApplyGroupMembers(p protocol.NodeGrpMembersPayload) {
	// Периодическая сверка голов идёт без состава — применять нечего.
	if s.groups == nil || len(p.Members) == 0 {
		return
	}
	n, err := s.groups.ApplyMembers(p.GroupID, p.CreatedMs, fromNodeMembers(p.Members))
	if err != nil {
		s.log.Warn("group members apply failed",
			slog.String("group_id", p.GroupID),
			slog.String("error", err.Error()))
		return
	}
	// Смену режима на server объявляет каждый узел своим клиентам: у соседа
	// в hub'е только его собственные сокеты. Условие n > 0 не даёт объявлять
	// режим по всем группам при каждом обмене состоянием на подъёме линка.
	if n > 0 {
		s.announceServerMode(p.GroupID)
	}
}

// GroupState — все группы узла с головами журналов; состав — только при
// withMembers (подъём линка).
func (s *Server) GroupState(withMembers bool) protocol.NodeGrpStatePayload {
	st := protocol.NodeGrpStatePayload{Groups: []protocol.NodeGrpGroupState{}}
	if s.groups == nil {
		return st
	}
	groups, err := s.groups.AllGroups()
	if err != nil {
		s.log.Warn("group state: groups", slog.String("error", err.Error()))
		return st
	}
	heads, err := s.groups.Heads()
	if err != nil {
		s.log.Warn("group state: heads", slog.String("error", err.Error()))
		return st
	}
	for _, g := range groups {
		h := heads[g.ID]
		if h == nil {
			h = map[string]int64{}
		}
		gs := protocol.NodeGrpGroupState{
			GroupID:   g.ID,
			CreatedMs: g.CreatedMs,
			Heads:     h,
		}
		if withMembers {
			rows, err := s.groups.MembersFull(g.ID)
			if err != nil {
				continue
			}
			gs.Members = toNodeMembers(rows)
		}
		st.Groups = append(st.Groups, gs)
	}
	return st
}

// GroupMissingFor — сообщения, которых нет у соседа по его головам журналов
// (из любых журналов, так сосед догоняет и третьи узлы).
func (s *Server) GroupMissingFor(peer protocol.NodeGrpStatePayload) []protocol.NodeGrpMsg {
	if s.groups == nil {
		return nil
	}
	peerHeads := make(map[string]map[string]int64, len(peer.Groups))
	for _, g := range peer.Groups {
		peerHeads[g.GroupID] = g.Heads
	}
	mine, err := s.groups.Heads()
	if err != nil {
		s.log.Warn("group catch-up: heads", slog.String("error", err.Error()))
		return nil
	}
	var out []protocol.NodeGrpMsg
	for groupID, origins := range mine {
		for origin, myMax := range origins {
			theirs := peerHeads[groupID][origin]
			if myMax <= theirs {
				continue
			}
			left := maxCatchUpMessages - len(out)
			if left <= 0 {
				return out
			}
			msgs, err := s.groups.MessagesAfter(groupID, origin, theirs, left)
			if err != nil {
				continue
			}
			for _, m := range msgs {
				out = append(out, toNodeGrpMsg(m))
			}
		}
	}
	return out
}

// --- преобразования ---

func groupMsgPayload(m groupstore.Message) protocol.GroupMessageNewPayload {
	return protocol.GroupMessageNewPayload{
		GroupID:     m.GroupID,
		Seq:         m.Seq,
		AuthorKey:   m.AuthorKey,
		ClientMsgID: m.ClientMsgID,
		Payload:     m.Payload,
		Encrypted:   m.Encrypted,
		ServerTsMs:  m.ServerTsMs,
		ClientTsMs:  m.ClientTsMs,
		OriginNode:  m.OriginNode,
	}
}

func toNodeGrpMsg(m groupstore.Message) protocol.NodeGrpMsg {
	return protocol.NodeGrpMsg{
		GroupID:     m.GroupID,
		OriginNode:  m.OriginNode,
		Seq:         m.Seq,
		AuthorKey:   m.AuthorKey,
		ClientMsgID: m.ClientMsgID,
		Payload:     m.Payload,
		Encrypted:   m.Encrypted,
		ServerTsMs:  m.ServerTsMs,
		ClientTsMs:  m.ClientTsMs,
	}
}

func fromNodeGrpMsg(m protocol.NodeGrpMsg) groupstore.Message {
	return groupstore.Message{
		GroupID:     m.GroupID,
		OriginNode:  m.OriginNode,
		Seq:         m.Seq,
		AuthorKey:   m.AuthorKey,
		ClientMsgID: m.ClientMsgID,
		Payload:     m.Payload,
		Encrypted:   m.Encrypted,
		SizeBytes:   len(m.Payload),
		ServerTsMs:  m.ServerTsMs,
		ClientTsMs:  m.ClientTsMs,
	}
}

func toNodeMembers(rows []groupstore.MemberRow) []protocol.NodeGrpMember {
	out := make([]protocol.NodeGrpMember, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.NodeGrpMember{PubKey: r.PubKey, IsAdmin: r.IsAdmin, Removed: r.Removed, TSms: r.UpdatedMs})
	}
	return out
}

func fromNodeMembers(ms []protocol.NodeGrpMember) []groupstore.MemberRow {
	out := make([]groupstore.MemberRow, 0, len(ms))
	for _, m := range ms {
		out = append(out, groupstore.MemberRow{PubKey: m.PubKey, IsAdmin: m.IsAdmin, Removed: m.Removed, UpdatedMs: m.TSms})
	}
	return out
}

func shortID(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
