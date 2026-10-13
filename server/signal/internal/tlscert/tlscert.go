// Package tlscert — самоподписанный TLS-сертификат узла. Доверие к нему даёт
// не центр сертификации, а ключ узла Ed25519: SHA-256 сертификата входит в
// подписи входа v2 (internal/auth), и клиент сверяет его с тем, что видит в TLS.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// validity — срок сертификата. Обновлять не нужно: отпечаток подтверждается
// ключом узла при каждом входе, срок CA-проверкой не используется.
const validity = 10 * 365 * 24 * time.Hour

// LoadOrCreate читает пару certFile/keyFile; если файлов нет — создаёт
// ECDSA P-256 сертификат на host. Возвращает сертификат и SHA-256 его DER.
func LoadOrCreate(certFile, keyFile, host string) (tls.Certificate, []byte, error) {
	if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
		if err := create(certFile, keyFile, host); err != nil {
			return tls.Certificate{}, nil, err
		}
	}
	return Load(certFile, keyFile)
}

// Load читает существующую пару (SFU — файлы создаёт сигналка).
func Load(certFile, keyFile string) (tls.Certificate, []byte, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("tlscert: %w", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return cert, sum[:], nil
}

func create(certFile, keyFile, host string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	if host == "" {
		host = "vox-node"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return err
	}
	return os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

// PeerHash — SHA-256 листового сертификата собеседника TLS-соединения; nil —
// соединение без TLS.
func PeerHash(st *tls.ConnectionState) []byte {
	if st == nil || len(st.PeerCertificates) == 0 {
		return nil
	}
	sum := sha256.Sum256(st.PeerCertificates[0].Raw)
	return sum[:]
}
