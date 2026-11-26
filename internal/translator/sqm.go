package translator

import (
	"fmt"

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
	return fmt.Sprintf("# Written by PeakRoute from Internet, Smart queue management. Changes made here are lost.\n"+
		"SQM_DEV=%s\nSQM_DOWN_KBIT=%d\nSQM_UP_KBIT=%d\nSQM_LINK=%s\nSQM_PERHOST=%d\n",
		cfg.WanDevice(), s.DownloadKbit, s.UploadKbit, s.Link, perHost), true
}

// ensureSafeSQM is the render-time twin of the validation: what goes into the file is read by a shell.
func ensureSafeSQM(cfg config.NetworkConfig) error {
	s := cfg.SQMSettings()
	if s.DownloadKbit < 0 || s.DownloadKbit > config.MaxSQMKbit || s.UploadKbit < 0 || s.UploadKbit > config.MaxSQMKbit {
		return fmt.Errorf("refusing to render the shaping speeds %d/%d", s.DownloadKbit, s.UploadKbit)
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
