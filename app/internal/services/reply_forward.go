package services

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ReplyRef — цитата в ответе. Снимок исходного сообщения на момент ответа:
// получатель показывает его, даже если оригинал у него не загружен или удалён.
// Kind: text | voice | photo | file — иконка в цитате.
type ReplyRef struct {
	ID         string `json:"id"`
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName"`
	Snippet    string `json:"snippet"`
	Kind       string `json:"kind,omitempty"`
}

// ForwardRef — «Переслано от {Name}».
type ForwardRef struct {
	Name string `json:"name"`
}

// SendOptions — необязательные поля текстового сообщения.
type SendOptions struct {
	ReplyTo       *ReplyRef   `json:"replyTo,omitempty"`
	ForwardedFrom *ForwardRef `json:"forwardedFrom,omitempty"`
}

const (
	replySnippetMax = 140
	refNameMax      = 64
)

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// normalize обрезает поля до разумных размеров и выбрасывает пустые ссылки.
// Вызывается и на отправке, и на приёме: чужой клиент может прислать что угодно.
func (o SendOptions) normalize() SendOptions {
	if r := o.ReplyTo; r != nil {
		if r.ID == "" {
			o.ReplyTo = nil
		} else {
			o.ReplyTo = &ReplyRef{
				ID:         truncRunes(r.ID, 64),
				SenderID:   truncRunes(r.SenderID, 128),
				SenderName: truncRunes(r.SenderName, refNameMax),
				Snippet:    truncRunes(r.Snippet, replySnippetMax),
				Kind:       truncRunes(r.Kind, 8),
			}
		}
	}
	if f := o.ForwardedFrom; f != nil {
		if strings.TrimSpace(f.Name) == "" {
			o.ForwardedFrom = nil
		} else {
			o.ForwardedFrom = &ForwardRef{Name: truncRunes(f.Name, refNameMax)}
		}
	}
	return o
}

func (o SendOptions) empty() bool { return o.ReplyTo == nil && o.ForwardedFrom == nil }

// encodeRef — JSON для колонки reply_to / forwarded_from; nil → "".
func encodeRef[T any](v *T) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeReply(s string) *ReplyRef {
	if s == "" {
		return nil
	}
	var r ReplyRef
	if json.Unmarshal([]byte(s), &r) != nil || r.ID == "" {
		return nil
	}
	return &r
}

func decodeForward(s string) *ForwardRef {
	if s == "" {
		return nil
	}
	var f ForwardRef
	if json.Unmarshal([]byte(s), &f) != nil || f.Name == "" {
		return nil
	}
	return &f
}

// ── Relay-конверт ────────────────────────────────────────────────────────
//
// Сигнальный сервер хранит payload группового сообщения непрозрачной строкой
// и теряет поля верхнего уровня. Цитату и «переслано от» кладём внутрь строки:
// relayEnvPrefix + JSON {t, r, f}. Обычные сообщения идут голым текстом, как
// раньше, — старые клиенты их видят без изменений.

const relayEnvPrefix = "\x1evox1:"

type relayEnvelope struct {
	T string      `json:"t"`
	R *ReplyRef   `json:"r,omitempty"`
	F *ForwardRef `json:"f,omitempty"`
}

func encodeRelayPayload(text string, opts SendOptions) string {
	if opts.empty() {
		return text
	}
	b, err := json.Marshal(relayEnvelope{T: text, R: opts.ReplyTo, F: opts.ForwardedFrom})
	if err != nil {
		return text
	}
	return relayEnvPrefix + string(b)
}

func decodeRelayPayload(payload string) (string, SendOptions) {
	if !strings.HasPrefix(payload, relayEnvPrefix) {
		return payload, SendOptions{}
	}
	var env relayEnvelope
	if err := json.Unmarshal([]byte(payload[len(relayEnvPrefix):]), &env); err != nil {
		return payload, SendOptions{}
	}
	return env.T, SendOptions{ReplyTo: env.R, ForwardedFrom: env.F}.normalize()
}

// ── Прыжок к цитате ──────────────────────────────────────────────────────

// GetMessageOffset — сколько сообщений новее msgID в чате (DM или группа).
// Фронт догружает историю до этого смещения и прокручивает к сообщению.
// -1 — сообщения нет или оно удалено.
func (s *MessagingService) GetMessageOffset(peerID, groupID, msgID string) (int, error) {
	table, col, chat := "messages", "peer_id", peerID
	if groupID != "" {
		table, col, chat = "group_messages", "group_id", groupID
	}
	if chat == "" || msgID == "" {
		return -1, fmt.Errorf("GetMessageOffset: chat and msgID required")
	}
	var createdAt int64
	err := s.store.DB().QueryRow(
		"SELECT created_at FROM "+table+" WHERE id = ? AND "+col+" = ? AND deleted = 0",
		msgID, chat).Scan(&createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("GetMessageOffset: %w", err)
	}
	var newer int
	if err := s.store.DB().QueryRow(
		"SELECT COUNT(*) FROM "+table+" WHERE "+col+" = ? AND deleted = 0 AND created_at > ?",
		chat, createdAt).Scan(&newer); err != nil {
		return -1, fmt.Errorf("GetMessageOffset: count: %w", err)
	}
	return newer, nil
}

// ── «печатает…» ──────────────────────────────────────────────────────────

// SendTyping — лёгкий сигнал набора текста. Только по уже открытым P2P-каналам:
// без БД, без очереди, без установки новых соединений. Фронт шлёт не чаще
// раза в 3 с, получатель гасит индикатор сам через 5 с.
func (s *MessagingService) SendTyping(peerID, groupID string) {
	msg := P2PMessage{Type: "typing", GroupID: groupID, Time: time.Now().Unix()}
	if groupID != "" {
		msg.Nickname = s.network.MyNickname()
	}
	data := s.marshalP2P(msg)
	if data == nil {
		return
	}
	if groupID == "" {
		if peerID != "" && s.network.IsP2PConnected(peerID) {
			_ = s.network.SendP2PData(peerID, data)
		}
		return
	}
	if s.groups == nil {
		return
	}
	myID, err := s.sess.PeerID()
	if err != nil {
		return
	}
	for _, pid := range s.groups.GetGroupMemberIDs(groupID) {
		if pid != myID && s.network.IsP2PConnected(pid) {
			_ = s.network.SendP2PData(pid, data)
		}
	}
}

func (s *MessagingService) handleTyping(fromPubKey string, msg *P2PMessage) {
	if msg.GroupID == "" {
		s.network.emitEvent("vox:typing", map[string]string{"peerId": fromPubKey})
		return
	}
	var status string
	if err := s.store.DB().QueryRow(
		"SELECT status FROM group_members WHERE group_id = ? AND peer_id = ?",
		msg.GroupID, fromPubKey).Scan(&status); err != nil || status != "active" {
		s.log.Debug("typing от не-участника группы, игнорируем",
			slog.String("group", safeShort8(msg.GroupID)),
			slog.String("peer", safeShort(fromPubKey)))
		return
	}
	name := s.network.PeerNickname(fromPubKey)
	if name == "" || name == safeShort(fromPubKey) {
		if n := truncRunes(strings.TrimSpace(msg.Nickname), refNameMax); n != "" {
			name = n
		}
	}
	s.network.emitEvent("vox:typing", map[string]string{
		"groupId": msg.GroupID,
		"peerId":  fromPubKey,
		"name":    name,
	})
}
