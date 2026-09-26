// Package version реализует версионирование формата "v0.00.000.0000".
// Parse/Compare используется системой автообновления и signal-server.
package version

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version — семантическая версия приложения в формате v<Major>.<Minor>.<Patch>.<Build>.
// Maj=1 цифра, Min=2 цифры, Patch=3 цифры, Build=4 цифры (zero-padded).
type Version struct {
	Major int
	Minor int
	Patch int
	Build int
	Raw   string
}

var versionRE = regexp.MustCompile(`^v(\d+)\.(\d{2})\.(\d{3})\.(\d{4})$`)

// Parse разбирает строку формата "v0.00.000.0000" в Version.
func Parse(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version.Parse: bad format %q, expected v0.00.000.0000", s)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	bld, _ := strconv.Atoi(m[4])
	return Version{Major: maj, Minor: min, Patch: pat, Build: bld, Raw: s}, nil
}

// MustParse — как Parse, но паникует при ошибке. Для констант.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

// String возвращает каноническое представление с zero-padding.
func (v Version) String() string {
	return fmt.Sprintf("v%d.%02d.%03d.%04d", v.Major, v.Minor, v.Patch, v.Build)
}

// Compare возвращает -1 если v < o, 0 если равны, 1 если v > o.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpInt(v.Minor, o.Minor)
	case v.Patch != o.Patch:
		return cmpInt(v.Patch, o.Patch)
	case v.Build != o.Build:
		return cmpInt(v.Build, o.Build)
	default:
		return 0
	}
}

// Less возвращает true если v < o.
func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

// IsZero возвращает true если версия пустая (все компоненты == 0 и Raw пуст).
func (v Version) IsZero() bool {
	return v.Major == 0 && v.Minor == 0 && v.Patch == 0 && v.Build == 0 && v.Raw == ""
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
