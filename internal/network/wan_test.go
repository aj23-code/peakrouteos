package network

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"peakroute/internal/config"
)

func reader(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("not found")
	}
}

func strs[T interface{ String() string }](in []T) []string {
	out := []string{}
	for _, a := range in {
		out = append(out, a.String())
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseResolvConf(t *testing.T) {
	text := "# comment\nsearch lan\nnameserver 192.168.0.1\nnameserver   8.8.8.8  \nnameserver 2001:4860:4860::8888\nnameserver\nnameserver not-an-ip\n;nameserver 9.9.9.9\n options edns0\n"
	if got := strs(ParseResolvConf([]byte(text))); !eq(got, []string{"192.168.0.1", "8.8.8.8"}) {
		t.Fatalf("%v", got)
	}
}

func TestSystemResolversSkipsLocalStub(t *testing.T) {
	// the stub in resolv.conf is not upstream: the real servers are in the systemd-resolved file
	files := map[string]string{
		"/etc/resolv.conf":                 "nameserver 127.0.0.53\n",
		"/run/systemd/resolve/resolv.conf": "nameserver 10.0.0.1\nnameserver 10.0.0.2\n",
	}
	if got := strs(SystemResolvers(reader(files))); !eq(got, []string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatalf("%v", got)
	}
	// a real server in resolv.conf wins, and a stub listed beside it is dropped
	files["/etc/resolv.conf"] = "nameserver 127.0.0.1\nnameserver 1.1.1.1\n"
	if got := strs(SystemResolvers(reader(files))); !eq(got, []string{"1.1.1.1"}) {
		t.Fatalf("%v", got)
	}
	// nothing usable anywhere
	if got := SystemResolvers(reader(map[string]string{"/etc/resolv.conf": "nameserver 127.0.0.53\n"})); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := SystemResolvers(reader(nil)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestUpstreamResolversByMode(t *testing.T) {
	files := reader(map[string]string{"/etc/resolv.conf": "nameserver 192.168.0.1\n"})

	got, src := UpstreamResolvers(config.NetworkConfig{WanMode: "static", WanDNS: []string{"9.9.9.9", "bogus", "149.112.112.112"}}, files)
	if !eq(strs(got), []string{"9.9.9.9", "149.112.112.112"}) || src != "DNS server you configured" {
		t.Fatalf("static: %v %q", got, src)
	}
	// static mode never falls back to the machine's own resolvers
	if got, _ := UpstreamResolvers(config.NetworkConfig{WanMode: "STATIC"}, files); len(got) != 0 {
		t.Fatalf("static without DNS used %v", got)
	}
	for _, mode := range []string{"dhcp", "", "pppoe"} {
		// no servers set by hand: the provider's
		got, src = UpstreamResolvers(config.NetworkConfig{WanMode: mode}, files)
		if !eq(strs(got), []string{"192.168.0.1"}) || src != "DNS server from your provider" {
			t.Fatalf("mode %q: %v %q", mode, got, src)
		}
		// servers set by hand replace the provider's, with every connection type
		got, src = UpstreamResolvers(config.NetworkConfig{WanMode: mode, WanDNS: []string{"9.9.9.9", "bogus"}}, files)
		if !eq(strs(got), []string{"9.9.9.9"}) || src != "DNS server you configured" {
			t.Fatalf("mode %q with an override: %v %q", mode, got, src)
		}
		// an override that is all junk does not leave the connection without any
		got, src = UpstreamResolvers(config.NetworkConfig{WanMode: mode, WanDNS: []string{"bogus"}}, files)
		if !eq(strs(got), []string{"192.168.0.1"}) || src != "DNS server from your provider" {
			t.Fatalf("mode %q with a junk override: %v %q", mode, got, src)
		}
	}
}

func TestDefaultGateway(t *testing.T) {
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth1\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"wlan0\t00000000\t010011AC\t0003\t0\t0\t600\t00000000\t0\t0\t0\n"
	if got := DefaultGateway([]byte(route), "eth0"); got != "192.168.0.1" {
		t.Fatalf("eth0: %q", got)
	}
	if got := DefaultGateway([]byte(route), "wlan0"); got != "172.17.0.1" {
		t.Fatalf("wlan0: %q", got)
	}
	if got := DefaultGateway([]byte(route), "eth1"); got != "" { // a connected subnet, not a default route
		t.Fatalf("eth1: %q", got)
	}
	if got := DefaultGateway([]byte(route), "nope"); got != "" {
		t.Fatalf("missing: %q", got)
	}
	if got := DefaultGateway(nil, "eth0"); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

func TestUpstreamGateway(t *testing.T) {
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	files := reader(map[string]string{"/proc/net/route": route})

	if a, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanMode: "dhcp"}, files); !ok || a.String() != "192.168.0.1" {
		t.Fatalf("dhcp: %v %v", a, ok)
	}
	if a, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0"}, files); !ok || a.String() != "192.168.0.1" {
		t.Fatalf("empty mode counts as dhcp: %v %v", a, ok)
	}
	// static: the configured one, whatever the kernel says
	if a, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanMode: "static", WanGateway: "203.0.113.1"}, files); !ok || a.String() != "203.0.113.1" {
		t.Fatalf("static: %v %v", a, ok)
	}
	// static without a gateway does not borrow the kernel's
	if _, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanMode: "static"}, files); ok {
		t.Fatal("static without a gateway should have none")
	}
	// another port, a missing route table, junk
	if _, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth9"}, files); ok {
		t.Fatal("a port without a default route has no gateway")
	}
	if _, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0"}, reader(nil)); ok {
		t.Fatal("no route table")
	}
	if _, ok := UpstreamGateway(config.NetworkConfig{WanMode: "static", WanGateway: "not-an-ip"}, files); ok {
		t.Fatal("junk gateway accepted")
	}
}

func TestParseRoutingTable(t *testing.T) {
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
		"eth1\t0000140A\t3201A8C0\t0003\t0\t0\t50\t0000FFFF\t0\t0\t0\n" +
		"eth1\t0000FEA9\t00000000\t0001\t0\t0\t1000\t0000FFFF\t0\t0\t0\n" +
		"junk line\n" +
		"eth2\tZZZZZZZZ\t00000000\t0001\t0\t0\t1\t00FFFFFF\t0\t0\t0\n" +
		"eth3\t0000A8C0\t00000000\t0001\t0\t0\t1\t00FF00FF\t0\t0\t0\n" // a mask with a hole
	got := ParseRoutingTable([]byte(route))
	want := []KernelRoute{
		{"0.0.0.0/0", "192.168.0.1", "eth0", 100},
		{"192.168.0.0/24", "", "eth0", 100},
		{"10.20.0.0/16", "192.168.1.50", "eth1", 50},
		{"169.254.0.0/16", "", "eth1", 1000},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(ParseRoutingTable(nil)) != 0 || len(ParseRoutingTable([]byte("Iface\tDestination\n"))) != 0 {
		t.Error("an empty table should give nothing")
	}
}

func TestIsWireless(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) { os.MkdirAll(filepath.Join(root, p), 0755) }
	mk("eth0")
	mk("wlan0/wireless")
	mk("wlx1/other")
	os.Symlink("../../ieee80211/phy1", filepath.Join(root, "wlx1", "phy80211"))
	for name, want := range map[string]bool{"eth0": false, "wlan0": true, "wlx1": true, "nope": false} {
		if got := IsWireless(root, name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

func TestUpstreamGatewayFollowsTheDeviceThatCarriesTheConnection(t *testing.T) {
	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0.835\t00000000\t010011AC\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"ppp0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"
	files := reader(map[string]string{"/proc/net/route": route})

	if a, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanVlan: 835}, files); !ok || a.String() != "172.17.0.1" {
		t.Fatalf("vlan: %v %v", a, ok)
	}
	saved := PPPPeer
	defer func() { PPPPeer = saved }()
	var asked string
	PPPPeer = func(dev string) string { asked = dev; return "10.64.64.64" }
	if a, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanMode: "pppoe"}, files); !ok || a.String() != "10.64.64.64" || asked != "ppp0" {
		t.Fatalf("pppoe: %v %v (asked %q)", a, ok, asked)
	}
	PPPPeer = func(string) string { return "" }
	if _, ok := UpstreamGateway(config.NetworkConfig{WanInterface: "eth0", WanMode: "pppoe"}, files); ok {
		t.Fatal("a PPPoE session that is not up has no gateway")
	}
	// a plain connection never asks for a peer
	PPPPeer = func(string) string { t.Fatal("asked for a peer"); return "" }
	UpstreamGateway(config.NetworkConfig{WanInterface: "eth9"}, files) // no default route there either
}

func TestParsePeer(t *testing.T) {
	cases := map[string]string{
		"5: ppp0    inet 10.0.0.2 peer 10.64.64.64/32 scope global ppp0\\       valid_lft forever preferred_lft forever\n": "10.64.64.64",
		"5: ppp0 inet 10.0.0.2 peer 10.64.64.64 scope global ppp0":                                                         "10.64.64.64",
		"2: eth0    inet 192.168.1.1/24 brd 192.168.1.255 scope global eth0":                                               "",
		"inet 10.0.0.2 peer 2001:db8::1/128 scope global":                                                                  "",
		"inet 10.0.0.2 peer":                       "",
		"inet 10.0.0.2 peer nonsense scope global": "",
		"": "",
	}
	for in, want := range cases {
		if got := ParsePeer(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
