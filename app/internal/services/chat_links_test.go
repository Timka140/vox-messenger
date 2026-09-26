package services

import (
	"reflect"
	"testing"
)

func TestExtractLinks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "смотри https://example.com/a?b=1", []string{"https://example.com/a?b=1"}},
		{"trailing dot", "Вот: https://example.com/page.", []string{"https://example.com/page"}},
		{"in parens", "(см. https://example.com/x)", []string{"https://example.com/x"}},
		{"paired paren kept", "wiki https://en.wikipedia.org/wiki/Go_(language) ок", []string{"https://en.wikipedia.org/wiki/Go_(language)"}},
		{"www", "заходи на www.archdaily.com, там всё", []string{"https://www.archdaily.com"}},
		{"cyrillic around", "ссылка:https://vox.app/д?x=1«", []string{"https://vox.app/д?x=1"}},
		{"two links", "http://a.io и https://b.io/", []string{"http://a.io", "https://b.io/"}},
		{"dedup", "https://a.io https://a.io", []string{"https://a.io"}},
		{"no scheme", "example.com и javascript:alert(1)", nil},
		{"ftp ignored", "ftp://files.example.com/x", nil},
		{"guillemets", "«https://example.com/q»", []string{"https://example.com/q"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractLinks(c.in); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("extractLinks(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLinkContext(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Архитектурные референсы — https://archdaily.com/x", "Архитектурные референсы — archdaily.com"},
		{"две ссылки: https://github.com/wailsapp/wails и www.archdaily.com/123, глянь", "две ссылки: github.com и archdaily.com, глянь"},
		{"Смотри доку (см. https://en.wikipedia.org/wiki/Foo_(bar)).", "Смотри доку (см. en.wikipedia.org)."},
		{"https://a.io", ""},
		{"  https://a.io!  ", ""},
		{"(https://a.io) — https://b.io", ""},
	}
	for _, c := range cases {
		if got := linkContext(c.in); got != c.want {
			t.Errorf("linkContext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
