package update

import (
	"strconv"
	"strings"
)

// Version is a release number in the order semantic versioning gives: v0.9.5-alpha < v0.10.0-alpha < v0.10.0.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string // the parts after the hyphen, split at dots; none for a full release
}

// ParseVersion reads "v0.10.0", "0.10.0-alpha" and "v1.2.3-rc.1+build5" (build data is ignored). A missing minor or patch is zero.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Version
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" {
			return Version{}, false
		}
		v.Pre = strings.Split(pre, ".")
		for _, p := range v.Pre {
			if p == "" {
				return Version{}, false
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, false
		}
		n[i] = x
	}
	v.Major, v.Minor, v.Patch = n[0], n[1], n[2]
	return v, true
}

func (v Version) String() string {
	s := "v" + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// Compare is -1, 0 or 1 as v is older than, the same as or newer than w.
func (v Version) Compare(w Version) int {
	if c := cmpInt(v.Major, w.Major); c != 0 {
		return c
	}
	if c := cmpInt(v.Minor, w.Minor); c != 0 {
		return c
	}
	if c := cmpInt(v.Patch, w.Patch); c != 0 {
		return c
	}
	// a full release is newer than any pre-release of the same number
	switch {
	case len(v.Pre) == 0 && len(w.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(w.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(w.Pre); i++ {
		a, b := v.Pre[i], w.Pre[i]
		an, bn := isNumeric(a), isNumeric(b)
		switch {
		case an && bn:
			x, _ := strconv.Atoi(a)
			y, _ := strconv.Atoi(b)
			if c := cmpInt(x, y); c != 0 {
				return c
			}
		case an: // numbers are older than names
			return -1
		case bn:
			return 1
		default:
			if c := strings.Compare(a, b); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(v.Pre), len(w.Pre))
}
