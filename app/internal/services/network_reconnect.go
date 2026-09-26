package services

import (
	"errors"
	"log/slog"
	"time"
	"vox_app/internal/network"
)

// startReconnectLoop запускает фоновый цикл переподключения к сигнальному серверу.
// Использует exponential backoff с потолком 60 секунд. Останавливается через
// reconnectStop канал (закрывается из stopReconnectLoop).
func (s *NetworkService) startReconnectLoop() {
	s.mu.Lock()
	if s.reconnectStop != nil {
		s.mu.Unlock()
		return // уже запущен
	}
	s.reconnectStop = make(chan struct{})
	stop := s.reconnectStop
	s.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("reconnect loop panic", slog.Any("recover", r))
			}
			// Сбрасываем reconnectStop при ЛЮБОМ выходе (успех/паника/stop),
			// иначе следующий startReconnectLoop увидит non-nil канал и решит,
			// что цикл уже работает. Раньше сброс делал Connect() через
			// stopReconnectLoop, но ReconnectSignal его не вызывает — после
			// первого успешного авто-реконнекта каждый следующий обрыв
			// сигналки игнорировался, пока пользователь не переподключался
			// вручную.
			s.mu.Lock()
			if s.reconnectStop == stop {
				s.reconnectStop = nil
			}
			s.mu.Unlock()
		}()
		delays := []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
		attempt := 0
		for {
			idx := attempt
			if idx >= len(delays) {
				idx = len(delays) - 1
			}
			delay := delays[idx]

			select {
			case <-stop:
				return
			case <-time.After(delay):
			}

			s.log.Info("авто-переподключение к сигналке",
				slog.Int("attempt", attempt+1),
				slog.String("delay", delay.String()))

			// ReconnectSignal, а не Connect: сохраняет PeerManager и живые P2P.
			// Connect делает CloseAll() ДО дозвона — любой чих сигналки (45s
			// pongWait, рестарт сервера, Wi-Fi blip) рвал все рабочие voice/
			// files/screen соединения, хотя P2P от сигналки не зависит.
			// Если PeerManager ещё не создавался (первый Connect не случился) —
			// падаем на полный Connect.
			s.mu.RLock()
			hasPeerMgr := s.peerMgr != nil
			s.mu.RUnlock()

			var err error
			if hasPeerMgr {
				err = s.ReconnectSignal()
			} else {
				err = s.Connect("", "", "")
			}
			if err == nil {
				s.log.Info("авто-переподключение успешно")
				return
			}
			attempt++
		}
	}()
}

// stopReconnectLoop останавливает фоновый цикл переподключения
func (s *NetworkService) stopReconnectLoop() {
	s.mu.Lock()
	if s.reconnectStop != nil {
		close(s.reconnectStop)
		s.reconnectStop = nil
	}
	s.mu.Unlock()
}

// connectToOnlineFriends — после подключения к сигналке пробуем P2P с каждым другом
func (s *NetworkService) connectToOnlineFriends() {
	rows, err := s.store.DB().Query("SELECT peer_id, nickname FROM contacts WHERE status = 'accepted'")
	if err != nil {
		return
	}
	defer rows.Close()

	var friends []struct{ id, nick string }
	for rows.Next() {
		var peerID, nick string
		if err := rows.Scan(&peerID, &nick); err != nil {
			continue
		}
		friends = append(friends, struct{ id, nick string }{peerID, nick})
	}
	rows.Close()

	s.log.Info("🔗 ПОДКЛЮЧЕНИЕ К ДРУЗЬЯМ",
		slog.Int("total_friends", len(friends)))

	for _, f := range friends {
		s.log.Debug("  → пробую P2P",
			slog.String("peer", safeShort(f.id)))
		s.ConnectToPeer(f.id)
	}
}

// scheduleReconnect планирует попытку P2P-реконнекта к пиру с exponential
// backoff из p2pReconnectDelays. Отменяет предыдущий таймер если он был.
func (s *NetworkService) scheduleReconnect(pubKey string) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	if s.shuttingDown {
		return
	}

	// Отменяем предыдущий таймер
	if t, ok := s.reconnectTimers[pubKey]; ok {
		t.Stop()
		delete(s.reconnectTimers, pubKey)
	}

	attempt := s.reconnectAttempts[pubKey]
	var delay time.Duration
	if attempt >= len(p2pReconnectDelays) {
		// Быстрые попытки исчерпаны. Пока сигналка считает пира онлайн —
		// продолжаем медленно (attempt не растёт); иначе ждём peer_online.
		if !s.signalOnline[pubKey] {
			s.log.Info("P2P reconnect: достигнут лимит попыток, пир офлайн по сигналке",
				slog.String("peer", safeShort(pubKey)))
			delete(s.reconnectAttempts, pubKey)
			return
		}
		delay = p2pSlowRetryDelay
		s.log.Info("P2P reconnect: медленный повтор",
			slog.String("peer", safeShort(pubKey)),
			slog.Duration("delay", delay))
	} else {
		delay = p2pReconnectDelays[attempt]
		s.reconnectAttempts[pubKey] = attempt + 1
		s.log.Info("P2P reconnect: запланировано",
			slog.String("peer", safeShort(pubKey)),
			slog.Int("attempt", attempt+1),
			slog.Duration("delay", delay))
	}

	timer := time.AfterFunc(delay, func() {
		s.reconnectMu.Lock()
		delete(s.reconnectTimers, pubKey)
		s.reconnectMu.Unlock()

		// Проверяем: пир — друг и не подключён
		s.mu.RLock()
		pm := s.peerMgr
		s.mu.RUnlock()
		if pm == nil || pm.IsConnected(pubKey) {
			s.cancelReconnect(pubKey)
			return
		}
		var count int
		if err := s.store.DB().QueryRow(
			"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'",
			pubKey).Scan(&count); err != nil {
			s.log.Warn("P2P reconnect: ошибка проверки contacts",
				slog.String("peer", safeShort(pubKey)),
				slog.String("error", err.Error()))
			return
		}
		if count == 0 {
			s.cancelReconnect(pubKey)
			return
		}

		s.log.Info("P2P reconnect: попытка подключения", slog.String("peer", safeShort(pubKey)))
		if err := pm.InitiateConnection(pubKey); err != nil {
			// ErrSlotBusy — handshake уже идёт (или завис: watchdog в peermgr
			// сбросит слот через offerAnswerTimeout) — перепланируем, не Warn.
			if errors.Is(err, network.ErrSlotBusy) {
				s.log.Debug("P2P reconnect: слот занят, перепланируем",
					slog.String("peer", safeShort(pubKey)))
			} else {
				s.log.Warn("P2P reconnect: ошибка",
					slog.String("peer", safeShort(pubKey)),
					slog.String("error", err.Error()))
			}
			s.scheduleReconnect(pubKey)
		}
	})
	s.reconnectTimers[pubKey] = timer
}

// setSignalOnline обновляет представление о том, онлайн ли пир по сигналке
// (см. signalOnline в NetworkService).
func (s *NetworkService) setSignalOnline(pubKey string, online bool) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()
	if online {
		s.signalOnline[pubKey] = true
	} else {
		delete(s.signalOnline, pubKey)
	}
}

func (s *NetworkService) cancelReconnect(pubKey string) {
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()
	if t, ok := s.reconnectTimers[pubKey]; ok {
		t.Stop()
		delete(s.reconnectTimers, pubKey)
	}
	delete(s.reconnectAttempts, pubKey)
}
