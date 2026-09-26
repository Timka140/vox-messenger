package services

import (
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Статус «в сети» только для друзей и участников общих групп (решение Тима
// 2026-09-15). Сигнальный сервер шлёт peer_online / peer_offline и отвечает
// peer_offline_err только тем, кто в watch-списке клиента. Список уходит в
// auth при каждом подключении (presenceForAuth) и обновляется
// presence_watch, когда меняются друзья, группы или блокировки
// (RefreshPresence, в том числе из emitEvent по presenceEvents).

// presenceEvents — события, после которых состав watch-списка мог измениться.
var presenceEvents = map[string]bool{
	"vox:friend-accepted":     true,
	"vox:friend-removed":      true,
	"vox:group-invite":        true,
	"vox:group-member-added":  true,
	"vox:group-member-left":   true,
	"vox:group-member-joined": true,
	"vox:group-kicked":        true,
	"vox:group-deleted":       true,
	"vox:peer-blocked":        true,
	"vox:peer-unblocked":      true,
}

// presenceRefreshDelay — пачка изменений (создание группы с участниками,
// импорт) даёт одну отправку.
const presenceRefreshDelay = time.Second

// presenceWatch — кому виден мой статус: друзья и активные участники групп,
// где я состою, кроме заблокированных и себя. Отсортирован, чтобы сравнивать
// с последним отправленным.
func (s *NetworkService) presenceWatch() []string {
	myID, _ := s.sess.PeerID()
	rows, err := s.store.DB().Query(`
		SELECT peer_id FROM contacts WHERE status = 'accepted'
		UNION
		SELECT gm.peer_id FROM group_members gm
		WHERE gm.status = 'active' AND gm.group_id IN (
			SELECT group_id FROM group_members WHERE peer_id = ? AND status = 'active')`, myID)
	if err != nil {
		s.log.Warn("presence: список не собран", slog.String("error", err.Error()))
		return []string{}
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" && id != myID {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !s.IsBlocked(id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// presenceForAuth — список для auth (SignalClient.Connect). Запоминается как
// отправленный: RefreshPresence не повторит его сразу после входа.
func (s *NetworkService) presenceForAuth() []string {
	w := s.presenceWatch()
	s.presMu.Lock()
	s.presSent = strings.Join(w, ",")
	s.presMu.Unlock()
	s.log.Info("presence: список в auth", slog.Int("watch", len(w)))
	return w
}

// RefreshPresence — состав друзей/групп/блокировок изменился: через
// presenceRefreshDelay пересчитать список и, если он другой, отправить
// серверу presence_watch.
func (s *NetworkService) RefreshPresence() {
	s.presMu.Lock()
	defer s.presMu.Unlock()
	if s.presTimer != nil {
		s.presTimer.Stop()
	}
	s.presTimer = time.AfterFunc(presenceRefreshDelay, s.sendPresence)
	s.log.Debug("presence: пересчёт запланирован")
}

func (s *NetworkService) sendPresence() {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()
	// Нет связи — список уйдёт в auth при переподключении. Старый сервер
	// presence_watch не понимает.
	switch {
	case client == nil:
		s.log.Debug("presence: пропуск — нет клиента сигналки")
		return
	case !client.IsConnected():
		s.log.Debug("presence: пропуск — сигналка не подключена")
		return
	case !client.SupportsPresence():
		s.log.Debug("presence: пропуск — сервер без watch-списков")
		return
	}
	w := s.presenceWatch()
	key := strings.Join(w, ",")
	s.presMu.Lock()
	same := key == s.presSent
	if !same {
		s.presSent = key
	}
	s.presMu.Unlock()
	if same {
		s.log.Debug("presence: список не изменился", slog.Int("watch", len(w)))
		return
	}
	if err := client.SendPresenceWatch(w); err != nil {
		s.presMu.Lock()
		s.presSent = ""
		s.presMu.Unlock()
		s.log.Warn("presence: список не отправлен", slog.String("error", err.Error()))
		return
	}
	s.log.Info("presence: список обновлён", slog.Int("watch", len(w)))
}
