package wireless

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

const surveyText = `Survey data from wlan0
	frequency:			2412 MHz
	noise:				-95 dBm
	channel active time:		1260 ms
	channel busy time:		32 ms
	channel receive time:		26 ms
	channel transmit time:		3 ms
Survey data from wlan0
	frequency:			5180 MHz [in use]
	noise:				-92 dBm
	channel active time:		200000 ms
	channel busy time:		50000 ms
	channel receive time:		40000 ms
	channel transmit time:		10000 ms
`

func TestParsingASurvey(t *testing.T) {
	got := ParseSurvey(surveyText)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got[0].InUse || got[0].MHz != 2412 || got[0].NoiseDBm != -95 || got[0].BusyMS != 32 {
		t.Fatalf("%+v", got[0])
	}
	s := got[1]
	if !s.InUse || s.MHz != 5180 || !s.HaveNoise || s.NoiseDBm != -92 || s.ActiveMS != 200000 || s.BusyMS != 50000 || s.RxMS != 40000 || s.TxMS != 10000 {
		t.Fatalf("%+v", s)
	}
	if len(ParseSurvey("")) != 0 || len(ParseSurvey("command failed: Operation not permitted (-1)")) != 0 {
		t.Fatal("garbage")
	}
}

func TestTheSurveyOfTheChannelInUse(t *testing.T) {
	d := Detector{Run: func(_ context.Context, name string, args ...string) (string, error) {
		if strings.Join(args, " ") != "dev wlan0 survey dump" {
			return "", errors.New("unexpected")
		}
		return surveyText, nil
	}}
	s, ok := d.Survey(context.Background(), "wlan0")
	if !ok || s.MHz != 5180 {
		t.Fatalf("%+v %v", s, ok)
	}
	// no channel in use (the adapter is down), and a failure
	d.Run = func(context.Context, string, ...string) (string, error) {
		return strings.Split(surveyText, "Survey data from wlan0\n\tfrequency:\t\t\t5180")[0], nil
	}
	if _, ok := d.Survey(context.Background(), "wlan0"); ok {
		t.Fatal("a channel that is not in use")
	}
	d.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("x") }
	if _, ok := d.Survey(context.Background(), "wlan0"); ok {
		t.Fatal("a failure")
	}
	if _, ok := (Detector{}).Survey(context.Background(), "wlan0"); ok {
		t.Fatal("no runner")
	}
}

const linkText = `Connected to 02:00:00:00:00:01 (on wlan1)
	SSID: Roof Link
	freq: 5180.0
	RX: 519175559 bytes (620086 packets)
	TX: 115222803 bytes (318030 packets)
	signal: -46 dBm
	rx bitrate: 433.3 MBit/s VHT-MCS 9 80MHz short GI VHT-NSS 1
	tx bitrate: 866.7 MBit/s VHT-MCS 9 80MHz short GI VHT-NSS 2
	bss flags: short-slot-time
`

func TestParsingALink(t *testing.T) {
	l := ParseLink(linkText)
	if !l.Connected || l.BSSID != "02:00:00:00:00:01" || l.SSID != "Roof Link" || l.MHz != 5180 || l.Band != "5 GHz" || l.SignalDBm == nil || *l.SignalDBm != -46 || l.RxMbps != 433.3 || l.TxMbps != 866.7 {
		t.Fatalf("%+v", l)
	}
	if l := ParseLink("Not connected.\n"); l.Connected || l.SignalDBm != nil {
		t.Fatalf("%+v", l)
	}
	d := Detector{Run: func(context.Context, string, ...string) (string, error) { return linkText, nil }}
	if l, ok := d.Link(context.Background(), "wlan1"); !ok || !l.Connected {
		t.Fatalf("%+v %v", l, ok)
	}
	d.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("x") }
	if _, ok := d.Link(context.Background(), "wlan1"); ok {
		t.Fatal("a failure")
	}
}

func TestTheRealIwLinkFormatIsUnderstood(t *testing.T) {
	if _, err := exec.LookPath("iw"); err != nil {
		t.Skip("iw is not installed")
	}
	out, _ := exec.Command("iw", "dev").Output()
	var iface string
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "Interface" {
			iface = f[1]
		}
	}
	if iface == "" {
		t.Skip("no wireless interface here")
	}
	link, err := exec.Command("iw", "dev", iface, "link").Output()
	if err != nil {
		t.Skip("the link cannot be read here")
	}
	l := ParseLink(string(link))
	if l.Connected && (l.SSID == "" || l.MHz == 0 || l.SignalDBm == nil) {
		t.Errorf("an incomplete link: %+v", l)
	}
}

func TestTransmitPowerOfAnInterface(t *testing.T) {
	list := ParseDev("phy#1\n\tInterface wlan1\n\t\ttype AP\n\t\tchannel 36 (5180 MHz), width: 80 MHz, center1: 5210 MHz\n\t\ttxpower 23.00 dBm\n\tInterface wlan2\n\t\ttype AP\n")
	if len(list) != 2 || !list[0].HaveTxPower || list[0].TxPowerDBM != 23 || list[0].Width != 80 || list[1].HaveTxPower {
		t.Fatalf("%+v", list)
	}
}
