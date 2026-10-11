package network

import "testing"

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
	if st := ParseTimedatectlShow("garbage\n=x\nNTP\n"); st != (TimeStatus{}) {
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
