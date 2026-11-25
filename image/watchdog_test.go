package image

import (
	"os"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The daemon says it is alive only while its web server answers; systemd needs to be told to expect that, and to take it from the daemon.
func TestTheDaemonServiceIsWatchedByTheServiceManager(t *testing.T) {
	u := read(t, "overlay/etc/systemd/system/peakroute.service")
	for _, want := range []string{"WatchdogSec=90", "NotifyAccess=main", "Restart=always"} {
		if !strings.Contains(u, want) {
			t.Errorf("peakroute.service lacks %s", want)
		}
	}
}

func TestTheMachineHasAHardwareWatchdogAndRestartsAfterAPanic(t *testing.T) {
	if w := read(t, "overlay/etc/systemd/system.conf.d/watchdog.conf"); !strings.Contains(w, "RuntimeWatchdogSec=30s") {
		t.Errorf("%s", w)
	}
	s := read(t, "overlay/etc/sysctl.d/99-peakroute.conf")
	for _, want := range []string{"kernel.panic = 10", "kernel.panic_on_oops = 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("the sysctl file lacks %s", want)
		}
	}
}

func TestTheDHCPAndDNSServerComesBackAfterADeath(t *testing.T) {
	d := read(t, "overlay/etc/systemd/system/dnsmasq.service.d/peakroute-restart.conf")
	for _, want := range []string{"Restart=on-failure", "StartLimitIntervalSec=0"} {
		if !strings.Contains(d, want) {
			t.Errorf("dnsmasq's drop-in lacks %s", want)
		}
	}
}
