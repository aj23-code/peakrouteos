package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// Smart queue management (docs/sqm.md). The shaping itself is `/usr/lib/peakroute/sqm` (in the image); the translator only writes the
// file it reads while shaping is on, and removes it when it is off. The action runs `sqm reconcile`: with the file the kernel is made to
// match it, without it the shaping is taken away. Interface hooks (if-up.d, ppp ip-up.d) run `sqm start-if` so that a connection that
// comes up again (a PPPoE session is a new interface every time) is shaped again.
const (
	PathSQM   = "/etc/default/peakroute-sqm"
	PathSQMGo = "/usr/lib/peakroute/sqm"
	SvcSQM    = "sqm"
)

// renderSQM writes the file the helper reads; ok is false while no shaping is wanted.
func renderSQM(cfg config.NetworkConfig) (string, bool) {
	if !cfg.SQMActive() {
		return "", false
	}
	s := cfg.SQMSettings()
	perHost := 0
	if s.PerHost {
		perHost = 1
	}
	out := fmt.Sprintf("# Written by PeakRoute from Internet, Smart queue management. Changes made here are lost.\n"+
		"# (the speeds are what the router shapes to: the line's speeds times the percentage)\n"+
		"SQM_DEV=%s\nSQM_DOWN_KBIT=%d\nSQM_UP_KBIT=%d\nSQM_LINK=%s\nSQM_PERHOST=%d\n",
		cfg.WanDevice(), s.ShapedDownKbit(), s.ShapedUpKbit(), s.Link, perHost)
	if cfg.QoSActive() {
		// the line is divided between the networks: one entry for each, "mark class priority up-rate up-ceil down-rate down-ceil" (kbit/s).
		// The mark is what the firewall puts on a network's connections (qosMarkTable); the class is the queue's, in hex.
		var b strings.Builder
		b.WriteString("# a share of the line for each network: mark, class, priority, upload rate and ceiling, download rate and ceiling (kbit/s)\nSQM_CLASSES=\"\n")
		for _, cl := range cfg.QoSClasses() {
			fmt.Fprintf(&b, "0x%x %x %d %d %d %d %d\n", qosMark(cl.Index), qosMinor(cl.Index), cl.Prio, cl.UpRate, cl.UpCeil, cl.DownRate, cl.DownCeil)
		}
		b.WriteString("\"\n")
		out += b.String()
	}
	return out, true
}

// The mark of a network's connections is its index in the bits 16 to 23 of the connection mark (qosMask), so that it cannot clash with a
// mark something else sets; the queue's class for it is 1:<minor>, where "everything else" (index 0) is minor 2.
const qosMask = 0x00ff0000

func qosMark(index int) int { return index << 16 }

func qosMinor(index int) int {
	if index == 0 {
		return 2
	}
	return 0x10 + index
}

// qosMarkTable marks the connections of each network (the ones it starts, and the ones started towards it, such as a forwarded port), and
// puts the connection's mark on each packet that is forwarded, which is what the queue sorts by when the packet goes out. The packets
// that come in are sorted by the same connection mark (the queue reads it from the connection tracking). Empty while the line is not divided.
func qosMarkTable(cfg config.NetworkConfig, lan string) string {
	if !cfg.QoSActive() {
		return ""
	}
	var b strings.Builder
	b.WriteString("\ntable inet peakroute_qos {\n    chain classify {\n        type filter hook forward priority mangle; policy accept;\n")
	for _, cl := range cfg.QoSClasses() {
		if cl.ID == "other" {
			continue
		}
		iface := lan
		if cl.ID != config.DefaultNetworkID {
			n, _ := cfg.FindNetwork(cl.ID)
			iface = networkIface(n)
		}
		for _, dir := range []string{"iifname", "oifname"} {
			fmt.Fprintf(&b, "        %s \"%s\" ct mark and 0x%08x == 0 ct mark set ct mark and 0x%08x or 0x%08x\n", dir, iface, qosMask, ^qosMask&0xffffffff, qosMark(cl.Index))
		}
	}
	fmt.Fprintf(&b, "        meta mark set ct mark and 0x%08x\n    }\n}\n", qosMask)
	return b.String()
}

// ensureSafeSQM is the render-time twin of the validation: what goes into the file is read by a shell.
func ensureSafeSQM(cfg config.NetworkConfig) error {
	s := cfg.SQMSettings()
	if s.DownloadKbit < 0 || s.DownloadKbit > config.MaxSQMKbit || s.UploadKbit < 0 || s.UploadKbit > config.MaxSQMKbit {
		return fmt.Errorf("refusing to render the shaping speeds %d/%d", s.DownloadKbit, s.UploadKbit)
	}
	if s.Percent != 0 && (s.Percent < config.MinSQMPercent || s.Percent > config.MaxSQMPercent) {
		return fmt.Errorf("refusing to render the shaping percentage %d", s.Percent)
	}
	if !config.ValidSQMLink(s.Link) {
		return fmt.Errorf("refusing to render the connection kind %q", s.Link)
	}
	if cfg.SQMActive() {
		return ensureSafe(cfg.WanDevice())
	}
	return nil
}

// sqmCommand makes the kernel match the file. An image from before this feature has no helper: nothing to take away, and a request to
// shape says so.
var sqmCommand = command{"sh", []string{"-c", `if [ -x ` + PathSQMGo + ` ]; then exec ` + PathSQMGo + ` reconcile; ` +
	`elif [ -f ` + PathSQM + ` ]; then echo "smart queue management is not in this system image: update to a version that has it" >&2; exit 1; fi`}}

// sqmNote is shown in the review when the settings cannot do what they say.
func sqmNotes(cfg config.NetworkConfig) []string {
	if !cfg.SQMActive() {
		return nil
	}
	var notes []string
	if cfg.WanKind() == config.WanPPPoE && cfg.SQMSettings().Link == config.SQMEthernet {
		notes = append(notes, "This connection is PPPoE: if it is DSL, pick VDSL or ADSL as the kind of line so that its overhead is counted.")
	}
	return notes
}
