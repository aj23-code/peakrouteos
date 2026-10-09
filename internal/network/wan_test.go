package network

import (
	"errors"
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
	for _, mode := range []string{"dhcp", ""} {
		got, src = UpstreamResolvers(config.NetworkConfig{WanMode: mode, WanDNS: []string{"9.9.9.9"}}, files)
		if !eq(strs(got), []string{"192.168.0.1"}) || src != "DNS server from your provider" {
			t.Fatalf("mode %q: %v %q", mode, got, src)
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
