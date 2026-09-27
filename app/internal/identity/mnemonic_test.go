package identity

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Официальные векторы BIP39 (английский, 128 бит).
var bip39Vectors = []struct{ entropy, phrase string }{
	{"00000000000000000000000000000000", "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"},
	{"7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f", "legal winner thank year wave sausage worth useful legal winner thank yellow"},
	{"80808080808080808080808080808080", "letter advice cage absurd amount doctor acoustic avoid letter advice cage above"},
	{"ffffffffffffffffffffffffffffffff", "zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong"},
}

func TestBIP39Vectors(t *testing.T) {
	for _, v := range bip39Vectors {
		e, _ := hex.DecodeString(v.entropy)
		got, err := EntropyToMnemonic(e, LangEN)
		if err != nil || got != v.phrase {
			t.Fatalf("EntropyToMnemonic(%s) = %q, %v; want %q", v.entropy, got, err, v.phrase)
		}
		back, lang, err := ParseMnemonic(v.phrase)
		if err != nil || lang != LangEN || !bytes.Equal(back, e) {
			t.Fatalf("ParseMnemonic(%q) = %x %s %v", v.phrase, back, lang, err)
		}
	}
}

func TestRoundTripBothLanguages(t *testing.T) {
	for range 200 {
		e, err := NewEntropy()
		if err != nil {
			t.Fatal(err)
		}
		for _, lang := range []string{LangEN, LangRU} {
			p, err := EntropyToMnemonic(e, lang)
			if err != nil {
				t.Fatal(err)
			}
			back, gotLang, err := ParseMnemonic("  " + strings.ToUpper(p) + "\n")
			if err != nil || gotLang != lang || !bytes.Equal(back, e) {
				t.Fatalf("%s: %q → %x %s %v", lang, p, back, gotLang, err)
			}
		}
	}
}

func TestParseErrors(t *testing.T) {
	good := bip39Vectors[1].phrase
	words := strings.Fields(good)

	if _, _, err := ParseMnemonic(strings.Join(words[:11], " ")); !errors.Is(err, ErrWordCount) {
		t.Fatalf("11 слов: %v", err)
	}

	bad := append([]string(nil), words...)
	bad[7] = "usefull"
	var unk *ErrUnknownWord
	if _, _, err := ParseMnemonic(strings.Join(bad, " ")); !errors.As(err, &unk) || unk.Index != 8 {
		t.Fatalf("неизвестное слово: %v", err)
	}

	swapped := append([]string(nil), words...)
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if _, _, err := ParseMnemonic(strings.Join(swapped, " ")); !errors.Is(err, ErrChecksum) {
		t.Fatalf("перестановка слов: %v", err)
	}

	mixed := append([]string(nil), words...)
	mixed[3] = wordlists[LangRU].words[0]
	if _, _, err := ParseMnemonic(strings.Join(mixed, " ")); !errors.Is(err, ErrMixedLang) {
		t.Fatalf("смешанные языки: %v", err)
	}
}

func TestYoNormalization(t *testing.T) {
	e, _ := hex.DecodeString(bip39Vectors[2].entropy)
	p, _ := EntropyToMnemonic(e, LangRU)
	// Русский список без «ё», но пользователь может её набрать.
	withYo := strings.Replace(p, "е", "ё", 1)
	if back, _, err := ParseMnemonic(withYo); err != nil || !bytes.Equal(back, e) {
		t.Fatalf("ё: %v", err)
	}
}

func TestSameProfileForBothLanguages(t *testing.T) {
	e, _ := NewEntropy()
	en, _ := EntropyToMnemonic(e, LangEN)
	ru, _ := EntropyToMnemonic(e, LangRU)
	e1, _, _ := ParseMnemonic(en)
	e2, _, _ := ParseMnemonic(ru)
	_, pub1, db1, _ := DeriveKeys(e1)
	_, pub2, db2, _ := DeriveKeys(e2)
	if !bytes.Equal(pub1, pub2) || !bytes.Equal(db1, db2) {
		t.Fatal("разные ключи для одной энтропии на разных языках")
	}
}

// Золотые значения: если тест упал, изменились info-строки HKDF или разбор
// фразы — уже выданные фразы перестанут восстанавливать профили.
func TestDeriveKeysGolden(t *testing.T) {
	e, _, err := ParseMnemonic(bip39Vectors[1].phrase)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, dbKey, err := DeriveKeys(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != 64 || len(dbKey) != 32 || bytes.Equal(priv.Seed(), dbKey) {
		t.Fatal("размеры или совпадение ключей")
	}
	const wantPub = "d0fff3ca10bd1fca172c0c0e71fbed177f1b0e9b77d7e5387429b76eb8c8b7f0"
	const wantDB = "594ce1e4f65bd724ec31c4c8c66ed9c334af79384c654077b601495fd0681501"
	if hex.EncodeToString(pub) != wantPub || hex.EncodeToString(dbKey) != wantDB {
		t.Fatalf("pub %x db %x", pub, dbKey)
	}
}

func TestRussianWordlistRules(t *testing.T) {
	wl := wordlists[LangRU]
	prefixes := map[string]string{}
	for _, w := range wl.words {
		n := utf8.RuneCountInString(w)
		if n < 4 || n > 8 {
			t.Errorf("%q: длина %d", w, n)
		}
		for _, r := range w {
			if r < 'а' || r > 'я' {
				t.Errorf("%q: буква %q", w, r)
			}
		}
		p := string([]rune(w)[:4])
		if other, ok := prefixes[p]; ok {
			t.Errorf("%q и %q: одинаковые первые 4 буквы", w, other)
		}
		prefixes[p] = w
	}
	for i := 1; i < len(wl.words); i++ {
		if wl.words[i-1] >= wl.words[i] {
			t.Fatalf("список не отсортирован: %q, %q", wl.words[i-1], wl.words[i])
		}
	}
}
