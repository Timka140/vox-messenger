package services

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"vox_app/internal/network"
	"vox_app/internal/session"
	"vox_app/internal/storage"

	"github.com/google/uuid"
)

// CallPopup — интерфейс для управления окном входящего звонка
// CallPopup — окно входящего звонка (реализация в main.go поверх Wails).
// Show показывает окно по центру, НЕ забирая фокус у активного приложения.
type CallPopup interface {
	Show()
	Close()
}

// CallsService управляет звонками: инициация, приём, завершение, история
type CallsService struct {
	log     *slog.Logger
	sess    *session.Session
	store   *storage.Storage
	network *NetworkService
	voice   *VoiceService

	mu             sync.Mutex
	activeCall     *ActiveCall
	pendingInvites map[string]*PendingInvite // callID → входящее приглашение
	ringTimer      *time.Timer
	reconnTimers   map[string]*time.Timer // peerID → grace period таймер (2 мин)

	// Попап окно входящего звонка
	popupMu      sync.Mutex
	popupWindow  CallPopup
	popupCallID  string // звонок, для которого открыто окно
	popupGen     int    // поколение окна: WindowClosing от нашего Close() игнорируется
	createWindow func(title string, w, h int, urlPath string, onClosed func()) CallPopup
	// showIncomingToast — карточка звонка в окне уведомлений (NotifyService).
	// true — показана, popup-окно не нужно. toastCallID — какой звонок
	// висит карточкой, чтобы снять её при accept/reject/end/timeout.
	showIncomingToast func(info IncomingCallInfo) bool
	toastCallID       string
}

// ActiveCall — текущий активный звонок
type ActiveCall struct {
	ID           string
	CreatorID    string
	Type         string // "dm" или "group"
	GroupID      string // type=2 группа для чата звонка
	Participants map[string]*CallParticipant
	Ringing      map[string]bool
	StartedAt    time.Time
}

// CallParticipant — участник звонка
type CallParticipant struct {
	PeerID   string
	Nickname string
	Status   string // "ringing", "active", "left", "rejected"
	JoinedAt time.Time
}

// PendingInvite — входящее приглашение, ожидающее ответа
type PendingInvite struct {
	CallID       string
	FromPeerID   string
	NickTag      string
	Type         string
	Participants []string
	GroupID      string // groupID от инициатора (при reconnect)
	ReceivedAt   time.Time
}

// CallHistoryItem — запись в истории звонков
type CallHistoryItem struct {
	ID        string `json:"id"`
	PeerID    string `json:"peerId"` // собеседник (DM) — профиль друга фильтрует историю по нему
	CreatorID string `json:"creatorId"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	PeerNick  string `json:"peerNick"`
	CreatedAt int64  `json:"createdAt"`
	EndedAt   int64  `json:"endedAt"`
	Duration  int64  `json:"duration"`
	Outbound  bool   `json:"outbound"`
}

func NewCallsService(log *slog.Logger, sess *session.Session, store *storage.Storage,
	network *NetworkService, voice *VoiceService) *CallsService {
	return &CallsService{
		log:            log.With(slog.String("service", "calls")),
		sess:           sess,
		store:          store,
		network:        network,
		voice:          voice,
		pendingInvites: make(map[string]*PendingInvite),
		reconnTimers:   make(map[string]*time.Timer),
	}
}

// InitCallbacks подключает колбэки к NetworkService
func (s *CallsService) InitCallbacks() {
	s.network.SetOnCallSignal(s.handleCallSignal)
}

// SetWindowCreator устанавливает функцию создания окна входящего звонка.
// onClosed вызывается, когда окно закрыл пользователь (крестик / Alt+F4) —
// это отклонение звонка.
func (s *CallsService) SetWindowCreator(fn func(title string, w, h int, urlPath string, onClosed func()) CallPopup) {
	s.createWindow = fn
}

// SetIncomingNotifier — показывать входящий звонок карточкой в окне
// уведомлений; при false (режим system/off) остаётся popup-окно.
func (s *CallsService) SetIncomingNotifier(fn func(info IncomingCallInfo) bool) {
	s.showIncomingToast = fn
}

// incomingInfo — данные карточки входящего: группа и число участников из
// pendingInvites. Зовётся ДО popupMu — берёт s.mu.
func (s *CallsService) incomingInfo(callID, nickname, peerID, callType string, busy bool) IncomingCallInfo {
	info := IncomingCallInfo{CallID: callID, Nickname: nickname, PeerID: peerID, Type: callType, Busy: busy}
	s.mu.Lock()
	if inv, ok := s.pendingInvites[callID]; ok {
		info.GroupID = inv.GroupID
		info.Participants = len(inv.Participants)
	}
	s.mu.Unlock()
	if info.GroupID != "" && s.store != nil {
		_ = s.store.DB().QueryRow("SELECT name FROM groups WHERE group_id = ?", info.GroupID).Scan(&info.GroupName)
	}
	return info
}

// Входящий звонок во время полноэкранной игры/презентации (userBusy) поверх
// неё не показываем (решение Тима), а откладываем: как только пользователь
// вышел из полноэкранного режима и звонок ещё звонит — показываем окно.
// Событий о выходе из полноэкранного режима Windows не шлёт, поэтому проверка
// раз в incomingDeferPoll, только пока звонок висит в pendingInvites.
var (
	incomingBusyFn    = userBusy
	incomingDeferPoll = time.Second
)

const incomingDeferMax = 2 * time.Minute

func (s *CallsService) showIncomingCallPopup(callID, nickname, peerID, callType string, busy bool) {
	if incomingBusyFn() {
		s.log.Info("входящий звонок отложен: полноэкранное приложение", slog.String("callId", safeShort8(callID)))
		go s.deferIncomingPopup(callID, nickname, peerID, callType, busy)
		return
	}
	info := s.incomingInfo(callID, nickname, peerID, callType, busy)

	s.popupMu.Lock()
	defer s.popupMu.Unlock()

	// Закрыть предыдущий если есть
	s.closePopupLocked()

	// Личный звонок — всегда отдельное окно по центру экрана (решение по
	// макету IncomingCall); карточка в окне уведомлений остаётся только для
	// приглашений в групповой звонок (и как fallback, если окна нет).
	if callType != "dm" || s.createWindow == nil {
		if s.showIncomingToast != nil && s.showIncomingToast(info) {
			s.toastCallID = callID
			return
		}
	}

	if s.createWindow == nil {
		return
	}

	urlPath := fmt.Sprintf("/incoming-call.html?callId=%s&nickname=%s&peerId=%s&type=%s&busy=%v",
		url.QueryEscape(callID),
		url.QueryEscape(nickname),
		url.QueryEscape(peerID),
		url.QueryEscape(callType),
		busy)

	s.popupGen++
	gen := s.popupGen
	popup := s.createWindow("Incoming Call", incomingCallWinW, incomingCallWinH, urlPath, func() { s.onPopupClosedByUser(gen) })
	if popup != nil {
		s.popupWindow = popup
		s.popupCallID = callID
		popup.Show()
	}
}

// Размер окна входящего звонка — по макету design/vox-redesign/IncomingCall.
const (
	incomingCallWinW = 360
	incomingCallWinH = 236
)

// deferIncomingPopup — показать отложенный входящий, когда пользователь выйдет
// из полноэкранного режима. Звонок принят/отклонён/отменён (нет в
// pendingInvites) или прошло incomingDeferMax — выходим без показа.
func (s *CallsService) deferIncomingPopup(callID, nickname, peerID, callType string, busy bool) {
	tick := time.NewTicker(incomingDeferPoll)
	defer tick.Stop()
	deadline := time.Now().Add(incomingDeferMax)
	for range tick.C {
		s.mu.Lock()
		_, pending := s.pendingInvites[callID]
		s.mu.Unlock()
		if !pending || time.Now().After(deadline) {
			s.log.Info("отложенный входящий снят", slog.String("callId", safeShort8(callID)), slog.Bool("pending", pending))
			return
		}
		if !incomingBusyFn() {
			s.log.Info("отложенный входящий показан", slog.String("callId", safeShort8(callID)))
			s.showIncomingCallPopup(callID, nickname, peerID, callType, busy)
			return
		}
	}
}

// onPopupClosedByUser — окно входящего закрыто пользователем (крестик /
// Alt+F4): это отклонение. Наш собственный Close() (accept/reject/end/timeout)
// сюда не попадает — поколение уже сдвинуто в closePopupLocked.
func (s *CallsService) onPopupClosedByUser(gen int) {
	s.popupMu.Lock()
	if gen != s.popupGen || s.popupWindow == nil {
		s.popupMu.Unlock()
		return
	}
	callID := s.popupCallID
	s.popupWindow = nil
	s.popupCallID = ""
	s.popupMu.Unlock()
	if callID != "" {
		s.log.Info("incoming call window closed by user → reject", slog.String("callId", callID))
		if err := s.RejectCall(callID); err != nil {
			s.log.Warn("reject after window close", slog.String("error", err.Error()))
		}
	}
}

func (s *CallsService) closeIncomingCallPopup() {
	s.popupMu.Lock()
	defer s.popupMu.Unlock()
	s.closePopupLocked()
}

func (s *CallsService) closePopupLocked() {
	if s.toastCallID != "" {
		s.network.EmitEvent("vox:toast-call-end", map[string]any{"callId": s.toastCallID})
		s.toastCallID = ""
	}
	if s.popupWindow != nil {
		win := s.popupWindow
		s.popupWindow = nil
		s.popupCallID = ""
		s.popupGen++ // WindowClosing от этого Close() не считается отклонением
		func() {
			defer func() { recover() }() // окно может быть уже закрыто
			win.Close()
		}()
	}
}

// Shutdown завершает активный звонок при выходе
func (s *CallsService) Shutdown() {
	s.closeIncomingCallPopup()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeCall != nil {
		s.leaveCallInternal()
	}
}

// --- Wails-bound методы ---

// StartCall инициирует звонок к одному или нескольким пирам
func (s *CallsService) StartCall(peerIDs []string) (map[string]any, error) {
	const op = "CallsService.StartCall"

	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	s.mu.Lock()
	// Если в звонке — выходим
	if s.activeCall != nil {
		s.leaveCallInternal()
	}
	s.mu.Unlock()

	// Если в голосовом канале — выходим
	s.voice.LeaveGroupVoice()

	callID := uuid.New().String()
	now := time.Now()

	callType := "dm"
	if len(peerIDs) > 1 {
		callType = "group"
	}

	s.mu.Lock()
	s.activeCall = &ActiveCall{
		ID:           callID,
		CreatorID:    myID,
		Type:         callType,
		Participants: make(map[string]*CallParticipant),
		Ringing:      make(map[string]bool),
		StartedAt:    now,
	}

	// Добавляем себя
	s.activeCall.Participants[myID] = &CallParticipant{
		PeerID: myID, Status: "active", JoinedAt: now,
	}

	// Добавляем приглашённых
	for _, pid := range peerIDs {
		nick := s.network.PeerNickname(pid)
		s.activeCall.Participants[pid] = &CallParticipant{
			PeerID: pid, Nickname: nick, Status: "ringing",
		}
		s.activeCall.Ringing[pid] = true
	}
	s.mu.Unlock()

	// Сохраняем в БД
	s.store.DB().Exec(
		"INSERT INTO calls (id, creator_id, type, status, created_at) VALUES (?, ?, ?, 'ringing', ?)",
		callID, myID, callType, now.Unix())
	s.store.DB().Exec(
		"INSERT INTO call_participants (call_id, peer_id, status, joined_at) VALUES (?, ?, 'active', ?)",
		callID, myID, now.Unix())
	for _, pid := range peerIDs {
		s.store.DB().Exec(
			"INSERT INTO call_participants (call_id, peer_id, status) VALUES (?, ?, 'ringing')",
			callID, pid)
	}

	// Получаем nickTag для invite
	nickTag, _ := s.network.getNickTag()

	// Отправляем call_invite через signal server
	client := s.network.Client()
	if client == nil {
		return nil, fmt.Errorf("%s: not connected to signal server", op)
	}

	for _, pid := range peerIDs {
		if err := client.SendCallInvite(pid, callID, nickTag, peerIDs, ""); err != nil {
			s.log.Warn("не удалось отправить call_invite",
				slog.String("to", safeShort(pid)),
				slog.String("error", err.Error()))
		}
	}

	// Запускаем таймер рингтона (60 сек)
	s.startRingTimer()

	s.log.Info("звонок инициирован",
		slog.String("callId", safeShort8(callID)),
		slog.String("type", callType),
		slog.Int("peers", len(peerIDs)))

	return map[string]any{
		"callId":    callID,
		"type":      callType,
		"creatorId": myID,
		"startedAt": now.UnixMilli(),
	}, nil
}

// AcceptCall принимает входящий звонок
func (s *CallsService) AcceptCall(callID string) error {
	const op = "CallsService.AcceptCall"

	// Выходим из голосового канала
	s.voice.LeaveGroupVoice()

	s.mu.Lock()

	invite, ok := s.pendingInvites[callID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%s: invite not found: %s", op, callID)
	}
	delete(s.pendingInvites, callID)
	s.mu.Unlock()

	s.closeIncomingCallPopup()

	s.mu.Lock()
	// Если в другом звонке — выходим
	if s.activeCall != nil {
		s.leaveCallInternal()
	}

	myID, _ := s.sess.PeerID()
	now := time.Now()

	// Создаём ActiveCall
	s.activeCall = &ActiveCall{
		ID:           callID,
		CreatorID:    invite.FromPeerID,
		Type:         invite.Type,
		Participants: make(map[string]*CallParticipant),
		Ringing:      make(map[string]bool),
		StartedAt:    now,
	}

	// Добавляем себя
	s.activeCall.Participants[myID] = &CallParticipant{
		PeerID: myID, Status: "active", JoinedAt: now,
	}

	// Добавляем инициатора
	s.activeCall.Participants[invite.FromPeerID] = &CallParticipant{
		PeerID:   invite.FromPeerID,
		Nickname: invite.NickTag,
		Status:   "active",
	}

	// Добавляем остальных участников
	for _, pid := range invite.Participants {
		if pid != myID && pid != invite.FromPeerID {
			s.activeCall.Participants[pid] = &CallParticipant{
				PeerID:   pid,
				Nickname: s.network.PeerNickname(pid),
				Status:   "ringing",
			}
		}
	}

	s.mu.Unlock()

	// Сохраняем в БД
	s.store.DB().Exec(
		"INSERT OR IGNORE INTO calls (id, creator_id, type, status, created_at) VALUES (?, ?, ?, 'active', ?)",
		callID, invite.FromPeerID, invite.Type, now.Unix())
	s.store.DB().Exec(
		"INSERT OR REPLACE INTO call_participants (call_id, peer_id, status, joined_at) VALUES (?, ?, 'active', ?)",
		callID, myID, now.Unix())

	// Создаём группу для голоса ДО отправки accept (чтобы передать groupId)
	// Если в invite есть groupID (reconnect) — используем его
	s.mu.Lock()
	if s.activeCall != nil && s.activeCall.GroupID == "" {
		if invite.GroupID != "" {
			s.adoptCallGroup(invite.GroupID, invite.FromPeerID)
		} else {
			s.createCallGroup()
		}
	}
	groupID := ""
	if s.activeCall != nil {
		groupID = s.activeCall.GroupID
	}
	s.mu.Unlock()

	// Отправляем call_accept с groupId через signal server
	client := s.network.Client()
	if client != nil {
		client.SendCallAccept(invite.FromPeerID, callID, groupID)
	}

	// Устанавливаем P2P с инициатором
	s.ensureP2P(invite.FromPeerID)

	// Устанавливаем P2P с остальными участниками
	for _, pid := range invite.Participants {
		if pid != myID && pid != invite.FromPeerID {
			s.ensureP2P(pid)
		}
	}

	s.log.Info("звонок принят",
		slog.String("callId", safeShort8(callID)),
		slog.String("from", safeShort(invite.FromPeerID)))

	// Уведомляем фронтенд acceptor'а о начале звонка (чтобы UI переключился)
	s.network.EmitEvent("vox:call-accepted", map[string]any{
		"callId":   callID,
		"peerId":   invite.FromPeerID,
		"nickname": s.network.PeerNickname(invite.FromPeerID),
	})

	return nil
}

// RejectCall отклоняет входящий звонок
func (s *CallsService) RejectCall(callID string) error {
	s.mu.Lock()
	invite, ok := s.pendingInvites[callID]
	if ok {
		delete(s.pendingInvites, callID)
	}
	s.mu.Unlock()
	if ok && invite != nil && invite.Type == "dm" {
		s.recordCallMessage(invite.FromPeerID, false, "declined", 0)
	}

	s.closeIncomingCallPopup()

	if !ok {
		return nil
	}

	myID, _ := s.sess.PeerID()

	// Отправляем call_reject
	client := s.network.Client()
	if client != nil {
		client.SendCallReject(invite.FromPeerID, callID)
	}

	// Сохраняем как missed
	s.store.DB().Exec(
		"INSERT OR IGNORE INTO calls (id, creator_id, type, status, created_at) VALUES (?, ?, ?, 'missed', ?)",
		callID, invite.FromPeerID, invite.Type, invite.ReceivedAt.Unix())
	s.store.DB().Exec(
		"INSERT OR REPLACE INTO call_participants (call_id, peer_id, status) VALUES (?, ?, 'rejected')",
		callID, myID)

	s.log.Info("звонок отклонён", slog.String("callId", safeShort8(callID)))
	return nil
}

// LeaveCall выходит из текущего звонка
func (s *CallsService) LeaveCall() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return nil
	}

	s.leaveCallInternal()
	return nil
}

// EndCallForAll завершает звонок для всех (только создатель)
func (s *CallsService) EndCallForAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return nil
	}

	myID, _ := s.sess.PeerID()
	if s.activeCall.CreatorID != myID {
		return fmt.Errorf("only creator can end call for all")
	}

	callID := s.activeCall.ID
	client := s.network.Client()

	// Отправляем call_end всем
	for pid, cp := range s.activeCall.Participants {
		if pid != myID && (cp.Status == "active" || cp.Status == "ringing") {
			if client != nil {
				client.SendCallEnd(pid, callID)
			}
		}
	}

	s.endCallInternal("ended")

	s.log.Info("звонок завершён для всех", slog.String("callId", safeShort8(callID)))
	return nil
}

// KickFromCall кикает участника из звонка (только создатель)
func (s *CallsService) KickFromCall(peerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return nil
	}

	myID, _ := s.sess.PeerID()
	if s.activeCall.CreatorID != myID {
		return fmt.Errorf("only creator can kick")
	}

	client := s.network.Client()
	if client != nil {
		client.SendCallEnd(peerID, s.activeCall.ID)
	}

	if cp, ok := s.activeCall.Participants[peerID]; ok {
		cp.Status = "left"
	}

	now := time.Now()
	s.store.DB().Exec(
		"UPDATE call_participants SET status = 'left', left_at = ? WHERE call_id = ? AND peer_id = ?",
		now.Unix(), s.activeCall.ID, peerID)

	s.network.EmitEvent("vox:call-participant-left", map[string]any{
		"peerId": peerID, "callId": s.activeCall.ID,
	})

	s.log.Info("участник кикнут из звонка",
		slog.String("peer", safeShort(peerID)),
		slog.String("callId", safeShort8(s.activeCall.ID)))

	return nil
}

// AddToCall добавляет участников в текущий звонок
func (s *CallsService) AddToCall(peerIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return nil
	}

	if s.activeCall.Type == "dm" && len(peerIDs) > 0 {
		s.activeCall.Type = "group"
	}

	nickTag, _ := s.network.getNickTag()
	client := s.network.Client()

	// Собираем всех текущих участников для invite
	allPIDs := make([]string, 0, len(s.activeCall.Participants))
	for pid := range s.activeCall.Participants {
		allPIDs = append(allPIDs, pid)
	}

	for _, pid := range peerIDs {
		if _, exists := s.activeCall.Participants[pid]; exists {
			continue
		}

		nick := s.network.PeerNickname(pid)
		s.activeCall.Participants[pid] = &CallParticipant{
			PeerID: pid, Nickname: nick, Status: "ringing",
		}
		s.activeCall.Ringing[pid] = true

		s.store.DB().Exec(
			"INSERT OR IGNORE INTO call_participants (call_id, peer_id, status) VALUES (?, ?, 'ringing')",
			s.activeCall.ID, pid)

		if client != nil {
			client.SendCallInvite(pid, s.activeCall.ID, nickTag, allPIDs, s.activeCall.GroupID)
		}

		// Уведомляем текущих участников
		s.network.EmitEvent("vox:call-participant-joined", map[string]any{
			"peerId":   pid,
			"nickname": nick,
			"callId":   s.activeCall.ID,
		})
	}

	// Перезапускаем таймер для новых участников
	if len(s.activeCall.Ringing) > 0 {
		s.startRingTimerLocked()
	}

	return nil
}

// GetCallHistory возвращает историю звонков
func (s *CallsService) GetCallHistory() ([]CallHistoryItem, error) {
	myID, err := s.sess.PeerID()
	if err != nil {
		return nil, err
	}

	rows, err := s.store.DB().Query(`
		SELECT c.id, c.creator_id, c.type, c.status, c.created_at, COALESCE(c.ended_at, 0)
		FROM calls c
		ORDER BY c.created_at DESC
		LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []CallHistoryItem
	for rows.Next() {
		var item CallHistoryItem
		if err := rows.Scan(&item.ID, &item.CreatorID, &item.Type, &item.Status,
			&item.CreatedAt, &item.EndedAt); err != nil {
			continue
		}
		item.Outbound = item.CreatorID == myID
		if item.EndedAt > 0 {
			// Длительность считаем от ответа собеседника (joined_at участника,
			// не создателя) — так же, как пузырь звонка в чате; created_at
			// включает дозвон. Fallback на created_at для старых записей.
			var answeredAt int64
			s.store.DB().QueryRow(
				"SELECT COALESCE(MIN(joined_at), 0) FROM call_participants WHERE call_id = ? AND peer_id != ? AND joined_at > 0",
				item.ID, item.CreatorID).Scan(&answeredAt)
			start := item.CreatedAt
			if answeredAt > start && answeredAt <= item.EndedAt {
				start = answeredAt
			}
			item.Duration = item.EndedAt - start
		}

		// Получаем никнейм собеседника
		var peerID string
		if item.Outbound {
			s.store.DB().QueryRow(
				"SELECT peer_id FROM call_participants WHERE call_id = ? AND peer_id != ? LIMIT 1",
				item.ID, myID).Scan(&peerID)
		} else {
			peerID = item.CreatorID
		}
		if peerID != "" {
			item.PeerID = peerID
			item.PeerNick = s.network.PeerNickname(peerID)
		}

		items = append(items, item)
	}

	if items == nil {
		items = []CallHistoryItem{}
	}
	return items, nil
}

// ClearCallHistory очищает историю звонков. Обе таблицы чистятся в одной
// транзакции — иначе при сбое первого DELETE остались бы orphaned записи в
// call_participants без соответствующих calls.
func (s *CallsService) ClearCallHistory() error {
	tx, err := s.store.DB().Begin()
	if err != nil {
		return fmt.Errorf("ClearCallHistory: begin tx: %w", err)
	}
	defer tx.Rollback() // no-op после успешного Commit

	if _, err := tx.Exec("DELETE FROM call_participants"); err != nil {
		return fmt.Errorf("ClearCallHistory: delete participants: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM calls"); err != nil {
		return fmt.Errorf("ClearCallHistory: delete calls: %w", err)
	}
	return tx.Commit()
}

// GetActiveCall возвращает информацию о текущем звонке
func (s *CallsService) GetActiveCall() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return nil
	}

	participants := make([]map[string]any, 0)
	for _, cp := range s.activeCall.Participants {
		participants = append(participants, map[string]any{
			"peerId":   cp.PeerID,
			"nickname": cp.Nickname,
			"status":   cp.Status,
			"muted":    false,
			"speaking": false,
		})
	}

	return map[string]any{
		"id":           s.activeCall.ID,
		"type":         s.activeCall.Type,
		"creatorId":    s.activeCall.CreatorID,
		"startedAt":    s.activeCall.StartedAt.UnixMilli(),
		"participants": participants,
	}
}

// CleanupOldCalls удаляет старые записи звонков
func (s *CallsService) CleanupOldCalls() {
	var enabled string
	s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'call_history_cleanup'").Scan(&enabled)
	if enabled == "0" {
		return
	}

	days := 60
	var daysStr string
	if s.store.DB().QueryRow("SELECT value FROM settings WHERE key = 'call_history_retention_days'").Scan(&daysStr) == nil {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	s.store.DB().Exec("DELETE FROM call_participants WHERE call_id IN (SELECT id FROM calls WHERE created_at < ?)", cutoff)
	res, _ := s.store.DB().Exec("DELETE FROM calls WHERE created_at < ?", cutoff)
	if affected, _ := res.RowsAffected(); affected > 0 {
		s.log.Info("очистка истории звонков", slog.Int64("deleted", affected))
	}
}

// --- Signal message handlers ---

func (s *CallsService) handleCallSignal(from string, msgType string, payload json.RawMessage) {
	switch msgType {
	case network.TypeCallInvite:
		s.handleCallInvite(from, payload)
	case network.TypeCallAccept:
		s.handleCallAccept(from, payload)
	case network.TypeCallReject:
		s.handleCallReject(from, payload)
	case network.TypeCallEnd:
		s.handleCallEnd(from, payload)
	}
}

func (s *CallsService) handleCallInvite(from string, payload json.RawMessage) {
	var p network.CallInvitePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		s.log.Warn("невалидный call_invite", slog.String("error", err.Error()))
		return
	}

	nick := s.network.PeerNickname(from)
	if nick == "" || nick == from[:min(16, len(from))] {
		nick = p.NickTag
	}

	callType := "dm"
	if len(p.Participants) > 1 {
		callType = "group"
	}

	s.mu.Lock()
	// Сохраняем invite
	s.pendingInvites[p.CallID] = &PendingInvite{
		CallID:       p.CallID,
		FromPeerID:   from,
		NickTag:      nick,
		Type:         callType,
		Participants: p.Participants,
		GroupID:      p.GroupID,
		ReceivedAt:   time.Now(),
	}

	busy := s.activeCall != nil
	s.mu.Unlock()

	// Готовим данные участников для фронтенда
	var participants []map[string]string
	for _, pid := range p.Participants {
		pnick := s.network.PeerNickname(pid)
		participants = append(participants, map[string]string{
			"peerId": pid, "nickname": pnick,
		})
	}

	s.network.EmitEvent("vox:call-incoming", map[string]any{
		"callId":       p.CallID,
		"peerId":       from,
		"nickname":     nick,
		"type":         callType,
		"participants": participants,
		"busy":         busy,
	})

	// Показать нативный попап
	s.showIncomingCallPopup(p.CallID, nick, from, callType, busy)

	s.log.Info("входящий звонок",
		slog.String("from", safeShort(from)),
		slog.String("callId", safeShort8(p.CallID)),
		slog.Bool("busy", busy))
}

func (s *CallsService) handleCallAccept(from string, payload json.RawMessage) {
	var p network.CallResponsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil || s.activeCall.ID != p.CallID {
		return
	}

	// Обновляем статус участника
	now := time.Now()
	if cp, ok := s.activeCall.Participants[from]; ok {
		cp.Status = "active"
		cp.JoinedAt = now
	}
	delete(s.activeCall.Ringing, from)

	// Обновляем БД
	s.store.DB().Exec("UPDATE calls SET status = 'active' WHERE id = ? AND status = 'ringing'", p.CallID)
	s.store.DB().Exec("UPDATE call_participants SET status = 'active', joined_at = ? WHERE call_id = ? AND peer_id = ?",
		now.Unix(), p.CallID, from)

	// Используем groupId от acceptor'а (или создаём свой как fallback)
	if s.activeCall.GroupID == "" {
		if p.GroupID != "" {
			s.adoptCallGroup(p.GroupID, from)
		} else {
			s.createCallGroup()
		}
	} else {
		// Добавляем нового участника в группу
		s.store.DB().Exec(
			"INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at, status) VALUES (?, ?, 'member', ?, 'active')",
			s.activeCall.GroupID, from, now.Unix())
		s.network.RefreshPresence() // участник группы звонка видит мой статус «в сети»
	}

	// P2P с принявшим
	s.ensureP2PLocked(from)

	nick := s.network.PeerNickname(from)
	s.network.EmitEvent("vox:call-accepted", map[string]any{
		"callId":   p.CallID,
		"peerId":   from,
		"nickname": nick,
	})

	// Если все ответили — останавливаем таймер
	if len(s.activeCall.Ringing) == 0 {
		s.stopRingTimerLocked()
	}

	s.log.Info("звонок принят",
		slog.String("by", safeShort(from)),
		slog.String("callId", safeShort8(p.CallID)))
}

func (s *CallsService) handleCallReject(from string, payload json.RawMessage) {
	var p network.CallResponsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil || s.activeCall.ID != p.CallID {
		return
	}

	if cp, ok := s.activeCall.Participants[from]; ok {
		cp.Status = "rejected"
	}
	delete(s.activeCall.Ringing, from)

	s.store.DB().Exec("UPDATE call_participants SET status = 'rejected' WHERE call_id = ? AND peer_id = ?",
		p.CallID, from)

	s.network.EmitEvent("vox:call-rejected", map[string]any{
		"callId": p.CallID, "peerId": from,
	})

	// Если никто не звонит и никто не в звонке — завершаем
	activeCount := 0
	for _, cp := range s.activeCall.Participants {
		if cp.Status == "active" {
			activeCount++
		}
	}
	if len(s.activeCall.Ringing) == 0 && activeCount <= 1 {
		s.endCallInternal("missed")
	}

	s.log.Info("звонок отклонён",
		slog.String("by", safeShort(from)),
		slog.String("callId", safeShort8(p.CallID)))
}

func (s *CallsService) handleCallEnd(from string, payload json.RawMessage) {
	var p network.CallResponsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Может быть для pending invite (звонящий отменил)
	if inv, ok := s.pendingInvites[p.CallID]; ok {
		delete(s.pendingInvites, p.CallID)
		// Сохраняем как missed
		s.store.DB().Exec(
			"INSERT OR IGNORE INTO calls (id, creator_id, type, status, created_at) VALUES (?, ?, ?, 'missed', ?)",
			p.CallID, inv.FromPeerID, inv.Type, inv.ReceivedAt.Unix())
		if inv.Type == "dm" {
			s.recordCallMessage(inv.FromPeerID, false, "missed", 0)
		}
		s.network.EmitEvent("vox:call-ended", map[string]any{"callId": p.CallID})
		go s.closeIncomingCallPopup() // закрыть попап (в горутине чтобы не блокировать mu)
		return
	}

	// Активный звонок
	if s.activeCall == nil || s.activeCall.ID != p.CallID {
		return
	}

	if cp, ok := s.activeCall.Participants[from]; ok {
		cp.Status = "left"
	}

	s.store.DB().Exec("UPDATE call_participants SET status = 'left', left_at = ? WHERE call_id = ? AND peer_id = ?",
		time.Now().Unix(), p.CallID, from)

	s.network.EmitEvent("vox:call-participant-left", map[string]any{
		"peerId": from, "callId": p.CallID,
	})

	// Если создатель вышел или я один — завершаем
	activeCount := 0
	for _, cp := range s.activeCall.Participants {
		if cp.Status == "active" {
			activeCount++
		}
	}
	if from == s.activeCall.CreatorID || activeCount <= 1 {
		s.endCallInternal("ended")
	}

	s.log.Info("участник вышел из звонка",
		slog.String("peer", safeShort(from)),
		slog.String("callId", safeShort8(p.CallID)))
}

// HandlePeerOffline обрабатывает отключение пира во время активного звонка.
// Запускает grace period (2 минуты) — если пир вернётся, звонок продолжится.
func (s *CallsService) HandlePeerOffline(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return
	}

	cp, ok := s.activeCall.Participants[peerID]
	if !ok || (cp.Status != "active" && cp.Status != "ringing") {
		return
	}

	// Помечаем как "reconnecting" — даём 2 минуты на возврат
	cp.Status = "reconnecting"

	s.network.EmitEvent("vox:call-participant-reconnecting", map[string]any{
		"peerId": peerID, "callId": s.activeCall.ID,
	})

	s.log.Info("📞 участник отключился, ожидание reconnect (2 мин)",
		slog.String("peer", safeShort(peerID)),
		slog.String("callId", safeShort8(s.activeCall.ID)))

	// Отменяем предыдущий таймер если был
	if t, ok := s.reconnTimers[peerID]; ok {
		t.Stop()
	}

	// Запускаем grace period таймер
	callID := s.activeCall.ID
	s.reconnTimers[peerID] = time.AfterFunc(2*time.Minute, func() {
		s.onReconnectTimeout(peerID, callID)
	})
}

// onReconnectTimeout — пир не вернулся за 2 минуты, завершаем участие
func (s *CallsService) onReconnectTimeout(peerID, callID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.reconnTimers, peerID)

	if s.activeCall == nil || s.activeCall.ID != callID {
		return
	}

	cp, ok := s.activeCall.Participants[peerID]
	if !ok || cp.Status != "reconnecting" {
		return
	}

	cp.Status = "left"
	now := time.Now()
	s.store.DB().Exec("UPDATE call_participants SET status = 'left', left_at = ? WHERE call_id = ? AND peer_id = ?",
		now.Unix(), callID, peerID)

	s.network.EmitEvent("vox:call-participant-left", map[string]any{
		"peerId": peerID, "callId": callID,
	})

	s.log.Info("📞 участник не вернулся (таймаут 2 мин)",
		slog.String("peer", safeShort(peerID)),
		slog.String("callId", safeShort8(callID)))

	// Проверяем, остался ли кто-то в звонке
	activeCount := 0
	for _, p := range s.activeCall.Participants {
		if p.Status == "active" || p.Status == "reconnecting" {
			activeCount++
		}
	}
	if activeCount <= 1 {
		s.endCallInternal("ended")
	}
}

// HandlePeerOnline обрабатывает возвращение пира в онлайн во время звонка.
// Отменяет grace period таймер и повторно приглашает в звонок.
func (s *CallsService) HandlePeerOnline(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return
	}

	cp, ok := s.activeCall.Participants[peerID]
	if !ok || cp.Status != "reconnecting" {
		return
	}

	// Отменяем grace period таймер
	if t, ok := s.reconnTimers[peerID]; ok {
		t.Stop()
		delete(s.reconnTimers, peerID)
	}

	s.log.Info("📞 участник вернулся, повторное приглашение",
		slog.String("peer", safeShort(peerID)),
		slog.String("callId", safeShort8(s.activeCall.ID)))

	// Ставим ringing и отправляем повторный call_invite
	cp.Status = "ringing"
	s.activeCall.Ringing[peerID] = true

	client := s.network.Client()
	if client != nil {
		nickTag, _ := s.network.getNickTag()
		var participants []string
		for pid := range s.activeCall.Participants {
			participants = append(participants, pid)
		}
		client.SendCallInvite(peerID, s.activeCall.ID, nickTag, participants, s.activeCall.GroupID)
	}

	s.network.EmitEvent("vox:call-participant-reconnected", map[string]any{
		"peerId": peerID, "callId": s.activeCall.ID,
	})
}

// --- Internal ---

// dmCallSummary — итог DM-звонка для системного сообщения в переписке:
// пир, исходящий ли, исход и длительность. Отвечен = у не-создателя есть
// JoinedAt (создатель получает JoinedAt при старте). Вызывать под s.mu до
// обнуления activeCall.
func (s *CallsService) dmCallSummary() (peerID string, outbound bool, outcome string, dur time.Duration, ok bool) {
	c := s.activeCall
	if c == nil || c.Type != "dm" {
		return
	}
	myID, _ := s.sess.PeerID()
	outbound = c.CreatorID == myID
	var answeredAt time.Time
	for pid, cp := range c.Participants {
		if pid != myID {
			peerID = pid
		}
		if pid != c.CreatorID && cp != nil && !cp.JoinedAt.IsZero() {
			answeredAt = cp.JoinedAt
		}
	}
	if peerID == "" {
		return
	}
	if !answeredAt.IsZero() {
		return peerID, outbound, "answered", time.Since(answeredAt), true
	}
	if outbound {
		return peerID, outbound, "no_answer", 0, true
	}
	return peerID, outbound, "missed", 0, true
}

// recordCallMessage — системное сообщение о звонке в DM-переписке (type
// 'call'; content "answered:<сек>" | "missed" | "no_answer" | "declined").
// Пишется на каждой стороне независимо, по сети не ходит. Фронт рисует
// system-bubble (callfmt.js parseCallContent) и превью в списке чатов.
func (s *CallsService) recordCallMessage(peerID string, outbound bool, outcome string, dur time.Duration) {
	if peerID == "" || s.store == nil {
		return
	}
	myID, _ := s.sess.PeerID()
	senderID := myID
	if !outbound {
		senderID = peerID
	}
	content := outcome
	if outcome == "answered" {
		content = fmt.Sprintf("answered:%d", int(dur.Seconds()))
	}
	id := uuid.New().String()
	now := time.Now()
	if _, err := s.store.DB().Exec(
		`INSERT INTO messages (id, peer_id, sender_id, content, type, read, delivered, created_at)
		 VALUES (?, ?, ?, ?, 'call', ?, 1, ?)`,
		id, peerID, senderID, content, callRecordRead(outcome, outbound), now.Unix()); err != nil {
		// Не-друг (call-only пир без записи в contacts) — FK не пустит; не критично.
		s.log.Debug("call message: insert skipped",
			slog.String("peer", safeShort(peerID)),
			slog.String("error", err.Error()))
		return
	}
	s.network.EmitEvent("vox:message", map[string]any{
		"id":        id,
		"peerId":    peerID,
		"senderId":  senderID,
		"content":   content,
		"msgType":   "call",
		"timestamp": now.UnixMilli(),
		"outbound":  outbound,
		"delivered": true,
	})
}

func (s *CallsService) leaveCallInternal() {
	if s.activeCall == nil {
		return
	}
	// Статус и ended_at в calls раньше писал только endCallInternal (по
	// удалённому call_end): у того, кто сам нажал «завершить», запись в
	// истории оставалась 'ringing'/'active' без длительности, хотя пузырь в
	// чате показывал 1:13 (QA 2026-09-14). ended_at ставим только если звонок
	// был отвечен — иначе длительностью стало бы время дозвона.
	callStatus, answered := "ended", true
	if pid, ob, oc, d, ok := s.dmCallSummary(); ok {
		s.recordCallMessage(pid, ob, oc, d)
		switch oc {
		case "no_answer":
			callStatus, answered = "cancelled", false
		case "missed":
			callStatus, answered = "missed", false
		}
	}

	myID, _ := s.sess.PeerID()
	callID := s.activeCall.ID

	// Уведомляем участников
	client := s.network.Client()
	for pid, cp := range s.activeCall.Participants {
		if pid != myID && (cp.Status == "active" || cp.Status == "ringing") {
			if client != nil {
				client.SendCallEnd(pid, callID)
			}
		}
	}

	// Выходим из голоса
	s.voice.LeaveGroupVoice()

	// Обновляем БД
	now := time.Now()
	s.store.DB().Exec("UPDATE call_participants SET status = 'left', left_at = ? WHERE call_id = ? AND peer_id = ?",
		now.Unix(), callID, myID)
	if answered {
		s.store.DB().Exec("UPDATE calls SET status = ?, ended_at = ? WHERE id = ?",
			callStatus, now.Unix(), callID)
	} else {
		s.store.DB().Exec("UPDATE calls SET status = ? WHERE id = ?", callStatus, callID)
	}

	// Закрываем call-only P2P
	pm := s.network.PeerMgr()
	if pm != nil {
		pm.CloseCallOnlyPeers()
	}

	s.stopRingTimerLocked()
	s.stopReconnTimersLocked()
	s.activeCall = nil

	s.network.EmitEvent("vox:call-ended", map[string]any{"callId": callID})
	go s.closeIncomingCallPopup()
}

func (s *CallsService) endCallInternal(status string) {
	if s.activeCall == nil {
		return
	}
	if pid, ob, oc, d, ok := s.dmCallSummary(); ok {
		s.recordCallMessage(pid, ob, oc, d)
	}

	callID := s.activeCall.ID
	now := time.Now()

	s.store.DB().Exec("UPDATE calls SET status = ?, ended_at = ? WHERE id = ?",
		status, now.Unix(), callID)
	s.store.DB().Exec(
		"UPDATE call_participants SET status = 'left', left_at = ? WHERE call_id = ? AND status IN ('active', 'ringing')",
		now.Unix(), callID)

	// Выходим из голоса
	s.voice.LeaveGroupVoice()

	pm := s.network.PeerMgr()
	if pm != nil {
		pm.CloseCallOnlyPeers()
	}

	s.stopRingTimerLocked()
	s.stopReconnTimersLocked()
	s.activeCall = nil

	s.network.EmitEvent("vox:call-ended", map[string]any{"callId": callID})
	go s.closeIncomingCallPopup()
}

// createCallGroup создаёт новую группу для текущего звонка с фрешим uuid.
// Используется creator'ом звонка.
func (s *CallsService) createCallGroup() {
	if s.activeCall == nil {
		return
	}
	groupID := uuid.New().String()
	s.installCallGroup(groupID, false /* mustBeFresh */)
	s.log.Info("группа звонка создана",
		slog.String("groupId", safeShort8(groupID)),
		slog.String("callId", safeShort8(s.activeCall.ID)))
}

// adoptCallGroup использует groupId, полученный от другого участника звонка.
// Группа в БД может уже существовать (если другой участник её создал
// и broadcast'нул), поэтому INSERT OR IGNORE.
func (s *CallsService) adoptCallGroup(groupID string, fromPeerID string) {
	if s.activeCall == nil {
		return
	}
	s.installCallGroup(groupID, true /* mustBeFresh */)
	s.log.Info("группа звонка принята от пира",
		slog.String("groupId", safeShort8(groupID)),
		slog.String("from", safeShort(fromPeerID)),
		slog.String("callId", safeShort8(s.activeCall.ID)))
}

// installCallGroup — общая для create/adopt механика установки группы:
//  1. INSERT в groups (без OR IGNORE если mustBeFresh=false — fresh uuid,
//     с OR IGNORE если mustBeFresh=true — приняли извне, может существовать)
//  2. INSERT OR IGNORE для каждого active участника как member (creator → admin)
//  3. UPDATE calls.group_id
//  4. Запуск voice через preset+JoinGroupVoice
//
// activeCall != nil гарантировано caller'ом.
func (s *CallsService) installCallGroup(groupID string, alreadyMayExist bool) {
	now := time.Now().Unix()

	insertGroup := "INSERT INTO groups (group_id, name, owner_id, type, created_at) VALUES (?, ?, ?, 2, ?)"
	if alreadyMayExist {
		insertGroup = "INSERT OR IGNORE INTO groups (group_id, name, owner_id, type, created_at) VALUES (?, ?, ?, 2, ?)"
	}
	s.store.DB().Exec(insertGroup, groupID, "Call", s.activeCall.CreatorID, now)

	for pid, cp := range s.activeCall.Participants {
		if cp.Status != "active" {
			continue
		}
		role := "member"
		if pid == s.activeCall.CreatorID {
			role = "admin"
		}
		s.store.DB().Exec(
			"INSERT OR IGNORE INTO group_members (group_id, peer_id, role, joined_at, status) VALUES (?, ?, ?, ?, 'active')",
			groupID, pid, role, now)
	}

	s.activeCall.GroupID = groupID
	s.store.DB().Exec("UPDATE calls SET group_id = ? WHERE id = ?", groupID, s.activeCall.ID)
	s.network.RefreshPresence() // участники группы звонка видят мой статус «в сети»

	// Запускаем голос через группу (preset чтобы voice re-announce работал до завершения init)
	s.voice.PresetActiveGroup(groupID)
	go s.voice.JoinGroupVoice(groupID)
}

// --- Ring timer ---

func (s *CallsService) startRingTimer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startRingTimerLocked()
}

func (s *CallsService) startRingTimerLocked() {
	s.stopRingTimerLocked()
	s.ringTimer = time.AfterFunc(60*time.Second, s.onRingTimeout)
}

func (s *CallsService) stopRingTimerLocked() {
	if s.ringTimer != nil {
		s.ringTimer.Stop()
		s.ringTimer = nil
	}
}

func (s *CallsService) stopReconnTimersLocked() {
	for pid, t := range s.reconnTimers {
		t.Stop()
		delete(s.reconnTimers, pid)
	}
}

func (s *CallsService) onRingTimeout() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil {
		return
	}

	client := s.network.Client()

	// Отправляем call_end всем ещё звенящим
	for pid := range s.activeCall.Ringing {
		if client != nil {
			client.SendCallEnd(pid, s.activeCall.ID)
		}
		if cp, ok := s.activeCall.Participants[pid]; ok {
			cp.Status = "missed"
		}
		s.store.DB().Exec("UPDATE call_participants SET status = 'missed' WHERE call_id = ? AND peer_id = ?",
			s.activeCall.ID, pid)
	}
	s.activeCall.Ringing = make(map[string]bool)

	// Если никто не принял — завершаем как missed
	callID := s.activeCall.ID
	activeCount := 0
	for _, cp := range s.activeCall.Participants {
		if cp.Status == "active" {
			activeCount++
		}
	}
	if activeCount <= 1 {
		s.endCallInternal("missed") // обнуляет s.activeCall
	}

	s.log.Info("таймаут рингтона",
		slog.String("callId", safeShort8(callID)),
		slog.Int("active", activeCount))
}

// RetryCallInvite переотправляет call_invite пиру, который только что вышел в онлайн.
// Вызывается из обработчика peer_online в NetworkService.
func (s *CallsService) RetryCallInvite(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeCall == nil || !s.activeCall.Ringing[peerID] {
		return
	}

	client := s.network.Client()
	if client == nil {
		return
	}

	nickTag, _ := s.network.getNickTag()
	var participants []string
	for pid := range s.activeCall.Participants {
		participants = append(participants, pid)
	}

	if err := client.SendCallInvite(peerID, s.activeCall.ID, nickTag, participants, s.activeCall.GroupID); err != nil {
		s.log.Warn("retry call_invite не удался",
			slog.String("to", safeShort(peerID)),
			slog.String("error", err.Error()))
	} else {
		s.log.Info("📞 RETRY call_invite",
			slog.String("to", safeShort(peerID)),
			slog.String("callId", safeShort8(s.activeCall.ID)))
	}
}

// --- Helpers ---

func (s *CallsService) ensureP2P(peerID string) {
	isFriend := s.isFriend(peerID)
	if isFriend {
		if !s.network.IsP2PConnected(peerID) {
			s.log.Debug("📞 P2P для звонка (друг, не подключён)",
				slog.String("peer", safeShort(peerID)))
			s.network.ConnectToPeer(peerID)
		}
	} else {
		s.log.Debug("📞 P2P для звонка (call-only)",
			slog.String("peer", safeShort(peerID)))
		s.network.ConnectToCallPeer(peerID)
	}
}

func (s *CallsService) ensureP2PLocked(peerID string) {
	// Вызывается под s.mu.Lock — не должен блокировать
	go s.ensureP2P(peerID)
}

func (s *CallsService) isFriend(peerID string) bool {
	var count int
	s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'",
		peerID).Scan(&count)
	return count > 0
}
