package oui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheMakerComesFromTheMostSpecificAssignment(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("oui.csv", "Registry,Assignment,Organization Name,Organization Address\nMA-L,001122,Acme Networks,\"1 Road, Town\"\nMA-L,0050C2,Big Company Ltd,x\n")
	write("mam.csv", "Registry,Assignment,Organization Name,Organization Address\nMA-M,0050C2A,Small Maker A,y\n")
	write("oui36.csv", "Registry,Assignment,Organization Name,Organization Address\nMA-S,0050C2B12,Tiny Maker,z\n")
	old := Dir
	Dir = dir
	Reset()
	defer func() { Dir = old; Reset() }()

	for mac, want := range map[string]string{
		"00:11:22:33:44:55": "Acme Networks",
		"00-11-22-aa-bb-cc": "Acme Networks",
		"0011.2233.4455":    "Acme Networks",
		"00:50:c2:00:00:01": "Big Company Ltd",
		"00:50:c2:a1:23:45": "Small Maker A",
		"00:50:c2:b1:2f:ff": "Tiny Maker",
		"00:50:c2:c1:23:45": "Big Company Ltd",
		"aa:bb:cc:dd:ee:ff": "", // not in the registry (and a private address)
		"00:99:99:00:00:00": "",
		"not a mac":         "",
		"":                  "",
	} {
		if got := Vendor(mac); got != want {
			t.Errorf("%q: %q, want %q", mac, got, want)
		}
	}
	// a private address names no maker even when its prefix is in the registry
	write("oui.csv", "Registry,Assignment,Organization Name,Organization Address\nMA-L,021122,Looks Like A Maker,\n")
	Reset()
	if got := Vendor("02:11:22:33:44:55"); got != "" {
		t.Errorf("%q", got)
	}
	for mac, want := range map[string]bool{"02:00:00:00:00:00": true, "da:a1:19:00:00:00": true, "00:11:22:33:44:55": false, "f4:f5:d8:00:00:00": false, "junk": false} {
		if Private(mac) != want {
			t.Errorf("Private(%q) = %v", mac, !want)
		}
	}
}

func TestWithoutTheRegistryNothingIsKnown(t *testing.T) {
	old := Dir
	Dir = filepath.Join(t.TempDir(), "nothing")
	Reset()
	defer func() { Dir = old; Reset() }()
	if got := Vendor("00:11:22:33:44:55"); got != "" {
		t.Errorf("%q", got)
	}
}
