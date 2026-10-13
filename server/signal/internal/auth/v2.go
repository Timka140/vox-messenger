package auth

import "encoding/binary"

// Метки подписей входа v2. Каждая подпись — над Message(метка, поля…), никогда
// над сырыми чужими байтами: иначе ключ узла превращается в оракул (подпись
// client_challenge от клиента годилась как подпись node_auth). Тот же формат
// продублирован в клиенте (app/internal/network/authv2.go), тест-векторы общие.
const (
	LabelClient   = "vox-auth-client-v2"  // клиент → узел: server_pubkey, challenge, tls, client_pubkey
	LabelServer   = "vox-auth-server-v2"  // узел → клиент: client_pubkey, client_challenge, tls, server_pubkey
	LabelRedirect = "vox-redirect-v2"     // узел → клиент: client_challenge, addr, pubkey, tls
	LabelNode     = "vox-node-auth-v2"    // инициатор → узел: receiver_id, challenge, tls, initiator_id
	LabelNodeOK   = "vox-node-auth-ok-v2" // узел → инициатор: initiator_id, client_challenge, tls, receiver_id
)

// AuthVersion — версия входа, которую объявляет клиент в auth.
const AuthVersion = 2

// Message — подписываемые байты: метка, 0x00, затем каждое поле как
// uint16 big-endian длина + байты. Длины исключают склейку соседних полей.
func Message(label string, fields ...[]byte) []byte {
	n := len(label) + 1
	for _, f := range fields {
		n += 2 + len(f)
	}
	out := make([]byte, 0, n)
	out = append(out, label...)
	out = append(out, 0)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint16(out, uint16(len(f)))
		out = append(out, f...)
	}
	return out
}
