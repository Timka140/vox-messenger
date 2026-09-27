package identity

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Секретная фраза профиля: 12 слов = 128 бит энтропии + 4 бита контрольной
// суммы (схема BIP39). Слова берутся из одного из двух списков по 2048 слов:
// английского (официальный BIP39) или русского (свой). Ключи выводятся из
// энтропии, а не из текста, поэтому одна и та же фраза на любом языке даёт
// один и тот же профиль.

// Язык списка слов.
const (
	LangEN = "en"
	LangRU = "ru"
)

const (
	mnemonicWords   = 12
	entropyBytes    = 16
	wordlistSize    = 2048
	bitsPerWord     = 11
	checksumBitsLen = 4
)

// Строки info для HKDF. МЕНЯТЬ НЕЛЬЗЯ НИКОГДА: от них зависит, какой профиль
// восстановится из уже выданных пользователям фраз.
const (
	hkdfInfoIdentity = "vox identity ed25519 v1"
	hkdfInfoDatabase = "vox database key v1"
)

//go:embed wordlist_en.txt
var wordlistEN string

//go:embed wordlist_ru.txt
var wordlistRU string

var (
	ErrWordCount = errors.New("mnemonic: нужно 12 слов")
	ErrChecksum  = errors.New("mnemonic: контрольная сумма не сходится")
	ErrMixedLang = errors.New("mnemonic: слова из разных списков")
)

// ErrUnknownWord — слова нет в списке. Index — номер слова с 1.
type ErrUnknownWord struct {
	Index int
	Word  string
}

func (e *ErrUnknownWord) Error() string {
	return fmt.Sprintf("mnemonic: слова %d нет в списке", e.Index)
}

type wordlist struct {
	words []string
	index map[string]int
}

var wordlists = map[string]*wordlist{
	LangEN: mustWordlist(LangEN, wordlistEN),
	LangRU: mustWordlist(LangRU, wordlistRU),
}

func mustWordlist(lang, raw string) *wordlist {
	words := strings.Fields(raw)
	if len(words) != wordlistSize {
		panic(fmt.Sprintf("identity: список %s: %d слов вместо %d", lang, len(words), wordlistSize))
	}
	idx := make(map[string]int, len(words))
	for i, w := range words {
		if _, dup := idx[w]; dup {
			panic(fmt.Sprintf("identity: список %s: повтор слова %q", lang, w))
		}
		idx[w] = i
	}
	return &wordlist{words: words, index: idx}
}

// NewEntropy возвращает 16 случайных байт для новой фразы.
func NewEntropy() ([]byte, error) {
	e := make([]byte, entropyBytes)
	if _, err := rand.Read(e); err != nil {
		return nil, fmt.Errorf("identity.NewEntropy: %w", err)
	}
	return e, nil
}

// EntropyToMnemonic записывает энтропию словами выбранного языка.
func EntropyToMnemonic(entropy []byte, lang string) (string, error) {
	wl, ok := wordlists[lang]
	if !ok {
		return "", fmt.Errorf("identity.EntropyToMnemonic: неизвестный язык %q", lang)
	}
	if len(entropy) != entropyBytes {
		return "", fmt.Errorf("identity.EntropyToMnemonic: нужно %d байт", entropyBytes)
	}
	// 128 бит энтропии + 4 бита SHA-256 = 132 бита = 12 × 11.
	sum := sha256.Sum256(entropy)
	bits := make([]byte, 0, entropyBytes+1)
	bits = append(bits, entropy...)
	bits = append(bits, sum[0])
	words := make([]string, mnemonicWords)
	for i := range words {
		words[i] = wl.words[readBits(bits, i*bitsPerWord, bitsPerWord)]
	}
	return strings.Join(words, " "), nil
}

// ParseMnemonic проверяет фразу и возвращает энтропию и язык.
// Регистр, лишние пробелы и «ё» вместо «е» не важны.
func ParseMnemonic(phrase string) ([]byte, string, error) {
	words := strings.Fields(normalizePhrase(phrase))
	if len(words) != mnemonicWords {
		return nil, "", ErrWordCount
	}
	lang := LangEN
	if hasCyrillic(words[0]) {
		lang = LangRU
	}
	wl := wordlists[lang]

	bits := make([]byte, entropyBytes+1)
	for i, w := range words {
		if hasCyrillic(w) != (lang == LangRU) {
			return nil, "", ErrMixedLang
		}
		n, ok := wl.index[w]
		if !ok {
			return nil, "", &ErrUnknownWord{Index: i + 1, Word: w}
		}
		writeBits(bits, i*bitsPerWord, bitsPerWord, n)
	}
	entropy := bits[:entropyBytes]
	sum := sha256.Sum256(entropy)
	if bits[entropyBytes]>>(8-checksumBitsLen) != sum[0]>>(8-checksumBitsLen) {
		return nil, "", ErrChecksum
	}
	return entropy, lang, nil
}

// DeriveKeys выводит из энтропии ключ профиля и 32-байтный ключ базы.
func DeriveKeys(entropy []byte) (ed25519.PrivateKey, ed25519.PublicKey, []byte, error) {
	const op = "identity.DeriveKeys"
	if len(entropy) != entropyBytes {
		return nil, nil, nil, fmt.Errorf("%s: нужно %d байт энтропии", op, entropyBytes)
	}
	seed, err := hkdf.Key(sha256.New, entropy, nil, hkdfInfoIdentity, ed25519.SeedSize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: seed: %w", op, err)
	}
	dbKey, err := hkdf.Key(sha256.New, entropy, nil, hkdfInfoDatabase, dbKeySize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: db key: %w", op, err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey), dbKey, nil
}

func normalizePhrase(s string) string {
	s = strings.ToLower(s)
	return strings.ReplaceAll(s, "ё", "е")
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// readBits читает n бит (старшие первыми) начиная с бита off.
func readBits(b []byte, off, n int) int {
	v := 0
	for i := range n {
		bit := (b[(off+i)/8] >> (7 - (off+i)%8)) & 1
		v = v<<1 | int(bit)
	}
	return v
}

// writeBits записывает n младших бит v начиная с бита off.
func writeBits(b []byte, off, n, v int) {
	for i := range n {
		if (v>>(n-1-i))&1 == 1 {
			b[(off+i)/8] |= 1 << (7 - (off+i)%8)
		}
	}
}
