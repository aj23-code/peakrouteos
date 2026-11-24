package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTimedatectlShow(t *testing.T) {
	st := ParseTimedatectlShow("Timezone=Europe/Berlin\nLocalRTC=no\nCanNTP=yes\nNTP=yes\nNTPSynchronized=yes\nTimeUSec=Sun 2026-10-11 16:44:03 CEST\n")
	if st.Timezone != "Europe/Berlin" || !st.NTPEnabled || !st.Synchronized {
		t.Fatalf("%+v", st)
	}
	st = ParseTimedatectlShow("Timezone=UTC\nNTP=no\nNTPSynchronized=no\n")
	if st.Timezone != "UTC" || st.NTPEnabled || st.Synchronized {
		t.Fatalf("%+v", st)
	}
	// the answer of a system that says nothing useful
	if st := ParseTimedatectlShow("garbage\n=x\nNTP\n"); st.Timezone != "" || st.NTPEnabled || st.Synchronized || st.Server != "" {
		t.Fatalf("%+v", st)
	}
	// CanNTP must not be read as NTP
	if st := ParseTimedatectlShow("CanNTP=yes\nNTP=no\n"); st.NTPEnabled {
		t.Fatal("CanNTP counted as NTP")
	}
}

func TestParseTimesyncServer(t *testing.T) {
	out := "       Server: 162.159.200.1 (time.cloudflare.com)\nPoll interval: 2min 8s (min: 32s; max 34min 8s)\n         Leap: normal\n      Version: 4\n      Stratum: 3\n"
	if got := ParseTimesyncServer(out); got != "162.159.200.1 (time.cloudflare.com)" {
		t.Fatalf("%q", got)
	}
	for _, none := range []string{"", "Failed to query server: Connection timed out\n", "Poll interval: 1s\n"} {
		if got := ParseTimesyncServer(none); got != "" {
			t.Errorf("%q gave %q", none, got)
		}
	}
}

func TestTimesyncStatusNamesTheServerItsAddressAndStratum(t *testing.T) {
	out := "       Server: 162.159.200.1 (time.cloudflare.com)\nPoll interval: 32s (min: 32s; max 34min 8s)\n         Leap: normal\n      Version: 4\n      Stratum: 3\n"
	server, ip, stratum := ParseTimesyncStatus(out)
	if server != "162.159.200.1 (time.cloudflare.com)" || ip.String() != "162.159.200.1" || stratum != 3 {
		t.Errorf("%q %v %d", server, ip, stratum)
	}
	if s, ip, n := ParseTimesyncStatus("Server: 2606:4700:f1::1 (time.cloudflare.com)\n"); s == "" || ip.String() != "2606:4700:f1::1" || n != 0 {
		t.Errorf("%q %v %d", s, ip, n)
	}
	if s, ip, n := ParseTimesyncStatus("nothing useful"); s != "" || ip != nil || n != 0 {
		t.Errorf("%q %v %d", s, ip, n)
	}
}

func TestAMachineWithoutAClockChipIsReportedAsSuch(t *testing.T) {
	root := t.TempDir()
	if HasHardwareClock(root) {
		t.Error("an empty machine has a clock chip")
	}
	if err := os.MkdirAll(filepath.Join(root, "sys/class/rtc"), 0755); err != nil {
		t.Fatal(err)
	}
	if HasHardwareClock(root) {
		t.Error("the class directory alone is not a chip")
	}
	if err := os.Mkdir(filepath.Join(root, "sys/class/rtc/rtc0"), 0755); err != nil {
		t.Fatal(err)
	}
	if !HasHardwareClock(root) {
		t.Error("rtc0 is a chip")
	}
}
