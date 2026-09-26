package services

import (
	"encoding/json"
	"log/slog"
	"vox_app/internal/network"
)

// onSignalMessage — диспетчер всех входящих сообщений от signal-сервера.
// Роутит SDP/ICE в PeerManager, friend/call события в соответствующие сервисы,
// peer_online/offline события через EventBus, ошибки.
func (s *NetworkService) onSignalMessage(msg *network.Message) {
	// Заблокированный (blocklist.go): ни offer/ICE, ни заявок, ни звонков, ни
	// peer_online. peer_offline пропускаем — прибрать состояние.
	if msg.From != "" && msg.Type != network.TypePeerOffline && s.IsBlocked(msg.From) {
		return
	}
	switch msg.Type {
	case network.TypeOffer:
		s.mu.RLock()
		pm := s.peerMgr
		s.mu.RUnlock()
		if pm != nil {
			if err := pm.HandleOffer(msg.From, msg.Payload, msg.PcId); err != nil {
				s.log.Warn("ошибка обработки offer",
					slog.String("error", err.Error()),
					slog.Int("pcId", msg.PcId))
			}
		}

	case network.TypeAnswer:
		s.mu.RLock()
		pm := s.peerMgr
		s.mu.RUnlock()
		if pm != nil {
			if err := pm.HandleAnswer(msg.From, msg.Payload, msg.PcId); err != nil {
				s.log.Warn("ошибка обработки answer",
					slog.String("error", err.Error()),
					slog.Int("pcId", msg.PcId))
			}
		}

	case network.TypeICE:
		s.mu.RLock()
		pm := s.peerMgr
		s.mu.RUnlock()
		if pm != nil {
			if err := pm.HandleICE(msg.From, msg.Payload, msg.PcId); err != nil {
				s.log.Debug("ошибка обработки ICE",
					slog.String("error", err.Error()),
					slog.Int("pcId", msg.PcId))
			}
		}

	case network.TypeServerList:
		// Список узлов кластера — только от аутентифицированного соединения
		// (сообщения сюда приходят после mutual auth в SignalClient.Connect).
		var p network.ServerListPayload
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			s.log.Warn("невалидный server_list", slog.String("error", err.Error()))
			return
		}
		if err := s.mergeClusterServers(p.Nodes); err != nil {
			s.log.Warn("server_list не применён", slog.String("error", err.Error()))
		}

	case network.TypePeerResult:
		var result network.PeerResultPayload
		if err := json.Unmarshal(msg.Payload, &result); err != nil {
			s.log.Warn("невалидный peer_result", slog.String("error", err.Error()))
			return
		}
		s.emitEvent("vox:peer-search-result", result.Peers)

	case network.TypeFriendRequest:
		var payload network.FriendRequestPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.log.Warn("невалидный friend_request", slog.String("error", err.Error()))
			return
		}
		s.mu.RLock()
		friends := s.friends
		s.mu.RUnlock()
		if friends != nil {
			friends.HandleIncomingFriendRequest(msg.From, payload.NickTag)
		} else {
			s.log.Warn("⚠️ friend_request дропнут: FriendsService не установлен",
				slog.String("from", safeShort(msg.From)))
		}

	case network.TypeFriendAccept:
		var payload network.FriendRequestPayload
		if err := json.Unmarshal(msg.Payload, &payload); err != nil {
			s.log.Warn("невалидный friend_accept", slog.String("error", err.Error()))
			return
		}
		s.mu.RLock()
		friends := s.friends
		s.mu.RUnlock()
		if friends != nil {
			friends.HandleFriendAccepted(msg.From, payload.NickTag)
		}
		// Если был call-only P2P — снять флаг (теперь друзья)
		if pm := s.PeerMgr(); pm != nil {
			pm.ClearCallOnly(msg.From)
		}
		// Инициируем P2P соединение с новым другом
		s.ConnectToPeer(msg.From)

	case network.TypeFriendReject:
		s.mu.RLock()
		friends := s.friends
		s.mu.RUnlock()
		if friends != nil {
			friends.HandleFriendRejected(msg.From)
		}

	case network.TypeFriendRemove:
		s.mu.RLock()
		friends := s.friends
		s.mu.RUnlock()
		if friends != nil {
			friends.HandleFriendRemoved(msg.From)
		}

	case network.TypeCallInvite, network.TypeCallAccept, network.TypeCallReject, network.TypeCallEnd:
		s.log.Debug("📞 call signal",
			slog.String("type", msg.Type),
			slog.String("from", safeShort(msg.From)))
		s.mu.RLock()
		callFn := s.onCallSignal
		s.mu.RUnlock()
		if callFn != nil {
			callFn(msg.From, msg.Type, msg.Payload)
		} else {
			s.log.Warn("⚠️ call signal дропнут: CallsService не установлен",
				slog.String("type", msg.Type),
				slog.String("from", safeShort(msg.From)))
		}

	case network.TypePeerOnline:
		s.log.Info("🌐 СИГНАЛ: ПИР ОНЛАЙН",
			slog.String("peer", safeShort(msg.From)))
		s.setSignalOnline(msg.From, true)
		s.emitEvent("vox:peer-connected", map[string]string{"id": msg.From})
		// Если это друг — инициируем P2P
		var count int
		s.store.DB().QueryRow(
			"SELECT COUNT(*) FROM contacts WHERE peer_id = ? AND status = 'accepted'",
			msg.From).Scan(&count)
		if count > 0 {
			s.log.Info("🔗 ИНИЦИИРУЮ P2P (друг зашёл)",
				slog.String("peer", safeShort(msg.From)))
			s.cancelReconnect(msg.From)
			// Очищаем stale PC (если есть) перед новой попыткой.
			// s.peerMgr читаем под локом — оно подменяется в Connect.
			s.mu.RLock()
			pm := s.peerMgr
			s.mu.RUnlock()
			if pm != nil {
				pm.CloseIfNotConnected(msg.From)
			}
			s.ConnectToPeer(msg.From)
		}
		// Fan-out через EventBus: CallsService делает retry/reconnect call_invite,
		// GroupsService — auto-connect участников общих групп.
		s.emitBus(TopicPeerOnline, msg.From)

	case network.TypePeerOffline:
		s.log.Info("🌐 СИГНАЛ: ПИР ОФЛАЙН",
			slog.String("peer", safeShort(msg.From)))
		s.setSignalOnline(msg.From, false)
		s.mu.RLock()
		pm := s.peerMgr
		s.mu.RUnlock()
		if pm != nil {
			pm.ClosePeer(msg.From)
		}
		s.emitEvent("vox:peer-disconnected", map[string]string{"id": msg.From})
		// Очищаем voice участника — ClosePeer удаляет из peers map до PC.Close(),
		// поэтому OnConnectionStateChange проглатывает событие как "stale PC"
		if s.onVoicePeerDisconnected != nil {
			s.onVoicePeerDisconnected(msg.From)
		}
		// Fan-out через EventBus: CallsService завершит звонок если пир был участником.
		s.emitBus(TopicPeerOffline, msg.From)
		s.cancelReconnect(msg.From)

	case network.TypeGroupRegisterOK,
		network.TypeGroupMessageNew,
		network.TypeGroupSyncResult,
		network.TypeGroupDeliveryMode,
		network.TypeGroupSyncHint:
		s.mu.RLock()
		groups := s.groups
		s.mu.RUnlock()
		if groups == nil {
			s.log.Warn("⚠️ group signal дропнут: GroupsService не установлен",
				slog.String("type", msg.Type))
			return
		}
		groups.HandleSignalMessage(msg)

	case network.TypeError:
		var errPayload network.ErrorPayload
		json.Unmarshal(msg.Payload, &errPayload)
		if errPayload.Code == "peer_offline_err" {
			s.log.Debug("пир не в сети",
				slog.String("message", errPayload.Message),
				slog.String("peer", safeShort(errPayload.PeerID)))
			if errPayload.PeerID != "" {
				s.setSignalOnline(errPayload.PeerID, false)
			}
			// Очищаем stale PeerConnection для офлайн-пира
			if errPayload.PeerID != "" && s.peerMgr != nil {
				s.peerMgr.CloseIfNotConnected(errPayload.PeerID)
			}
		} else {
			s.log.Warn("ошибка от сигнального сервера",
				slog.String("code", errPayload.Code),
				slog.String("message", errPayload.Message))
			// Отказ отправки в группу — с id сообщения: снять с повтора.
			if errPayload.ClientMsgID != "" {
				s.mu.RLock()
				groups := s.groups
				s.mu.RUnlock()
				if groups != nil {
					groups.HandleRelaySendError(errPayload.GroupID, errPayload.ClientMsgID, errPayload.Code)
				}
			}
		}
	}
}

func (s *NetworkService) getNickTag() (string, error) {
	nick := s.MyNickname()

	peerID, err := s.sess.PeerID()
	if err != nil {
		return "", err
	}

	shortID := peerID
	if len(shortID) > 16 {
		shortID = shortID[:16]
	}

	if nick == "" {
		nick = shortID
	}
	return nick + "#" + shortID, nil
}

// onP2PDataMessage — вызывается когда приходит сообщение через DataChannel
func (s *NetworkService) onP2PDataMessage(fromPubKey string, data []byte) {
	s.mu.RLock()
	fn := s.onP2PMessage
	s.mu.RUnlock()

	if fn != nil {
		fn(fromPubKey, data)
	}
}

// onP2PStateChanged — вызывается при изменении состояния P2P соединения
func (s *NetworkService) onP2PStateChanged(pubKey string, connected bool) {
	if connected {
		s.log.Info("🟢 P2P ПОДКЛЮЧЁН",
			slog.String("peer", safeShort(pubKey)))
		s.emitEvent("vox:peer-connected", map[string]string{"id": pubKey})
		s.cancelReconnect(pubKey)

		// Voice re-announce: после ICE restart DC не пересоздаётся,
		// поэтому onDataChannelReady не вызывается — делаем re-announce здесь
		s.mu.RLock()
		vfn := s.onVoicePeerConnected
		psync := s.onProfileSync
		s.mu.RUnlock()
		if vfn != nil {
			go vfn(pubKey)
		}
		// Фото могли сменить, пока шёл ICE-restart, — повторяем обмен хешами.
		if psync != nil {
			go psync(pubKey)
		}
		// caps стёрты на разрыве — после ICE-restart DC прежний, шлём заново.
		go s.sendCaps(pubKey)
	} else {
		s.log.Info("🔴 P2P ОТКЛЮЧЁН",
			slog.String("peer", safeShort(pubKey)))
		s.emitEvent("vox:peer-disconnected", map[string]string{"id": pubKey})
		// Пир мог переподключиться уже старой сборкой — caps только свежие.
		s.clearPeerCaps(pubKey)

		// Очистка голосового канала
		s.mu.RLock()
		vdfn := s.onVoicePeerDisconnected
		s.mu.RUnlock()
		if vdfn != nil {
			vdfn(pubKey)
		}

		// Авто-переподключение (не для call-only пиров)
		s.mu.RLock()
		pm2 := s.peerMgr
		s.mu.RUnlock()
		if pm2 != nil && pm2.IsCallOnly(pubKey) {
			s.log.Debug("пропускаю reconnect для call-only пира", slog.String("peer", safeShort(pubKey)))
		} else {
			s.scheduleReconnect(pubKey)
		}
	}
}

// onDataChannelReady — вызывается когда DataChannel открыт и готов к отправке
func (s *NetworkService) onDataChannelReady(pubKey string) {
	s.log.Info("📡 DATACHANNEL ГОТОВ",
		slog.String("peer", safeShort(pubKey)))

	// Отправляем свой актуальный ник пиру при каждом подключении
	s.syncNicknameToPeer(pubKey)

	s.mu.RLock()
	fn := s.onPeerConnected
	vfn := s.onVoicePeerConnected
	psync := s.onProfileSync
	s.mu.RUnlock()
	if fn != nil {
		fn(pubKey)
	}
	// Хеш фото — отдельно от ника: syncNicknameToPeer молчит при пустом нике.
	if psync != nil {
		psync(pubKey)
	}
	// Возможности клиента (звук демонстрации) — network_screen_audio.go.
	s.sendCaps(pubKey)
	// Fan-out через EventBus: GroupsService доставит pending group messages
	// и pending group actions (invite/leave/kick/rename/...).
	s.emitBus(TopicP2PReady, pubKey)
	if vfn != nil {
		vfn(pubKey)
	}
}

// syncNicknameToPeer отправляет свой текущий ник пиру при подключении DataChannel
func (s *NetworkService) syncNicknameToPeer(pubKey string) {
	nickname := s.MyNickname()
	if nickname == "" {
		return
	}

	msg := struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}{Type: "nickname_update", Content: nickname}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	if err := s.SendP2PData(pubKey, data); err != nil {
		s.log.Debug("не удалось синхронизировать ник при подключении",
			slog.String("peer", safeShort(pubKey)),
			slog.String("error", err.Error()))
	}
}
