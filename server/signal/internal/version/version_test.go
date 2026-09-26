package version

import "testing"

func TestParseAndCompare(t *testing.T) {
	a := MustParse("v0.00.000.0000")
	b := MustParse("v0.00.000.0001")
	if !a.Less(b) {
		t.Fatal("expected a < b")
	}
	if a.Compare(b) != -1 {
		t.Fatal("expected Compare -1")
	}
	if b.Compare(a) != 1 {
		t.Fatal("expected Compare 1")
	}
	if a.Compare(a) != 0 {
		t.Fatal("expected Compare 0")
	}
}

func TestParseErrors(t *testing.T) {
	bad := []string{"", "0.00.000.0000", "v0.0.000.0000", "v0.00.000.00000"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error", s)
		}
	}
}

func TestString(t *testing.T) {
	v := Version{Major: 1, Minor: 2, Patch: 3, Build: 4}
	if v.String() != "v1.02.003.0004" {
		t.Errorf("String = %q", v.String())
	}
}

func TestRoundtrip(t *testing.T) {
	s := "v12.34.567.8901"
	v := MustParse(s)
	if v.String() != s {
		t.Errorf("roundtrip: %q -> %q", s, v.String())
	}
}
