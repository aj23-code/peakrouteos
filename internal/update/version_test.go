package update

import "testing"

func TestVersionsAreOrderedLikeSemver(t *testing.T) {
	// each is older than the next
	order := []string{"v0.9.5-alpha", "v0.10.0-alpha", "v0.10.0-alpha.1", "v0.10.0-alpha.2", "v0.10.0-alpha.10", "v0.10.0-beta", "v0.10.0-rc.1", "v0.10.0", "v0.10.1", "v1.0.0"}
	for i := range order {
		for j := range order {
			a, ok := ParseVersion(order[i])
			if !ok {
				t.Fatalf("%q did not parse", order[i])
			}
			b, ok := ParseVersion(order[j])
			if !ok {
				t.Fatalf("%q did not parse", order[j])
			}
			want := cmpInt(i, j)
			if got := a.Compare(b); got != want {
				t.Errorf("%s compared with %s: %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestVersionsParseTheWaysTagsAreWritten(t *testing.T) {
	for in, want := range map[string]string{
		"v0.10.0":        "v0.10.0",
		"0.10.0":         "v0.10.0",
		"v1.2":           "v1.2.0",
		"v2":             "v2.0.0",
		"v1.2.3-rc.1+b5": "v1.2.3-rc.1",
		" v0.9.5-alpha ": "v0.9.5-alpha",
	} {
		v, ok := ParseVersion(in)
		if !ok || v.String() != want {
			t.Errorf("%q: %v %v, want %s", in, v, ok, want)
		}
	}
	for _, in := range []string{"", "v", "latest", "v1.2.3.4", "v1.x", "v-1.0.0", "v1.0.0-", "v1.0.0-a..b", "v01.2.3", "v1.2.-3"} {
		if v, ok := ParseVersion(in); ok {
			t.Errorf("%q parsed as %v", in, v)
		}
	}
}

func TestAVersionWithABuildSuffixIsStillOrdered(t *testing.T) {
	// what a daemon built from a dirty checkout calls itself
	dirty, ok := ParseVersion("v0.9.5-alpha-gabc1234-dirty")
	if !ok {
		t.Fatal("did not parse")
	}
	release, _ := ParseVersion("v0.9.5")
	older, _ := ParseVersion("v0.9.4")
	if dirty.Compare(release) >= 0 || dirty.Compare(older) <= 0 {
		t.Errorf("%v is not between %v and %v", dirty, older, release)
	}
}
