package services

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"

	"vox_app/internal/e2e"
	"vox_app/internal/network"
)

// Сообщения группы через сервер шифруются для текущих участников
// (internal/e2e/group.go): сервер хранит и пересылает только шифротекст,
// автора удостоверяет подпись. Новый участник читает сообщения с момента
// вступления; исключённый перестаёт получать ключи со следующего сообщения.

// relayPayload — готовый к отправке payload: relay-конверт (цитата,
// «переслано от») зашифрован для активных участников группы и себя.
func (s *GroupsService) relayPayload(groupID, msgID, text string, opts SendOptions) (string, error) {
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return "", err
	}
	members := s.GetGroupMemberIDs(groupID)
	recipients := make([]ed25519.PublicKey, 0, len(members)+1)
	recipients = append(recipients, priv.Public().(ed25519.PublicKey))
	for _, id := range members {
		if b, err := hex.DecodeString(id); err == nil && len(b) == ed25519.PublicKeySize {
			recipients = append(recipients, b)
		}
	}
	plain := encodeRelayPayload(text, opts)
	return e2e.SealGroup(priv, recipients, groupID, msgID, []byte(plain))
}

// errRelayPlaintext — сообщение группы пришло без шифрования: сервер мог его
// подделать, такое не принимаем.
var errRelayPlaintext = errors.New("group relay: открытый текст")

// openRelay расшифровывает payload сообщения группы от p.AuthorKey.
// e2e.ErrNotForMe — сообщение отправлено до нашего вступления.
func (s *GroupsService) openRelay(p *network.GroupMessageNewPayload) (string, error) {
	if !e2e.IsGroupPayload(p.Payload) {
		return "", errRelayPlaintext
	}
	author, err := hex.DecodeString(p.AuthorKey)
	if err != nil || len(author) != ed25519.PublicKeySize {
		return "", fmt.Errorf("group relay: author_key: %w", e2e.ErrBadMessage)
	}
	priv, err := s.sess.PrivateKey()
	if err != nil {
		return "", err
	}
	plain, err := e2e.OpenGroup(priv, author, p.GroupID, p.ClientMsgID, p.Payload)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
