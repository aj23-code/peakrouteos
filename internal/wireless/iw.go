// Package wireless finds the machine's Wi-Fi adapters and reads what each one can do (the bands,
// channels and channel widths it supports, and whether it can act as an access point). It only
// reads: nothing here changes the radio.
package wireless

import (
	"regexp"
	"strconv"
	"strings"
)

// Channel is one channel of a band, with what the regulatory rules of the current country say about it.
type Channel struct {
	Number   int  `json:"number"`
	MHz      int  `json:"mhz"`
	Disabled bool `json:"disabled"` // not allowed at all here
	NoIR     bool `json:"no_ir"`    // may be listened on but not started on: no access point can use it
	DFS      bool `json:"dfs"`      // shares the spectrum with radar: an access point must wait and listen first
}

// Usable reports whether an access point can be started on the channel.
func (c Channel) Usable() bool { return !c.Disabled && !c.NoIR }

// Band is a frequency band an adapter supports.
type Band struct {
	Name     string    `json:"name"`   // "2.4 GHz", "5 GHz" or "6 GHz"
	Widths   []int     `json:"widths"` // channel widths in MHz the adapter can use in this band
	Channels []Channel `json:"channels"`
}

// Phy is what `iw phy <name> info` says about an adapter.
type Phy struct {
	Modes []string // interface modes: managed, AP, monitor, ...
	Bands []Band
}

// CanAP reports whether the adapter can act as an access point.
func (p Phy) CanAP() bool {
	for _, m := range p.Modes {
		if m == "AP" {
			return true
		}
	}
	return false
}

var (
	bandHeader  = regexp.MustCompile(`^\tBand \d+:`)
	modeLine    = regexp.MustCompile(`^\t\t \* (.+)$`)
	freqLine    = regexp.MustCompile(`^\t\t\t\* (\d+(?:\.\d+)?) MHz \[(\d+)\](.*)$`)
	modesHeader = "Supported interface modes:"
)

func bandName(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2.4 GHz"
	case mhz >= 4900 && mhz < 5900:
		return "5 GHz"
	case mhz >= 5925 && mhz <= 7125:
		return "6 GHz"
	}
	return ""
}

// ParsePhyInfo reads the text `iw phy <name> info` prints. Bands that are not Wi-Fi bands the page
// can use (60 GHz, say) are left out.
func ParsePhyInfo(text string) Phy {
	var phy Phy
	lines := strings.Split(text, "\n")

	// the interface modes
	for i, l := range lines {
		if strings.TrimSpace(l) != modesHeader {
			continue
		}
		for _, next := range lines[i+1:] {
			m := modeLine.FindStringSubmatch(next)
			if m == nil {
				break
			}
			phy.Modes = append(phy.Modes, m[1])
		}
		break
	}

	// the bands: each starts at "\tBand N:" and runs to the next line with a single tab of indent
	for i := 0; i < len(lines); i++ {
		if !bandHeader.MatchString(lines[i]) {
			continue
		}
		end := i + 1
		for end < len(lines) && (lines[end] == "" || strings.HasPrefix(lines[end], "\t\t")) {
			end++
		}
		if b, ok := parseBand(lines[i+1 : end]); ok {
			phy.Bands = append(phy.Bands, b)
		}
		i = end - 1
	}
	return phy
}

func parseBand(lines []string) (Band, bool) {
	var b Band
	var ht40, vht, vht160, he, he160 bool
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.Contains(t, "HT20/HT40"):
			ht40 = true
		case strings.HasPrefix(t, "VHT Capabilities"):
			vht = true
		case strings.HasPrefix(t, "Supported Channel Width:"):
			// "neither 160 nor 80+80" says no; anything that names 160 MHz says yes
			vht160 = vht160 || (strings.Contains(t, "160") && !strings.Contains(t, "neither 160"))
		case strings.HasPrefix(t, "HE MAC Capabilities"), strings.HasPrefix(t, "HE Iftypes"):
			he = true
		case strings.Contains(t, "HE160"), strings.Contains(t, "HE 160"):
			he160 = true
		}

		m := freqLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		f, _ := strconv.ParseFloat(m[1], 64)
		n, _ := strconv.Atoi(m[2])
		ch := Channel{Number: n, MHz: int(f + 0.5)}
		flags := m[3]
		ch.Disabled = strings.Contains(flags, "disabled")
		ch.NoIR = strings.Contains(flags, "no IR")
		ch.DFS = strings.Contains(flags, "radar detection")
		b.Channels = append(b.Channels, ch)
	}
	if len(b.Channels) == 0 {
		return Band{}, false
	}
	b.Name = bandName(b.Channels[0].MHz)
	if b.Name == "" {
		return Band{}, false
	}

	b.Widths = []int{20}
	if ht40 {
		b.Widths = append(b.Widths, 40)
	}
	if b.Name != "2.4 GHz" && (vht || he) {
		b.Widths = append(b.Widths, 80)
		if vht160 || he160 {
			b.Widths = append(b.Widths, 160)
		}
	}
	return b, true
}

// ParseCountry reads the first "country XX" line of `iw reg get`: the regulatory domain in effect.
// "00" is the world domain, which is what a machine with no country set uses.
func ParseCountry(text string) (string, bool) {
	for _, l := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "country "); ok {
			if code, _, found := strings.Cut(rest, ":"); found && len(code) == 2 {
				return code, true
			}
		}
	}
	return "", false
}
