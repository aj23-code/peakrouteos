// Package oui names the maker of a network device from its hardware address, with the IEEE registry that Debian ships (the package
// ieee-data: oui.csv for 24-bit assignments, mam.csv for 28-bit and oui36.csv for 36-bit ones). The registry is read the first time it
// is asked. Without the files nothing is known and every answer is empty.
package oui

import (
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Dir is where the registry is (the tests point it elsewhere).
var Dir = "/usr/share/ieee-data"

var (
	once    sync.Once
	vendors map[string]string // hex prefix (upper case, 6, 7 or 9 digits) -> organisation
)

func load() {
	vendors = map[string]string{}
	for _, name := range []string{"oui.csv", "mam.csv", "oui36.csv"} {
		f, err := os.Open(filepath.Join(Dir, name))
		if err != nil {
			continue
		}
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		for {
			rec, err := r.Read()
			if err == io.EOF {
				break
			}
			if err != nil || len(rec) < 3 || rec[0] == "Registry" {
				continue
			}
			if p := strings.ToUpper(strings.TrimSpace(rec[1])); p != "" {
				vendors[p] = strings.TrimSpace(rec[2])
			}
		}
		f.Close()
	}
}

// hex is the address without separators, upper case ("" when it is not a hardware address).
func hex(mac string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(mac) {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			b.WriteRune(c)
		case c == ':' || c == '-' || c == '.':
		default:
			return ""
		}
	}
	if b.Len() != 12 {
		return ""
	}
	return b.String()
}

// Private says whether the address is one a device made up for itself (the "locally administered" bit): phones and laptops do this on
// purpose, to be harder to follow from network to network, so it names no maker.
func Private(mac string) bool {
	h := hex(mac)
	if h == "" {
		return false
	}
	first := (strings.IndexByte("0123456789ABCDEF", h[0])<<4 | strings.IndexByte("0123456789ABCDEF", h[1]))
	return first&0x02 != 0
}

// Vendor is the maker of the device with this hardware address, "" when it is not known (or the address is a private one).
func Vendor(mac string) string {
	h := hex(mac)
	if h == "" || Private(mac) {
		return ""
	}
	once.Do(load)
	for _, n := range []int{9, 7, 6} { // the most specific assignment first
		if v, ok := vendors[h[:n]]; ok {
			return v
		}
	}
	return ""
}

// Reset forgets what was read (the tests change Dir).
func Reset() { once = sync.Once{}; vendors = nil }
