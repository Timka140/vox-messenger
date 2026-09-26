package version

import "testing"

func TestParseValid(t *testing.T) {
	cases := []struct {
		in           string
		maj, min, pa, bl int
	}{
		{"v0.00.000.0000", 0, 0, 0, 0},
		{"v0.00.000.0001", 0, 0, 0, 1},
		{"v1.23.456.7890", 1, 23, 456, 7890},
		{"v12.99.999.9999", 12, 99, 999, 9999},
	}
	for _, c := range cases {
		v, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.in, err)
		}
		if v.Major != c.maj || v.Minor != c.min || v.Patch != c.pa || v.Build != c.bl {
			t.Errorf("Parse(%q) = %+v, want %d.%d.%d.%d", c.in, v, c.maj, c.min, c.pa, c.bl)
		}
		if v.Raw != c.in {
			t.Errorf("Raw = %q, want %q", v.Raw, c.in)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	bad := []string{
		"",
		"0.00.000.0000",       // no v
		"v0.0.000.0000",       // minor not 2 digits
		"v0.00.00.0000",       // patch not 3
		"v0.00.000.000",       // build not 4
		"v0.00.000.00000",     // build too long
		"v0.00.000",           // missing part
		"v0.00.000.0000.0000", // too many
		"x0.00.000.0000",
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error", s)
		}
	}
}

func TestString(t *testing.T) {
	v := Version{Major: 1, Minor: 2, Patch: 3, Build: 4}
	want := "v1.02.003.0004"
	if got := v.String(); got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.00.000.0000", "v0.00.000.0000", 0},
		{"v0.00.000.0000", "v0.00.000.0001", -1},
		{"v0.00.000.0001", "v0.00.000.0000", 1},
		{"v0.00.000.9999", "v0.00.001.0000", -1},
		{"v0.00.999.9999", "v0.01.000.0000", -1},
		{"v0.99.999.9999", "v1.00.000.0000", -1},
		{"v1.00.000.0000", "v0.99.999.9999", 1},
	}
	for _, c := range cases {
		va := MustParse(c.a)
		vb := MustParse(c.b)
		if got := va.Compare(vb); got != c.want {
			t.Errorf("%s compare %s = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLess(t *testing.T) {
	a := MustParse("v0.00.000.0000")
	b := MustParse("v0.00.000.0001")
	if !a.Less(b) {
		t.Error("expected a < b")
	}
	if b.Less(a) {
		t.Error("expected !(b < a)")
	}
}

func TestIsZero(t *testing.T) {
	if !(Version{}.IsZero()) {
		t.Error("zero Version must be zero")
	}
	if MustParse("v0.00.000.0000").IsZero() {
		t.Error("parsed v0.00.000.0000 has Raw set, not zero")
	}
}

func TestRoundtrip(t *testing.T) {
	s := "v12.99.999.9999"
	v := MustParse(s)
	if v.String() != s {
		t.Errorf("roundtrip: %q -> %+v -> %q", s, v, v.String())
	}
}
