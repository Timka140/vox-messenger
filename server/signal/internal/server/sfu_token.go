package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"messenger_signal/internal/hub"
	"messenger_signal/pkg/protocol"
)

// maxSFURoomLen — id группы (UUID) с запасом; комната идёт в URL SFU.
const maxSFURoomLen = 64

// validSFURoom — непустой id из [A-Za-z0-9_-]: клиент вставляет его в query
// без экранирования.
func validSFURoom(room string) bool {
	if room == "" || len(room) > maxSFURoomLen {
		return false
	}
	for _, c := range room {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// sfuToken — тот же HMAC, что проверяет SFU (server/sfu/internal/handler).
func sfuToken(secret, peer, room string, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s:%s:%d", peer, room, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

// handleSFUTokenReq выдаёт токен входа в комнату SFU. Секрет SFU остаётся на
// узле, а peer в токене — ключ этого соединения: под чужим ключом в комнату
// не войти и чужое соединение не выбить. Членство проверяется для групп,
// которые знает узел (группы через сервер); про P2P-группы и звонки узел не
// знает, их звук защищён сквозным шифрованием.
func (s *Server) handleSFUTokenReq(peer *hub.Peer, msg *protocol.Message) {
	var p protocol.SFUTokenReqPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		s.sendError(peer, protocol.ErrInvalidMsg, "invalid sfu_token_req payload")
		return
	}
	resp := protocol.SFUTokenPayload{Room: p.Room}
	switch {
	case s.sfu.Endpoint == "":
		resp.Error = "sfu_disabled"
	case !validSFURoom(p.Room):
		resp.Error = "invalid_room"
	default:
		if s.groupsEnabled() {
			known, err := s.groups.GetGroup(p.Room)
			if err != nil {
				resp.Error = "group_lookup_failed"
				break
			}
			if known != nil {
				member, err := s.groups.IsMember(p.Room, peer.PublicKey)
				if err != nil || !member {
					resp.Error = protocol.ErrNotGroupMember
					break
				}
			}
		}
		resp.Peer = peer.PublicKey
		resp.TS = time.Now().Unix()
		resp.Token = sfuToken(s.sfu.Secret, peer.PublicKey, p.Room, resp.TS)
	}
	if resp.Error != "" {
		s.log.Info("sfu_token: отказ",
			slog.String("peer", peer.NickTag()),
			slog.String("error", resp.Error))
	}
	payload, _ := json.Marshal(resp)
	data, _ := json.Marshal(protocol.Message{Type: protocol.TypeSFUToken, Payload: payload})
	_ = peer.Send(data)
}
