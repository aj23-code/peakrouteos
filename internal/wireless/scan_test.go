package wireless

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func scanFixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/iw-scan.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParsingAScan(t *testing.T) {
	got := ParseScan(scanFixture(t))
	byName := map[string][]Network{}
	for _, n := range got {
		byName[n.SSID] = append(byName[n.SSID], n)
	}
	// strongest first
	for i := 1; i < len(got); i++ {
		if got[i].Signal > got[i-1].Signal {
			t.Fatalf("not sorted by signal: %+v", got)
		}
	}
	// a name on two bands with different security is two entries; the same name and security is one, the strongest
	home := byName["Home Network"]
	if len(home) != 2 {
		t.Fatalf("%+v", home)
	}
	var mixed, wpa2 Network
	for _, n := range home {
		if n.Security == SecWPA2WPA3 {
			mixed = n
		} else {
			wpa2 = n
		}
	}
	if mixed.Signal != -45 || mixed.Band != "5 GHz" || !mixed.Connected || !mixed.Supported {
		t.Fatalf("%+v", mixed)
	}
	if wpa2.Security != SecWPA2 || wpa2.Signal != -61 || wpa2.Band != "2.4 GHz" || wpa2.Connected || !wpa2.Supported {
		t.Fatalf("%+v", wpa2)
	}
	want := map[string]struct {
		sec       string
		supported bool
		band      string
	}{
		"Strict WPA3":   {SecWPA3, true, "2.4 GHz"},
		"Old Neighbour": {SecWPA2, true, "2.4 GHz"}, // WPA1 with a password: joined as WPA2
		"Cafe Guest":    {SecOpen, false, "5 GHz"},
		"Office":        {SecEnterprise, false, "2.4 GHz"},
		"Ancient WEP":   {SecWEP, false, "2.4 GHz"},
		"Café Bar":      {SecWPA2, true, "2.4 GHz"}, // iw prints UTF-8 as \xNN
	}
	for name, w := range want {
		list := byName[name]
		if len(list) != 1 || list[0].Security != w.sec || list[0].Supported != w.supported || list[0].Band != w.band {
			t.Errorf("%s: %+v, want %+v", name, list, w)
		}
	}
	// what is not listed: networks without a name, and 6 GHz
	for _, n := range got {
		if strings.TrimSpace(strings.Trim(n.SSID, "\x00")) == "" || n.SSID == "Six Gig" {
			t.Errorf("listed: %+v", n)
		}
	}
	if len(got) != 8 {
		t.Fatalf("%d networks: %+v", len(got), got)
	}
}

func TestParsingNothing(t *testing.T) {
	for _, in := range []string{"", "\n", "command failed: Network is down (-100)\n", "BSS"} {
		if got := ParseScan(in); len(got) != 0 {
			t.Errorf("%q: %+v", in, got)
		}
	}
}

func TestTheRealIwScanFormatIsUnderstood(t *testing.T) {
	// the real thing, if there is a cached scan on this machine
	if _, err := exec.LookPath("iw"); err != nil {
		t.Skip("iw is not installed")
	}
	out, err := exec.Command("iw", "dev").Output()
	if err != nil || !strings.Contains(string(out), "Interface ") {
		t.Skip("no wireless interface here")
	}
	var iface string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "Interface" {
			iface = f[1]
		}
	}
	dump, err := exec.Command("iw", "dev", iface, "scan", "dump").Output()
	if err != nil || !strings.Contains(string(dump), "BSS ") {
		t.Skip("no scan results to read here")
	}
	for _, n := range ParseScan(string(dump)) {
		if n.SSID == "" || n.Band == "" || n.Security == "" {
			t.Errorf("an incomplete entry: %+v", n)
		}
	}
}

type scanRunner struct {
	calls []string
	reply map[string]struct {
		out string
		err error
	}
}

func (r *scanRunner) run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	if x, ok := r.reply[line]; ok {
		return x.out, x.err
	}
	return "", errors.New("no reply for " + line)
}

func reply(out string, err error) struct {
	out string
	err error
} {
	return struct {
		out string
		err error
	}{out, err}
}

func TestScanningTriesAScanThenWhatTheKernelRemembers(t *testing.T) {
	fix := scanFixture(t)
	// a scan works
	r := &scanRunner{reply: map[string]struct {
		out string
		err error
	}{"iw dev wlan0 scan": reply(fix, nil)}}
	nets, cached, err := Detector{Run: r.run}.Scan(context.Background(), "wlan0")
	if err != nil || cached || len(nets) != 8 {
		t.Fatalf("%v %v %d", err, cached, len(nets))
	}
	// refused (not root): the remembered results, said to be so
	r = &scanRunner{reply: map[string]struct {
		out string
		err error
	}{"iw dev wlan0 scan": reply("", errors.New("exit status 240")), "iw dev wlan0 scan dump": reply(fix, nil)}}
	nets, cached, err = Detector{Run: r.run}.Scan(context.Background(), "wlan0")
	if err != nil || !cached || len(nets) != 8 {
		t.Fatalf("%v %v %d", err, cached, len(nets))
	}
	// neither
	r = &scanRunner{reply: map[string]struct {
		out string
		err error
	}{"iw dev wlan0 scan": reply("", errors.New("x")), "iw dev wlan0 scan dump": reply("", errors.New("y"))}}
	if _, _, err := (Detector{Run: r.run}).Scan(context.Background(), "wlan0"); !errors.Is(err, ErrScanUnavailable) {
		t.Fatalf("%v", err)
	}
	if _, _, err := (Detector{}).Scan(context.Background(), "wlan0"); !errors.Is(err, ErrScanUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestAnAdapterThatIsDownIsSwitchedUpForTheScan(t *testing.T) {
	fix := scanFixture(t)
	down := &exec.ExitError{Stderr: []byte("command failed: Network is down (-100)")}
	calls := 0
	run := func(_ context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		switch line {
		case "iw dev wlan0 scan":
			calls++
			if calls == 1 {
				return "", down
			}
			return fix, nil
		case "ip link set dev wlan0 up":
			return "", nil
		}
		return "", errors.New("unexpected " + line)
	}
	nets, cached, err := Detector{Run: run}.Scan(context.Background(), "wlan0")
	if err != nil || cached || len(nets) != 8 || calls != 2 {
		t.Fatalf("%v %v %d %d", err, cached, len(nets), calls)
	}
}
