package services

// Групповой relay кластера (docs/cluster-contract.md §7): курсоры group_sync
// по журналам узлов и повтор своих сообщений, которые сервер ещё не
// подтвердил. Подтверждение — своя же копия group_message_new с seq.

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"vox_app/internal/network"
)

const (
	// relayMaxBodyBytes — предел тела сообщения на сервере (GROUP_MAX_MSG_BYTES).
	relayMaxBodyBytes = 32 * 1024
	// relayMaxFrameBytes — предел кадра: сервер читает не больше 64 КБ и
	// больший кадр рвёт вместе с соединением, без ошибки. С запасом.
	relayMaxFrameBytes = 60 * 1024
)

// ErrRelayTooLarge — сообщение не влезет в лимиты серверного relay.
var ErrRelayTooLarge = errors.New("сообщение слишком длинное для группы")

// relayFits — влезет ли сообщение в лимиты сервера: тело и готовый кадр.
// Не влезающее отправлять нельзя совсем: отказ по телу сервер присылает, а
// кадр больше предела рвёт соединение, и повтор после переподключения рвал бы
// его снова, бесконечно.
func relayFits(groupID, msgID, payload string) bool {
	if len(payload) > relayMaxBodyBytes {
		return false
	}
	return network.GroupSendFrameSize(groupID, msgID, payload, true, time.Now().UnixMilli()) <= relayMaxFrameBytes
}

const (
	// relayRetryDelay — через сколько повторить неподтверждённое сообщение.
	relayRetryDelay = 10 * time.Second
	// relayMaxRetries — повторов по таймеру; дальше — только после
	// переподключения (resendPendingRelay).
	relayMaxRetries = 5
	// relayPendingTTL — сервер хранит сообщения 7 дней, старше повторять
	// бессмысленно.
	relayPendingTTL = 7 * 24 * time.Hour
)

// relayCursors — курсоры журналов группы {origin_node: seq} для group_sync.
func (s *GroupsService) relayCursors(groupID string) map[string]int64 {
	out := map[string]int64{}
	rows, err := s.store.DB().Query(
		"SELECT origin_node, seq FROM group_relay_cursors WHERE group_id = ?", groupID)
	if err != nil {
		s.log.Warn("relay cursors: query", slog.String("error", err.Error()))
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		var seq int64
		if err := rows.Scan(&origin, &seq); err == nil {
			out[origin] = seq
		}
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("relay cursors: rows", slog.String("error", err.Error()))
	}
	return out
}

// Курсор журнала — граница непрерывно полученного префикса, а не максимум
// увиденного: с него group_sync продолжает догон, а сервер отдаёт строго
// seq > курсора. Прыжок вперёд по живому сообщению навсегда терял бы всё, что
// осталось ниже, поэтому двигают курсор две разные функции.

// bumpRelayCursorMax — продвижение по ответу сервера. group_sync отдаёт из
// каждого журнала непрерывный префикс хвоста, поэтому максимум страницы и есть
// новая граница. Безусловно: сообщения ниже могли быть удалены по retention, и
// требование непрерывности заклинило бы курсор навсегда.
func (s *GroupsService) bumpRelayCursorMax(groupID, origin string, seq int64) {
	if seq <= 0 {
		return
	}
	if _, err := s.store.DB().Exec(`
		INSERT INTO group_relay_cursors (group_id, origin_node, seq) VALUES (?, ?, ?)
		ON CONFLICT(group_id, origin_node) DO UPDATE SET seq = MAX(seq, excluded.seq)`,
		groupID, origin, seq); err != nil {
		s.log.Warn("relay cursor update failed", slog.String("error", err.Error()))
	}
}

// bumpRelayCursorNext — продвижение по живому сообщению: только если оно
// следующее по журналу. Разрыв (эхо своих неподтверждённых, оборванный
// fan-out) оставляет курсор на месте, и пропуск добирает ближайший group_sync;
// дубли отсекаются по id сообщения.
func (s *GroupsService) bumpRelayCursorNext(groupID, origin string, seq int64) {
	if seq <= 0 {
		return
	}
	db := s.store.DB()
	// Строка курсора должна существовать, иначе двигать нечего: новый журнал
	// начинается с 0 и первым непрерывным будет seq = 1.
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO group_relay_cursors (group_id, origin_node, seq) VALUES (?, ?, 0)`,
		groupID, origin); err != nil {
		s.log.Warn("relay cursor init failed", slog.String("error", err.Error()))
		return
	}
	if _, err := db.Exec(`
		UPDATE group_relay_cursors SET seq = ?
		WHERE group_id = ? AND origin_node = ? AND seq = ? - 1`,
		seq, groupID, origin, seq); err != nil {
		s.log.Warn("relay cursor update failed", slog.String("error", err.Error()))
	}
}

// addPendingRelay — своё сообщение ждёт подтверждения сервера.
func (s *GroupsService) addPendingRelay(groupID, msgID string) error {
	_, err := s.store.DB().Exec(
		"INSERT OR REPLACE INTO group_relay_pending (msg_id, group_id, created_at) VALUES (?, ?, ?)",
		msgID, groupID, time.Now().Unix())
	return err
}

// ackRelay — сообщение подтверждено (или отправка отменена): повтор не нужен.
func (s *GroupsService) ackRelay(msgID string) {
	if _, err := s.store.DB().Exec("DELETE FROM group_relay_pending WHERE msg_id = ?", msgID); err != nil {
		s.log.Warn("pending relay delete failed", slog.String("error", err.Error()))
	}
	s.stopRelayRetry(msgID)
}

func (s *GroupsService) scheduleRelayRetry(groupID, msgID string, attempt int) {
	s.relayRetryMu.Lock()
	defer s.relayRetryMu.Unlock()
	if s.relayRetry == nil {
		s.relayRetry = make(map[string]*time.Timer)
	}
	if t := s.relayRetry[msgID]; t != nil {
		t.Stop()
	}
	s.relayRetry[msgID] = time.AfterFunc(relayRetryDelay, func() { s.retryRelay(groupID, msgID, attempt) })
}

func (s *GroupsService) stopRelayRetry(msgID string) {
	s.relayRetryMu.Lock()
	defer s.relayRetryMu.Unlock()
	if t := s.relayRetry[msgID]; t != nil {
		t.Stop()
		delete(s.relayRetry, msgID)
	}
}

// retryRelay — повтор неподтверждённого сообщения с тем же client_msg_id:
// сервер отдаст уже сохранённую копию или сохранит (если прежний узел упал).
func (s *GroupsService) retryRelay(groupID, msgID string, attempt int) {
	s.relayRetryMu.Lock()
	delete(s.relayRetry, msgID)
	s.relayRetryMu.Unlock()

	var one int
	if err := s.store.DB().QueryRow(
		"SELECT 1 FROM group_relay_pending WHERE msg_id = ?", msgID).Scan(&one); err != nil {
		return // уже подтверждено
	}
	var content, replyTo, fwd string
	var createdAt int64
	var deleted int
	if err := s.store.DB().QueryRow(`
		SELECT content, COALESCE(reply_to, ''), COALESCE(forwarded_from, ''), created_at, deleted
		FROM group_messages WHERE id = ?`, msgID).Scan(&content, &replyTo, &fwd, &createdAt, &deleted); err != nil {
		s.ackRelay(msgID) // сообщение удалено — повторять нечего
		return
	}
	// Удаление мягкое и очередь не трогает. Если сервер оригинал не сохранил,
	// повтор опубликовал бы всей группе уже удалённое сообщение.
	if deleted != 0 {
		s.ackRelay(msgID)
		return
	}
	opts := SendOptions{ReplyTo: decodeReply(replyTo), ForwardedFrom: decodeForward(fwd)}
	// Шифруем заново для текущих участников; сервер дедуплицирует по msg_id.
	payload, err := s.relayPayload(groupID, msgID, content, opts)
	if err != nil {
		s.log.Warn("повтор: шифрование не удалось", slog.String("msg_id", msgID), slog.String("error", err.Error()))
		return
	}
	// Строка могла попасть в очередь до проверки размера (клиент 0078,
	// правка сообщения): такой кадр сервер рвёт вместе с соединением, и
	// повтор после каждого переподключения рвал бы его снова.
	if !relayFits(groupID, msgID, payload) {
		s.failRelay(groupID, msgID, relayErrTooLarge)
		return
	}
	client := s.network.Client()
	if client == nil {
		return // повторит resendPendingRelay после переподключения
	}
	if err := client.SendGroupMessageRelay(groupID, msgID, payload, true, createdAt*1000); err != nil {
		s.log.Warn("повтор отправки в группу не удался",
			slog.String("msg_id", msgID), slog.String("error", err.Error()))
	} else {
		s.log.Info("повтор отправки в группу",
			slog.String("group_id", safeShort8(groupID)),
			slog.String("msg_id", msgID),
			slog.Int("attempt", attempt+1))
	}
	if attempt+1 < relayMaxRetries {
		s.scheduleRelayRetry(groupID, msgID, attempt+1)
	}
}

// resendPendingRelay — после переподключения повторяет все свои
// неподтверждённые сообщения; старше relayPendingTTL — забывает.
func (s *GroupsService) resendPendingRelay() {
	cutoff := time.Now().Add(-relayPendingTTL).Unix()
	if _, err := s.store.DB().Exec("DELETE FROM group_relay_pending WHERE created_at < ?", cutoff); err != nil {
		s.log.Warn("pending relay cleanup failed", slog.String("error", err.Error()))
	}
	rows, err := s.store.DB().Query("SELECT msg_id, group_id FROM group_relay_pending")
	if err != nil {
		s.log.Warn("pending relay query failed", slog.String("error", err.Error()))
		return
	}
	type pending struct{ msgID, groupID string }
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.msgID, &p.groupID); err == nil {
			list = append(list, p)
		}
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("pending relay rows", slog.String("error", err.Error()))
	}
	rows.Close()
	if len(list) > 0 {
		s.log.Info("повтор неподтверждённых сообщений групп", slog.Int("count", len(list)))
	}
	for _, p := range list {
		s.stopRelayRetry(p.msgID)
		s.retryRelay(p.groupID, p.msgID, 0)
	}
}

// Коды отказа group_message_send (server/signal/pkg/protocol).
const (
	relayErrTooLarge  = "message_too_large"
	relayErrNotMember = "not_group_member"
	relayErrNoGroup   = "group_not_found"
)

// relayRejectFinal — отказ, после которого автоматический повтор не нужен.
// message_too_large — навсегда. not_group_member и group_not_found тоже
// снимают сообщение с повтора, но повтор руками разрешён: состав доезжает до
// второго узла асинхронно, и в окне репликации отказ временный. Остальное
// (rate_limited, invalid_message) — временное, повтор продолжается.
func relayRejectFinal(code string) bool {
	switch code {
	case relayErrTooLarge, relayErrNotMember, relayErrNoGroup:
		return true
	}
	return false
}

// HandleRelaySendError — сервер отклонил своё сообщение в группу.
func (s *GroupsService) HandleRelaySendError(groupID, msgID, code string) {
	if msgID == "" {
		return
	}
	if !relayRejectFinal(code) {
		s.log.Info("временный отказ отправки в группу, повтор продолжится",
			slog.String("msg_id", msgID), slog.String("code", code))
		return
	}
	// Только если сообщение ещё ждёт подтверждения: запоздавший отказ не
	// должен пометить уже принятое.
	var one int
	if err := s.store.DB().QueryRow(
		"SELECT 1 FROM group_relay_pending WHERE msg_id = ?", msgID).Scan(&one); err != nil {
		return
	}
	s.failRelay(groupID, msgID, code)
}

// failRelay — снять сообщение с повтора и пометить неотправленным.
func (s *GroupsService) failRelay(groupID, msgID, code string) {
	s.ackRelay(msgID)
	if _, err := s.store.DB().Exec(
		"UPDATE group_messages SET send_failed = ? WHERE id = ? AND seq = 0", code, msgID); err != nil {
		s.log.Warn("send_failed update failed", slog.String("error", err.Error()))
	}
	s.log.Warn("сообщение в группу не отправлено",
		slog.String("group_id", safeShort8(groupID)),
		slog.String("msg_id", msgID),
		slog.String("code", code))
	s.network.emitEvent("vox:group-message-failed", map[string]any{
		"id":      msgID,
		"groupId": groupID,
		"code":    code,
	})
}

// RetryGroupRelayMessage — повтор руками неотправленного сообщения, с тем же
// id: отклонённое сервер не сохранял, ключ дедупа свободен.
func (s *GroupsService) RetryGroupRelayMessage(msgID string) error {
	const op = "RetryGroupRelayMessage"
	myID, err := s.sess.PeerID()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	var groupID, senderID, content, replyTo, fwd, failed string
	var deleted int
	var seq int64
	if err := s.store.DB().QueryRow(`
		SELECT group_id, sender_id, content, COALESCE(reply_to, ''), COALESCE(forwarded_from, ''),
		       deleted, seq, send_failed
		FROM group_messages WHERE id = ?`, msgID).Scan(
		&groupID, &senderID, &content, &replyTo, &fwd, &deleted, &seq, &failed); err != nil {
		return fmt.Errorf("%s: сообщение не найдено: %w", op, err)
	}
	if senderID != myID || deleted != 0 || seq != 0 || failed == "" {
		return fmt.Errorf("%s: повторять нечего", op)
	}
	opts := SendOptions{ReplyTo: decodeReply(replyTo), ForwardedFrom: decodeForward(fwd)}
	if sealed, err := s.relayPayload(groupID, msgID, content, opts); err != nil || !relayFits(groupID, msgID, sealed) {
		return ErrRelayTooLarge
	}
	if _, err := s.store.DB().Exec("UPDATE group_messages SET send_failed = '' WHERE id = ?", msgID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := s.addPendingRelay(groupID, msgID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	s.network.emitEvent("vox:group-message-failed", map[string]any{
		"id":      msgID,
		"groupId": groupID,
		"code":    "",
	})
	s.retryRelay(groupID, msgID, 0)
	return nil
}
