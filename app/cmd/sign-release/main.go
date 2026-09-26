// sign-release — утилита для подписи release бинарей приватным Ed25519 ключом.
//
// Режимы:
//
//	sign-release -gen-key <prefix>
//	    Генерирует пару ключей: <prefix>.key (private, 0600) и <prefix>.pub (public, base64).
//	    Используется один раз при первоначальной настройке.
//
//	sign-release -key <priv> -in <binary> -out <sigfile>
//	    Подписывает SHA-256 хеш бинаря приватным ключом Ed25519.
//	    Записывает base64 подпись в <sigfile>.
//	    Печатает hex SHA-256 в stdout (для capture в release.sh).
//
// Публичный ключ встраивается в клиент через ldflags:
//
//	-X main.UpdatePublicKey=<base64>
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	var (
		genKey  = flag.String("gen-key", "", "generate keypair with given prefix (writes <prefix>.key and <prefix>.pub)")
		keyPath = flag.String("key", "", "path to Ed25519 private key (base64)")
		inPath  = flag.String("in", "", "binary to sign")
		outPath = flag.String("out", "", "output signature file (base64)")
	)
	flag.Parse()

	if *genKey != "" {
		if err := doGenKey(*genKey); err != nil {
			fmt.Fprintln(os.Stderr, "gen-key:", err)
			os.Exit(1)
		}
		return
	}

	if *keyPath == "" || *inPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: sign-release -key <priv> -in <binary> -out <sigfile>")
		fmt.Fprintln(os.Stderr, "       sign-release -gen-key <prefix>")
		os.Exit(2)
	}

	sha, err := doSign(*keyPath, *inPath, *outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
	// Печатаем SHA-256 hex в stdout для capture в shell script
	fmt.Println(sha)
}

func doGenKey(prefix string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	privFile := prefix + ".key"
	pubFile := prefix + ".pub"

	// Не перезаписываем существующий приватный ключ — защита от случайной потери
	if _, err := os.Stat(privFile); err == nil {
		return fmt.Errorf("refuse to overwrite existing %s", privFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", privFile, err)
	}

	if err := os.WriteFile(privFile, []byte(privB64+"\n"), 0600); err != nil {
		return fmt.Errorf("write priv: %w", err)
	}
	if err := os.WriteFile(pubFile, []byte(pubB64+"\n"), 0644); err != nil {
		return fmt.Errorf("write pub: %w", err)
	}

	fmt.Printf("Generated keypair:\n")
	fmt.Printf("  private: %s (KEEP SECRET, mode 0600)\n", privFile)
	fmt.Printf("  public:  %s\n", pubFile)
	fmt.Printf("\nPublic key (base64, for ldflags):\n%s\n", pubB64)
	return nil
}

func doSign(keyPath, inPath, outPath string) (string, error) {
	// Прочитать и декодировать приватный ключ
	privB64, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	privBytes, err := base64.StdEncoding.DecodeString(trim(string(privB64)))
	if err != nil {
		return "", fmt.Errorf("decode key: %w", err)
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("bad private key size %d (want %d)", len(privBytes), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(privBytes)

	// Стримом посчитать SHA-256 бинаря
	f, err := os.Open(inPath)
	if err != nil {
		return "", fmt.Errorf("open in: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	sum := h.Sum(nil)

	// Подписываем sha256 bytes
	sig := ed25519.Sign(priv, sum)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	if err := os.WriteFile(outPath, []byte(sigB64+"\n"), 0644); err != nil {
		return "", fmt.Errorf("write sig: %w", err)
	}

	return hex.EncodeToString(sum), nil
}

func trim(s string) string {
	// простой trim пробелов и переводов строк без strings import
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == '\n' || s[0] == '\r' || s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}
