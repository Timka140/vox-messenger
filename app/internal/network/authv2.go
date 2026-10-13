package network

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"strings"

	"github.com/gorilla/websocket"
)

// Вход v2 — тот же формат, что server/signal/internal/auth/v2.go: каждая
// подпись над метка||поля, никогда над сырыми чужими байтами. В подписи
// входит SHA-256 TLS-сертификата узла: сертификат самоподписанный, и только
// подпись ключом узла доказывает, что TLS заканчивается у него, а не у
// посредника.
const (
	authLabelClient   = "vox-auth-client-v2"
	authLabelServer   = "vox-auth-server-v2"
	authLabelRedirect = "vox-redirect-v2"
	authVersion       = 2
)

// authMessage — метка, 0x00, поля как uint16 big-endian длина + байты.
func authMessage(label string, fields ...[]byte) []byte {
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

// signalURL — адрес узла в WebSocket URL. По умолчанию wss://; явная схема
// ws:// (только тестовые экземпляры, ENV=local) сохраняется. Без пути — /ws.
func signalURL(addr string) string {
	scheme := "wss://"
	switch {
	case strings.HasPrefix(addr, "wss://"):
		addr = strings.TrimPrefix(addr, "wss://")
	case strings.HasPrefix(addr, "ws://"):
		scheme, addr = "ws://", strings.TrimPrefix(addr, "ws://")
	}
	if !strings.Contains(addr, "/") {
		addr += "/ws"
	}
	return scheme + addr
}

// insecureTLS — CA не проверяется: сертификат узла самоподписанный, его
// подлинность подтверждает подпись входа v2 (см. connTLSHash).
func insecureTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

// connTLSHash — SHA-256 листового сертификата узла; nil — соединение без TLS.
func connTLSHash(conn *websocket.Conn) []byte {
	tc, ok := conn.UnderlyingConn().(*tls.Conn)
	if !ok {
		return nil
	}
	st := tc.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return nil
	}
	sum := sha256.Sum256(st.PeerCertificates[0].Raw)
	return sum[:]
}

// ErrCertPin — сертификат не тот, что узел подтвердил своим ключом при входе.
var ErrCertPin = errors.New("TLS certificate does not match the node")

// PinnedTLS — tls.Config, который принимает только сертификат с заданным
// SHA-256 (подтверждённым ключом узла при входе). Для SFU и HTTP того же узла.
func PinnedTLS(certHash []byte) *tls.Config {
	want := append([]byte(nil), certHash...)
	return &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(st tls.ConnectionState) error {
			if len(want) == 0 || len(st.PeerCertificates) == 0 {
				return ErrCertPin
			}
			sum := sha256.Sum256(st.PeerCertificates[0].Raw)
			if string(sum[:]) != string(want) {
				return ErrCertPin
			}
			return nil
		},
	}
}
