package services

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"vox_app/internal/logsafe"
	"vox_app/internal/e2e"
	"vox_app/internal/network"
)

// defaultRelayThreshold — сколько участников должно быть в группе, чтобы
// клиент автоматически переключал её на server relay. 50 зеркалит серверный
// порог (signal config GROUP_P2P_THRESHOLD).
const defaultRelayThreshold = 50

// syncLimit — сколько сообщений просить у сервера за один group_sync.
// 100 — компромисс между трафиком и количеством round-trip'ов при догоне.
const syncLimit = 100

// ---------------------------------------------------------------------------
// Settings: force + threshold (per-user, хранятся в таблице settings)
// ---------------------------------------------------------------------------

const (
	settingKeyGroupForceRelay = "group_force_relay"
	settingKeyGroupRelayThr   = "group_relay_threshold"
)

// LoadRelaySettings — читает group_force_relay и group_relay_threshold из
// settings. Вызывается при старте приложения до того как пользователь мог
// нажать на что-либо в UI.
func (s *GroupsService) LoadRelaySettings() {
	var val string
	force := false
	if err := s.store.DB().QueryRow(
		"SELECT value FROM settings WHERE key = ?", settingKeyGroupForceRelay).Scan(&val); err == nil {
		force = val == "1"
	}
	threshold := defaultRelayThreshold
	if err := s.store.DB().QueryRow(
		"SELECT value FROM settings WHERE key = ?", settingKeyGroupRelayThr).Scan(&val); err == nil {
		if n, err := strconv.Atoi(val); err == nil && n >= 2 {
			threshold = n
		}
	}
	s.relayMu.Lock()
	s.forceRelay = force
	s.relayThreshold = threshold
	s.relayMu.Unlock()
	s.log.Info("group relay settings loaded",
		slog.Bool("force", force),
		slog.Int("threshold", threshold))
}

// SetGroupForceRelay включает/выключает принудительное использование server relay
// для всех групп независимо от их размера. Для тестирования Фазы 3.
func (s *GroupsService) SetGroupForceRelay(force bool) error {
	val := "0"
	if force {
		val = "1"
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)",
		settingKeyGroupForceRelay, val); err != nil {
		return fmt.Errorf("SetGroupForceRelay: persist: %w", err)
	}
	s.relayMu.Lock()
	s.forceRelay = force
	s.relayMu.Unlock()
	return nil
}

// GetGroupForceRelay возвращает текущее значение force-галки.
func (s *GroupsService) GetGroupForceRelay() (bool, error) {
	s.relayMu.RLock()
	defer s.relayMu.RUnlock()
	return s.forceRelay, nil
}

// SetGroupRelayThreshold меняет пороговое количество участников для перехода
// на server relay. Минимум 2 — иначе тест 1-на-1 невозможен.
func (s *GroupsService) SetGroupRelayThreshold(n int) error {
	if n < 2 {
		n = 2
	}
	if _, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)",
		settingKeyGroupRelayThr, strconv.Itoa(n)); err != nil {
		return fmt.Errorf("SetGroupRelayThreshold: persist: %w", err)
	}
	s.relayMu.Lock()
	s.relayThreshold = n
	s.relayMu.Unlock()
	return nil
}

// GetGroupRelayThreshold возвращает текущий порог.
func (s *GroupsService) GetGroupRelayThreshold() (int, error) {
	s.relayMu.RLock()
	defer s.relayMu.RUnlock()
	return s.relayThreshold, nil
}

// ---------------------------------------------------------------------------
// Decision: use server relay for groupID?
// ---------------------------------------------------------------------------

// useServerRelay решает, отправлять ли сообщения группы через server relay.
// Логика:
//  1. Если delivery_mode группы уже 'server' — да.
//  2. Иначе — force || memberCount >= threshold.
//
// Случай 2 может привести к регистрации группы на сервере (если ещё не
// регистрировали) — см. maybeRegisterGroup.
func (s *GroupsService) useServerRelay(groupID string) bool {
	var mode string
	_ = s.store.DB().QueryRow(
		"SELECT delivery_mode FROM groups WHERE group_id = ?", groupID).Scan(&mode)
	if mode == network.DeliveryModeServer {
		return true
	}
	s.relayMu.RLock()
	force := s.forceRelay
	threshold := s.relayThreshold
	s.relayMu.RUnlock()
	if force {
		return true
	}
	members := s.GetGroupMemberIDs(groupID)
	return len(members) >= threshold
}

// ---------------------------------------------------------------------------
// Registration / membership mirroring
// ---------------------------------------------------------------------------

// RegisterGroupOnServer отправляет group_register серверу. Идемпотентно:
// сервер обновляет membership при повторном вызове.
// delivery_mode='server' ставится только после получения group_register_ok.
func (s *GroupsService) RegisterGroupOnServer(groupID string) error {
	client := s.network.Client()
	if client == nil {
		return fmt.Errorf("RegisterGroupOnServer: not connected to signal")
	}

	members, admins := s.getGroupMembersAndAdmins(groupID)
	if len(members) == 0 {
		return fmt.Errorf("RegisterGroupOnServer: group has no members: %s", groupID)
	}

	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("RegisterGroupOnServer: session: %w", err)
	}
	// Сервер требует чтобы отправитель был в members. Если нас там нет —
	// значит что-то не так с локальным состоянием.
	if !slices.Contains(members, myID) {
		return fmt.Errorf("RegisterGroupOnServer: self not in members list")
	}

	if err := client.SendGroupRegister(groupID, members, admins); err != nil {
		return fmt.Errorf("RegisterGroupOnServer: send: %w", err)
	}
	s.log.Info("📦 group_register sent",
		slog.String("group_id", safeShort8(groupID)),
		slog.Int("members", len(members)),
		slog.Int("admins", len(admins)))
	return nil
}

// getGroupMembersAndAdmins возвращает списки активных участников и админов.
func (s *GroupsService) getGroupMembersAndAdmins(groupID string) (members, admins []string) {
	rows, err := s.store.DB().Query(
		"SELECT peer_id, role FROM group_members WHERE group_id = ? AND status = 'active'",
		groupID)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	for rows.Next() {
		var peerID, role string
		if err := rows.Scan(&peerID, &role); err != nil {
			continue
		}
		members = append(members, peerID)
		if role == "admin" {
			admins = append(admins, peerID)
		}
	}
	return members, admins
}

// MirrorMemberChange уведомляет сервер о добавлении/удалении участника в уже
// зарегистрированной группе. Вызывается после успешного P2P-процесса add/kick.
// Если группа на сервере не зарегистрирована — ничего не делает (no-op).
func (s *GroupsService) MirrorMemberChange(groupID, peerID string, add, isAdmin bool) {
	var mode string
	_ = s.store.DB().QueryRow(
		"SELECT delivery_mode FROM groups WHERE group_id = ?", groupID).Scan(&mode)
	if mode != network.DeliveryModeServer {
		return
	}
	client := s.network.Client()
	if client == nil {
		return
	}
	if err := client.SendGroupMemberChange(groupID, peerID, add, isAdmin); err != nil {
		s.log.Warn("mirror member change failed",
			slog.String("group_id", safeShort8(groupID)),
			slog.String("peer", safeShort(peerID)),
			slog.Bool("add", add),
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Sync at startup / reconnect
// ---------------------------------------------------------------------------

// SyncAllServerGroups для каждой локальной группы с delivery_mode='server'
// шлёт group_sync с курсорами журналов узлов, затем повторяет свои
// неподтверждённые сообщения. Вызывается после auth_ok и при reconnect.
// Блокировки не использует — запросы асинхронные, ответы обрабатывает
// HandleSignalMessage.
func (s *GroupsService) SyncAllServerGroups() {
	client := s.network.Client()
	if client == nil {
		return
	}
	rows, err := s.store.DB().Query(
		"SELECT group_id FROM groups WHERE delivery_mode = ?",
		network.DeliveryModeServer)
	if err != nil {
		s.log.Warn("sync all server groups: query", slog.String("error", err.Error()))
		return
	}
	var list []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			list = append(list, id)
		}
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("sync all server groups: rows", slog.String("error", err.Error()))
	}
	rows.Close()

	if len(list) > 0 {
		s.log.Info("syncing server-relay groups", slog.Int("count", len(list)))
	}
	for _, id := range list {
		if err := client.SendGroupSync(id, s.relayCursors(id), syncLimit); err != nil {
			s.log.Warn("group_sync send failed",
				slog.String("group_id", safeShort8(id)),
				slog.String("error", err.Error()))
		}
	}
	// Узел мог принять сообщение и упасть до подтверждения — повторяем.
	s.resendPendingRelay()
}

// ---------------------------------------------------------------------------
// Inbound dispatch
// ---------------------------------------------------------------------------

// HandleSignalMessage — единая точка входа для group_* сообщений от signal.
// Вызывается из NetworkService.onSignalMessage.
func (s *GroupsService) HandleSignalMessage(msg *network.Message) {
	switch msg.Type {
	case network.TypeGroupRegisterOK:
		s.handleGroupRegisterOK(msg)
	case network.TypeGroupMessageNew:
		s.handleGroupMessageNew(msg, false)
	case network.TypeGroupSyncResult:
		s.handleGroupSyncResult(msg)
	case network.TypeGroupDeliveryMode:
		s.handleGroupDeliveryMode(msg)
	case network.TypeGroupSyncHint:
		s.handleGroupSyncHint(msg)
	default:
		s.log.Warn("unknown group signal type", slog.String("type", msg.Type))
	}
}

func (s *GroupsService) handleGroupRegisterOK(msg *network.Message) {
	var p network.GroupRegisterOKPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.log.Warn("bad group_register_ok", slog.String("error", err.Error()))
		return
	}
	if _, err := s.store.DB().Exec(
		"UPDATE groups SET delivery_mode = ? WHERE group_id = ?",
		network.DeliveryModeServer, p.GroupID); err != nil {
		s.log.Warn("group_register_ok persist failed",
			slog.String("group_id", safeShort8(p.GroupID)),
			slog.String("error", err.Error()))
		return
	}
	s.log.Info("✅ group registered on server",
		slog.String("group_id", safeShort8(p.GroupID)),
		slog.Int("members", p.MemberCount),
		slog.Int64("current_seq", p.CurrentSeq))
	s.network.emitEvent("vox:group-mode-changed", map[string]any{
		"groupId": p.GroupID,
		"mode":    network.DeliveryModeServer,
	})
	// Догоняем историю, если что-то пропустили (например другой админ
	// зарегистрировал раньше и уже есть сообщения). Без оглядки на
	// current_seq: он считает только журнал ответившего узла, и у участника
	// на втором узле он ноль, хотя история лежит в журнале первого. Пустой
	// ответ по курсорам стоит дёшево.
	if client := s.network.Client(); client != nil {
		_ = client.SendGroupSync(p.GroupID, s.relayCursors(p.GroupID), syncLimit)
	}
}

// handleGroupSyncHint — узел получил догоном сообщения группы и не рассылает
// их поштучно: забираем сами. Без этого онлайн-клиент ждал бы
// переподключения — живой курсор двигается только непрерывно.
func (s *GroupsService) handleGroupSyncHint(msg *network.Message) {
	var p network.GroupSyncHintPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.GroupID == "" {
		s.log.Warn("bad group_sync_hint")
		return
	}
	client := s.network.Client()
	if client == nil {
		return // синхронизация пройдёт при переподключении
	}
	if err := client.SendGroupSync(p.GroupID, s.relayCursors(p.GroupID), syncLimit); err != nil {
		s.log.Warn("group_sync по подсказке не отправлен",
			slog.String("group_id", safeShort8(p.GroupID)),
			slog.String("error", err.Error()))
	}
}

func (s *GroupsService) handleGroupDeliveryMode(msg *network.Message) {
	var p network.GroupDeliveryModePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.log.Warn("bad group_delivery_mode", slog.String("error", err.Error()))
		return
	}
	if p.Mode != network.DeliveryModeP2P && p.Mode != network.DeliveryModeServer {
		s.log.Warn("unknown delivery mode", slog.String("mode", p.Mode))
		return
	}
	// Читаем предыдущий режим чтобы понять — это реальная смена, или дубль
	// (sender'у приходит и register_ok, и delivery_mode broadcast — не делать
	// sync дважды, иначе UI дёргает полную перерисовку чата).
	var prevMode string
	_ = s.store.DB().QueryRow(
		"SELECT delivery_mode FROM groups WHERE group_id = ?", p.GroupID).Scan(&prevMode)
	if prevMode == p.Mode {
		return
	}
	if _, err := s.store.DB().Exec(
		"UPDATE groups SET delivery_mode = ? WHERE group_id = ?", p.Mode, p.GroupID); err != nil {
		s.log.Warn("group_delivery_mode persist failed", slog.String("error", err.Error()))
		return
	}
	s.log.Info("group delivery mode changed",
		slog.String("group_id", safeShort8(p.GroupID)),
		slog.String("mode", p.Mode))
	s.network.emitEvent("vox:group-mode-changed", map[string]any{
		"groupId": p.GroupID,
		"mode":    p.Mode,
	})
	// При переходе на server — подтягиваем историю (только если этот клиент
	// НЕ инициатор register: иначе sender получит собственное сообщение
	// через обычный fan-out, двойной sync-роунд не нужен).
	if p.Mode == network.DeliveryModeServer {
		if client := s.network.Client(); client != nil {
			_ = client.SendGroupSync(p.GroupID, s.relayCursors(p.GroupID), syncLimit)
		}
	}
}

// handleGroupMessageNew обрабатывает fan-out от сервера (одно сообщение).
// Дедуп на уровне (group_id, seq) через UNIQUE INDEX + INSERT OR IGNORE.
// Для своих же сообщений (AuthorKey == myID): вероятно локальная строка уже
// есть с seq=0 → UPDATE seq/server_ts_ms. Иначе — обычный INSERT OR IGNORE.
// fromSync=true — сообщение из ответа group_sync, а не живой fan-out: курсор
// журнала тогда двигает вызывающий, по всей странице сразу.
func (s *GroupsService) handleGroupMessageNew(msg *network.Message, fromSync bool) {
	var p network.GroupMessageNewPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.log.Warn("bad group_message_new", slog.String("error", err.Error()))
		return
	}
	if p.GroupID == "" || p.Seq <= 0 {
		s.log.Warn("group_message_new missing fields",
			slog.String("group_id", p.GroupID), slog.Int64("seq", p.Seq))
		return
	}

	myID, _ := s.sess.PeerID()
	outbound := p.AuthorKey == myID

	// Нашли ли локально эту строку по client_msg_id?
	var localID, sendFailed string
	err := s.store.DB().QueryRow(
		"SELECT id, send_failed FROM group_messages WHERE id = ?", p.ClientMsgID).Scan(&localID, &sendFailed)
	alreadyHave := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Warn("local msg lookup failed", slog.String("error", err.Error()))
	}

	createdAt := p.ClientTsMs / 1000
	if createdAt <= 0 {
		createdAt = p.ServerTsMs / 1000
	}
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	// Признак "это первый раз мы увидели это сообщение" — если false, значит
	// приходит дубль (fan-out + sync одновременно, либо retry), UI уже отрисовал,
	// повторно эмитить event не нужно.
	firstSeen := !alreadyHave

	// Payload зашифрован для участников (groups_e2e.go). Своё уже лежит
	// локально открытым текстом — расшифровывать незачем.
	plain := ""
	if !(outbound && alreadyHave) {
		var derr error
		plain, derr = s.openRelay(&p)
		if derr != nil {
			if errors.Is(derr, e2e.ErrNotForMe) {
				s.log.Debug("сообщение группы до нашего вступления — пропущено",
					slog.String("group_id", safeShort8(p.GroupID)), slog.Int64("seq", p.Seq))
			} else {
				s.log.Warn("сообщение группы отвергнуто",
					slog.String("group_id", safeShort8(p.GroupID)),
					slog.String("peer", safeShort(p.AuthorKey)),
					slog.Int64("seq", p.Seq),
					slog.String("error", derr.Error()))
			}
			if !fromSync {
				s.bumpRelayCursorNext(p.GroupID, p.OriginNode, p.Seq)
			}
			return
		}
	}
	// Цитата и «переслано от» едут внутри (relay-конверт, см. reply_forward.go).
	content, relayOpts := decodeRelayPayload(plain)

	if alreadyHave {
		// Обновляем seq/server_ts — локальная строка была без них (свой же send
		// или из предыдущего запуска до миграции).
		// Первая серверная копия задаёт seq и журнал; копия того же
		// сообщения из другого журнала (повтор на другой узел) не перетирает.
		if _, err := s.store.DB().Exec(
			`UPDATE group_messages SET seq = ?, server_ts_ms = ?, origin_node = ?, send_failed = ''
			 WHERE id = ? AND seq = 0`,
			p.Seq, p.ServerTsMs, p.OriginNode, p.ClientMsgID); err != nil {
			s.log.Warn("update seq failed", slog.String("error", err.Error()))
		} else if sendFailed != "" {
			// Отказ оказался не окончательным: сообщение всё-таки принято
			// (другим узлом, через group_sync после переключения).
			s.network.emitEvent("vox:group-message-failed", map[string]any{
				"id":        p.ClientMsgID,
				"groupId":   p.GroupID,
				"code":      "",
				"delivered": true,
			})
		}
	} else {
		res, err := s.store.DB().Exec(
			`INSERT OR IGNORE INTO group_messages
			 (id, group_id, sender_id, content, type, created_at, seq, server_ts_ms, origin_node, reply_to, forwarded_from)
			 VALUES (?, ?, ?, ?, 'text', ?, ?, ?, ?, ?, ?)`,
			p.ClientMsgID, p.GroupID, p.AuthorKey, content, createdAt, p.Seq, p.ServerTsMs, p.OriginNode,
			encodeRef(relayOpts.ReplyTo), encodeRef(relayOpts.ForwardedFrom),
		)
		if err != nil {
			s.log.Warn("group_message_new insert failed", slog.String("error", err.Error()))
			return
		}
		// Строку мог молча отбросить уникальный индекс (group_id, origin_node,
		// seq): сервер выдал тот же seq повторно. Курсор двигать нельзя — он
		// уехал бы за несохранённое сообщение.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			s.log.Warn("group_message_new dropped: seq already taken",
				slog.String("group_id", safeShort8(p.GroupID)),
				slog.String("origin", safeShort(p.OriginNode)),
				slog.Int64("seq", p.Seq))
			return
		}
	}

	// Курсор по живому сообщению двигаем здесь; по ответу догона — одной
	// пачкой в handleGroupSyncResult, там он двигается и за строки, которые
	// записать не удалось.
	if !fromSync {
		s.bumpRelayCursorNext(p.GroupID, p.OriginNode, p.Seq)
	}

	if outbound {
		// Своё сообщение вернулось с seq — сервер его принял: повтор больше
		// не нужен. UI уже показал его, повторно не эмитим.
		s.ackRelay(p.ClientMsgID)
		return
	}
	if !firstSeen {
		// Дубль (fan-out + sync или retry) — UI уже отрисовал.
		return
	}

	// Никнейм автора для UI.
	nickname := s.network.PeerNickname(p.AuthorKey)

	var groupName string
	_ = s.store.DB().QueryRow(
		"SELECT name FROM groups WHERE group_id = ?", p.GroupID).Scan(&groupName)

	s.log.Info("📥 GROUP SERVER ПОЛУЧЕНО",
		slog.String("group_id", safeShort8(p.GroupID)),
		slog.String("peer", safeShort(p.AuthorKey)),
		slog.Int64("seq", p.Seq),
		slog.String("msg_id", p.ClientMsgID))

	s.network.emitEvent("vox:group-message", map[string]any{
		"groupId":   p.GroupID,
		"groupName": groupName,
		"message": map[string]any{
			"id":            p.ClientMsgID,
			"groupId":       p.GroupID,
			"senderId":      p.AuthorKey,
			"senderName":    nickname,
			"content":       content,
			"replyTo":       relayOpts.ReplyTo,
			"forwardedFrom": relayOpts.ForwardedFrom,
			"timestamp":     createdAt * 1000,
			"outbound":      false,
			"delivered":     true,
		},
	})
}

// handleGroupSyncResult батч-обрабатывает пропущенные сообщения.
// Для каждого вызываем handleGroupMessageNew через marshal-roundtrip — это
// проще чем дублировать логику, и частота sync'ов низкая.
func (s *GroupsService) handleGroupSyncResult(msg *network.Message) {
	var p network.GroupSyncResultPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.log.Warn("bad group_sync_result", slog.String("error", err.Error()))
		return
	}
	s.log.Info("group sync received",
		slog.String("group_id", safeShort8(p.GroupID)),
		slog.Int("messages", len(p.Messages)),
		slog.Bool("has_more", p.HasMore))

	for i := range p.Messages {
		raw, err := json.Marshal(p.Messages[i])
		if err != nil {
			continue
		}
		s.handleGroupMessageNew(&network.Message{
			Type:    network.TypeGroupMessageNew,
			Payload: raw,
		}, true)
	}

	// Курсоры двигаем по всей пачке, даже если какую-то строку не удалось
	// записать, иначе запрос зациклится. Не только при has_more: последняя
	// страница тоже должна сдвинуть границу, иначе её перекачаем заново.
	for _, m := range p.Messages {
		s.bumpRelayCursorMax(p.GroupID, m.OriginNode, m.Seq)
	}

	// Если ещё есть — продолжаем с курсоров. Пустую пачку не продолжаем:
	// has_more при нулевом списке дал бы бесконечный цикл по сокету.
	if p.HasMore && len(p.Messages) > 0 {
		if client := s.network.Client(); client != nil {
			_ = client.SendGroupSync(p.GroupID, s.relayCursors(p.GroupID), syncLimit)
		}
	}

	// После догона — одно событие на группу, пусть фронт перерисует чат.
	s.network.emitEvent("vox:group-messages-synced", map[string]any{
		"groupId": p.GroupID,
		"count":   len(p.Messages),
	})
}

// ---------------------------------------------------------------------------
// Outbound: sendViaRelay (вызывается из SendGroupMessage при useServerRelay)
// ---------------------------------------------------------------------------

// sendViaRelay пишет сообщение локально (seq=0, delivered=0) и шлёт через WS.
// Авторитетный seq придёт обратно в group_message_new — там seq обновится.
// Возвращает SendResult{ID, Delivered:true}: UI сразу показывает галочку,
// так как сервер — источник правды (если up, он либо примет, либо отклонит
// с error'ом — ошибку обработаем через events, но в happy path считаем OK).
func (s *GroupsService) sendViaRelay(groupID, text string, opts SendOptions) (*SendResult, error) {
	const op = "GroupsService.sendViaRelay"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	// Размер — до всего остального: не влезающее в лимиты сервера не должно ни
	// регистрировать группу, ни оставлять строку, ни уходить в сеть. Кадр
	// больше предела сервер рвёт вместе с соединением.
	msgID := genClientMsgID()
	payload, err := s.relayPayload(groupID, msgID, text, opts) // шифротекст для участников (groups_e2e.go)
	if err != nil {
		return nil, fmt.Errorf("%s: encrypt: %w", op, err)
	}
	if !relayFits(groupID, msgID, payload) {
		return nil, fmt.Errorf("%s: %w", op, ErrRelayTooLarge)
	}

	// Автоматическая lazy-регистрация: если force включили уже после создания
	// группы и delivery_mode ещё 'p2p' — регистрируем перед отправкой.
	// NB: server принимает group_register идемпотентно.
	var mode string
	_ = s.store.DB().QueryRow(
		"SELECT delivery_mode FROM groups WHERE group_id = ?", groupID).Scan(&mode)
	if mode != network.DeliveryModeServer {
		if err := s.RegisterGroupOnServer(groupID); err != nil {
			return nil, fmt.Errorf("%s: register: %w", op, err)
		}
		// Не меняем mode локально сразу — дождёмся register_ok, но первое
		// сообщение всё равно отправляем: сервер обработает register и
		// message_send в порядке поступления.
	}

	client := s.network.Client()
	if client == nil {
		return nil, fmt.Errorf("%s: not connected", op)
	}

	nowSec := time.Now().Unix()
	nowMs := nowSec * 1000

	// Локально записываем СРАЗУ, чтобы UI не ждал round-trip'а сервера.
	// seq=0, server_ts_ms=0 — будут заполнены когда придёт fan-out к нам.
	if _, err := s.store.DB().Exec(
		`INSERT INTO group_messages (id, group_id, sender_id, content, type, created_at, reply_to, forwarded_from)
		 VALUES (?, ?, ?, ?, 'text', ?, ?, ?)`,
		msgID, groupID, myID, text, nowSec, encodeRef(opts.ReplyTo), encodeRef(opts.ForwardedFrom)); err != nil {
		return nil, fmt.Errorf("%s: insert: %w", op, err)
	}

	// Ждёт подтверждения (своя копия с seq): если узел примет и упадёт, не
	// успев разослать, сообщение уйдёт повторно (groups_relay_retry.go).
	if err := s.addPendingRelay(groupID, msgID); err != nil {
		s.log.Warn("pending relay insert failed", slog.String("error", err.Error()))
	}

	if err := client.SendGroupMessageRelay(groupID, msgID, payload, true, nowMs); err != nil {
		// Удаляем локальную строку — иначе застрянет без seq навсегда.
		_, _ = s.store.DB().Exec("DELETE FROM group_messages WHERE id = ?", msgID)
		s.ackRelay(msgID)
		return nil, fmt.Errorf("%s: send: %w", op, err)
	}
	s.scheduleRelayRetry(groupID, msgID, 0)

	s.log.Info("📤 GROUP SERVER ОТПРАВЛЕНО",
		slog.String("group_id", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		logsafe.Len("len", text))

	return &SendResult{ID: msgID, Delivered: true}, nil
}

// genClientMsgID — UUID для client_msg_id.
func genClientMsgID() string {
	return uuid.New().String()
}
