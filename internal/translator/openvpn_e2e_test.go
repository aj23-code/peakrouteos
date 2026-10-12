package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/ovpn"
	"peakroute/internal/pki"
)

// The real OpenVPN lab: four network namespaces (this router, the internet, a branch router and a device, and a host on this router's
// network), the real openvpn, the files the translator writes, the script that applies them (with a stand-in for systemctl that runs
// openvpn in the background) and the rules it writes for the firewall. It checks what the unit tests cannot: that OpenVPN accepts
// the files, that the certificates are accepted and refused as intended, that ccd files and the revocation list decide who gets in,
// and that traffic flows or is held back as the zones say.

func openvpnBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("openvpn")
	if err != nil {
		t.Skip("openvpn is not installed here")
	}
	return p
}

const ovLabScript = `set -u
D="$1"
export PATH="$D/bin:$PATH"
say() { echo "RESULT $1 $2"; }
mkdir -p "$D/bin" "$D/run" "$D/log" "$D/ostate" "$D/etc/openvpn/server" "$D/etc/openvpn/client" "$D/etc/openvpn/ccd"
cat > "$D/bin/systemctl" <<SHIM
#!/bin/sh
cmd=\$1; shift
[ "\${1:-}" = "--quiet" ] && shift
unit=\$1
kind=\${unit#openvpn-}; kind=\${kind%%@*}; inst=\${unit#*@}
pidf="$D/run/\$kind-\$inst.pid"
alive() { [ -s "\$pidf" ] && kill -0 "\$(cat "\$pidf")" 2>/dev/null; }
stop() { if alive; then p=\$(cat "\$pidf"); kill "\$p"; i=0; while kill -0 "\$p" 2>/dev/null && [ \$i -lt 50 ]; do sleep 0.1; i=\$((i+1)); done; fi; rm -f "\$pidf"; }
case "\$cmd" in
  is-active) alive ;;
  stop) stop ;;
  restart) stop; openvpn --daemon --writepid "\$pidf" --cd "$D/etc/openvpn/\$kind" --config "\$inst.conf" --log "$D/log/\$kind-\$inst.log" ;;
  *) exit 1 ;;
esac
SHIM
chmod +x "$D/bin/systemctl"
sed "s|/etc/openvpn|$D/etc/openvpn|g; s|/run/peakroute-ovpn|$D/ostate|g; s|^PATH=|PATH=$D/bin:|" "$D/sync.sh" > "$D/sync-local.sh"
ip link set lo up
mkns() { unshare -n sh -c "echo \$\$ > $D/$1.pid; exec sleep 600" & i=0; while [ ! -s "$D/$1.pid" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done; }
mkns inet; mkns b; mkns c; mkns h
pid() { cat "$D/$1.pid"; }
ns() { n="$1"; shift; nsenter -t "$(pid $n)" -n "$@"; }
trap 'for n in inet b c h; do kill "$(pid $n)" 2>/dev/null; done; for f in "$D"/run/*.pid; do [ -e "$f" ] && kill "$(cat "$f")" 2>/dev/null; done' EXIT
for n in inet b c h; do ns $n ip link set lo up; done
# the wires: A (here) - the internet - B and C; A - H on A's network
ip link add wan0 type veth peer name i_a netns "$(pid inet)"
ns inet ip link add i_b type veth peer name wan0 netns "$(pid b)"
ns inet ip link add i_c type veth peer name wan0 netns "$(pid c)"
ip link add lan0 type veth peer name h0 netns "$(pid h)"
ip addr add 192.0.2.1/24 dev wan0; ip link set wan0 up; ip route add default via 192.0.2.254
ns inet ip addr add 192.0.2.254/24 dev i_a; ns inet ip link set i_a up
ns inet ip addr add 198.51.100.254/24 dev i_b; ns inet ip link set i_b up
ns inet ip addr add 203.0.113.254/24 dev i_c; ns inet ip link set i_c up
ns inet sysctl -qw net.ipv4.ip_forward=1
ns b ip addr add 198.51.100.2/24 dev wan0; ns b ip link set wan0 up; ns b ip route add default via 198.51.100.254
ns b ip link add lan0 type dummy; ns b ip addr add 10.50.0.1/24 dev lan0; ns b ip link set lan0 up
ns c ip addr add 203.0.113.2/24 dev wan0; ns c ip link set wan0 up; ns c ip route add default via 203.0.113.254
ip addr add 192.168.1.1/24 dev lan0; ip link set lan0 up
ns h ip addr add 192.168.1.50/24 dev h0; ns h ip link set h0 up; ns h ip route add default via 192.168.1.1
sysctl -qw net.ipv4.ip_forward=1
ns b ping -c1 -W2 192.0.2.1 >/dev/null 2>&1 && say wires ok || say wires fail

# a set of files, put where the translator would have put them (the old ones go: the directories are the translator's)
install_set() {
	rm -rf "$D/etc/openvpn/server" "$D/etc/openvpn/client" "$D/etc/openvpn/ccd"
	mkdir -p "$D/etc/openvpn/server" "$D/etc/openvpn/client" "$D/etc/openvpn/ccd"
	for kind in server client; do
		for f in "$D/$1/$kind"/*.conf; do
			[ -e "$f" ] || continue
			# there is no user "nobody" with a group to drop to inside the lab's user namespace; the files are otherwise as written
			sed "s|/etc/openvpn|$D/etc/openvpn|g; /^user nobody\$/d; /^group nogroup\$/d" "$f" > "$D/etc/openvpn/$kind/$(basename "$f")"
		done
	done
	for f in "$D/$1/ccd"/*; do [ -e "$f" ] && cp "$f" "$D/etc/openvpn/ccd/"; done
}
# B (the branch router) and C (the device) are started by hand, the way their own software would
start_in() { n="$1"; name="$2"; shift 2; ns "$n" openvpn --daemon --writepid "$D/run/$name.pid" --cd "$D/files" --log "$D/log/$name.log" "$@"; }
waitping() { n="$1"; src="$2"; dst="$3"; t="$4"; i=0; while [ $i -lt $t ]; do if [ "$src" = "-" ]; then ns "$n" ping -c1 -W1 "$dst" >/dev/null 2>&1 && return 0; else ns "$n" ping -c1 -W1 -I "$src" "$dst" >/dev/null 2>&1 && return 0; fi; i=$((i+1)); done; return 1; }
stop_in() { f="$D/run/$1.pid"; [ -s "$f" ] && { p=$(cat "$f"); kill "$p" 2>/dev/null; i=0; while kill -0 "$p" 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done; rm -f "$f"; }; }

mkdir -p "$D/files"
nft -f "$D/vpn.nft" && say vpn-ruleset ok || say vpn-ruleset fail
install_set set1
sh "$D/sync-local.sh" 2>"$D/sync1.err" && say sync ok || { say sync fail; cat "$D/sync1.err"; }
for l in ovra ovs1; do i=0; while ! ip link show $l >/dev/null 2>&1 && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done; ip link show $l >/dev/null 2>&1 && say "up-$l" ok || say "up-$l" fail; done
pidra=$(cat "$D/run/server-pr-ra.pid" 2>/dev/null)

# the branch router connects (it was given a file by this router)
start_in b b-site --config "$D/b.conf"
waitping b - 10.98.1.1 25 && say site-up ok || { say site-up fail; tail -20 "$D/log/b-site.log"; tail -20 "$D/log/server-pr-s1.log"; }
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.1 >/dev/null 2>&1 && say site-to-router ok || say site-to-router fail
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.50 >/dev/null 2>&1 && say site-to-host ok || say site-to-host fail
ns h ping -c2 -W3 10.50.0.1 >/dev/null 2>&1 && say host-to-site ok || say host-to-site fail
ns b ping -c1 -W2 -I 10.50.0.1 192.168.1.1 >/dev/null 2>&1
# the tunnel address of the other end is the one the server pushed
ns b ip -4 addr show ovs1 2>/dev/null | grep -q "10.98.1.2" && say site-address ok || say site-address fail

# a device connects with its file
start_in c c-dev --config "$D/c1.ovpn"
waitping c - 10.9.0.1 25 && say device-up ok || { say device-up fail; tail -20 "$D/log/c-dev.log"; tail -20 "$D/log/server-pr-ra.log"; }
ns c ip -4 addr show tun0 2>/dev/null | grep -q "10.9.0.2" && say device-address ok || say device-address fail
ns c ping -c2 -W3 192.168.1.50 >/dev/null 2>&1 && say device-to-host ok || say device-to-host fail
ns c ping -c2 -W3 10.50.0.1 >/dev/null 2>&1 && say device-to-site ok || say device-to-site fail
ns h ping -c2 -W3 10.9.0.2 >/dev/null 2>&1 && say host-to-device ok || say host-to-device fail
ns c sh -c 'exec 3<>/dev/tcp/10.9.0.1/443' >/dev/null 2>&1 && say tunnel-to-web fail || say tunnel-to-web ok

# the script again: nothing changed, nothing restarted
sh "$D/sync-local.sh" 2>/dev/null
[ "$(cat "$D/run/server-pr-ra.pid" 2>/dev/null)" = "$pidra" ] && say sync-keeps-tunnels ok || say sync-keeps-tunnels fail
ns c ping -c1 -W2 10.9.0.1 >/dev/null 2>&1 && say device-stays ok || say device-stays fail

# a device taken out of the configuration: its file goes, the server restarts, the device is refused
stop_in c-dev
install_set set2
sh "$D/sync-local.sh" 2>/dev/null
[ "$(cat "$D/run/server-pr-ra.pid" 2>/dev/null)" != "$pidra" ] && say server-restarted ok || say server-restarted fail
start_in c c-dev2 --config "$D/c1.ovpn" --connect-retry-max 1 --connect-timeout 5
waitping c - 10.9.0.1 12 && say removed-device-refused fail || say removed-device-refused ok
stop_in c-dev2
# the branch router comes back by itself after the restart of the other servers
waitping b - 10.98.1.1 25 && say site-survives ok || say site-survives fail

# a certificate that is on the revocation list is refused even when the device has a file
install_set set3
sh "$D/sync-local.sh" 2>/dev/null
start_in c c-rev --config "$D/c3.ovpn" --connect-retry-max 1 --connect-timeout 5
waitping c - 10.9.0.1 12 && say revoked-refused fail || say revoked-refused ok
grep -qi "revoked" "$D/log/server-pr-ra.log" && say revoked-said-so ok || say revoked-said-so fail
stop_in c-rev
start_in c c-dev3 --config "$D/c1.ovpn"
waitping c - 10.9.0.1 25 && say good-device-still-ok ok || say good-device-still-ok fail
stop_in c-dev3

# a device of another server's kind cannot use this one: the site peer's certificate is not accepted by the remote access server
start_in c c-peer --config "$D/peer-as-device.ovpn" --connect-retry-max 1 --connect-timeout 5
waitping c - 10.9.0.1 12 && say wrong-kind-refused fail || say wrong-kind-refused ok
stop_in c-peer

# isolation: the same tunnels, isolated from the Local Networks
nft -f "$D/iso.nft" && say iso-ruleset ok || say iso-ruleset fail
ns b ping -c1 -W2 -I 10.50.0.1 192.168.1.50 >/dev/null 2>&1 && say isolated-site-to-host fail || say isolated-site-to-host ok
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.1 >/dev/null 2>&1 && say isolated-site-to-router-ping ok || say isolated-site-to-router-ping fail

# tunnels taken out: the files go, the script takes the servers down
install_set set4
sh "$D/sync-local.sh" 2>/dev/null
ip link show ovs1 >/dev/null 2>&1 && say tunnel-removed fail || say tunnel-removed ok
ip link show ovra >/dev/null 2>&1 && say remote-removed fail || say remote-removed ok
echo DONE
`

// writeSet writes the files a configuration makes into dir/<set> (server, client and ccd directories).
func writeSet(t *testing.T, dir, set string, cfg config.NetworkConfig) {
	t.Helper()
	d := mustRender(t, cfg)
	for _, a := range d.Artifacts {
		if !strings.HasPrefix(a.Path, "/etc/openvpn/") || a.Remove {
			continue
		}
		full := filepath.Join(dir, set, strings.TrimPrefix(a.Path, "/etc/openvpn/"))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, a.Content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, set, "server"), 0755)
	os.MkdirAll(filepath.Join(dir, set, "client"), 0755)
	os.MkdirAll(filepath.Join(dir, set, "ccd"), 0755)
}

func TestRealOpenVPNTunnelsWorkThroughTheGeneratedFiles(t *testing.T) {
	openvpnBinary(t)
	for _, tool := range []string{"unshare", "nsenter", "nft", "ip", "ping", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skip("no /dev/net/tun here")
	}
	if os.Getenv("PEAKROUTE_VPN_E2E_CHILD") != "" {
		return
	}
	now := time.Now()
	ca, caKey, _ := pki.NewCA("PeakRoute lab CA", now)
	srv, srvKey, _ := pki.Issue(ca, caKey, "server", true, now)
	issue := func(cn string) (cert, key, serial string) {
		c, k, err := pki.Issue(ca, caKey, cn, false, now)
		if err != nil {
			t.Fatal(err)
		}
		i, _ := pki.Describe(c)
		return c, k, i.Serial
	}
	c1cert, c1key, c1serial := issue("ra-c1")
	c3cert, c3key, c3serial := issue("ra-c3")
	peerCert, peerKey, peerSerial := issue("s1-peer")
	crlOf := func(revoked ...string) string {
		var r []pki.Revoked
		for _, s := range revoked {
			r = append(r, pki.Revoked{Serial: s, At: now})
		}
		l, err := pki.NewCRL(ca, caKey, r, int64(len(r)+1), now)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	tcRA, _ := pki.NewTLSCryptKey()
	tcS1, _ := pki.NewTLSCryptKey()
	cfgWith := func(isolated bool, crl string, revoked []string, clients ...config.OVClient) config.NetworkConfig {
		c := wanCfg(func(c *config.NetworkConfig) {
			c.WanInterface = "wan0"
			c.LanInterfaces = []string{"lan0"}
		})
		var rev []config.VPNRevoked
		for _, s := range revoked {
			rev = append(rev, config.VPNRevoked{Serial: s, Name: "x"})
		}
		c.VPN = &config.VPNConfig{
			CA:       &config.VPNCA{Cert: ca, Key: caKey, ServerCert: srv, ServerKey: srvKey, CRL: crl, CRLNumber: 1, Revoked: rev},
			OVRemote: &config.OVRemote{ListenPort: 1194, Proto: "udp", Network: "10.9.0.0/24", Endpoint: "192.0.2.1", TLSCrypt: tcRA, Isolated: isolated, Clients: clients},
			Sites: []config.VPNSite{{ID: "s1", Name: "Branch", Engine: config.EngineOpenVPN, ListenPort: 1195, Address: "10.98.1.1/29", Isolated: isolated,
				RemoteNetworks: []string{"10.50.0.0/24"}, OV: &config.OVSite{Role: config.OVRoleServer, Proto: "tcp", Remote: "192.0.2.1:1195", TLSCrypt: tcS1, PeerSerial: peerSerial}}},
		}
		return c
	}
	dev1 := config.OVClient{ID: "c1", Name: "Laptop", Serial: c1serial, Address: "10.9.0.2/32"}
	dev3 := config.OVClient{ID: "c3", Name: "Old phone", Serial: c3serial, Address: "10.9.0.4/32"}
	set1 := cfgWith(false, crlOf(), nil, dev1)
	if err := set1.Validate(); err != nil {
		t.Fatal(err)
	}
	set2 := cfgWith(false, crlOf(), nil) // the device removed
	iso := cfgWith(true, crlOf(), nil, dev1)
	empty := wanCfg(func(c *config.NetworkConfig) { c.WanInterface = "wan0"; c.LanInterfaces = []string{"lan0"} })

	dir := t.TempDir()
	writeSet(t, dir, "set1", set1)
	writeSet(t, dir, "set2", set2)
	writeSet(t, dir, "set4", empty)
	// a device whose certificate is revoked and that still has a file: the checks of the configuration would refuse that, which is the
	// point of the check of the router, so they are off for this one
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	set3 := cfgWith(false, crlOf(c3serial), nil, dev1, dev3)
	set3.VPN.CA.Revoked = []config.VPNRevoked{{Serial: c3serial, Name: "Old phone"}}
	writeSet(t, dir, "set3", set3)
	validateConfig = saved

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("vpn.nft", text(t, set1, PathNftables))
	write("iso.nft", text(t, iso, PathNftables))
	write("sync.sh", ovSyncScript)
	// the files the devices and the branch router were given, made the way the page makes them
	devFile := func(cert, key string) string {
		s, err := ovpn.Write(ovpn.Params{Name: "lab", Host: "192.0.2.1", Port: 1194, Proto: "udp", CA: ca, Cert: cert, Key: key, TLSCrypt: tcRA})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	write("c1.ovpn", devFile(c1cert, c1key))
	write("c3.ovpn", devFile(c3cert, c3key))
	// the site peer's certificate offered to the remote access server (it has the right authority but the wrong kind of name)
	write("peer-as-device.ovpn", devFile(peerCert, peerKey))
	peerFile, err := ovpn.Write(ovpn.Params{Name: "Main office", Host: "192.0.2.1", Port: 1195, Proto: "tcp", CA: ca, Cert: peerCert, Key: peerKey, TLSCrypt: tcS1,
		Tunnel: "10.98.1.2/29", Networks: []string{"192.168.1.0/24", "10.9.0.0/24"}, RouteNoPull: true})
	if err != nil {
		t.Fatal(err)
	}
	// B reads the file the way the page will, and writes its own tunnel from it
	got, err := ovpn.Read(peerFile)
	if err != nil {
		t.Fatal(err)
	}
	bTC := got.TLSCrypt
	bsite := config.VPNSite{ID: "s1", Name: "Main office", Engine: config.EngineOpenVPN, Address: "10.98.1.2/29", RemoteNetworks: got.Networks,
		OV: &config.OVSite{Role: config.OVRoleClient, Proto: got.Proto, Remote: "192.0.2.1:1195", TLSCrypt: bTC, CACert: got.CA, Cert: got.Cert, Key: got.Key}}
	bconf, err := renderOVSiteClient(bsite)
	if err != nil {
		t.Fatal(err)
	}
	write("b.conf", strings.NewReplacer("user nobody\n", "", "group nogroup\n", "").Replace(bconf))

	script := filepath.Join(dir, "lab.sh")
	write("lab.sh", ovLabScript)
	logFile, err := os.Create(filepath.Join(dir, "lab.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	args := []string{"-rmn", "sh"}
	if os.Getenv("PEAKROUTE_VPN_TRACE") != "" {
		args = append(args, "-x")
	}
	cmd := exec.Command("unshare", append(args, script, dir)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), "PEAKROUTE_VPN_E2E_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(280 * time.Second):
		cmd.Process.Kill()
		<-done
		partial, _ := os.ReadFile(filepath.Join(dir, "lab.log"))
		t.Fatalf("the lab took too long:\n%s", partial)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "lab.log"))
	labText := string(out)
	if !strings.Contains(labText, "DONE") {
		if strings.Contains(labText, "Operation not permitted") && !strings.Contains(labText, "RESULT") {
			t.Skipf("cannot build the lab here: %s (%v)", labText, err)
		}
		t.Fatalf("the lab did not finish: %v\n%s", err, labText)
	}
	res := map[string]string{}
	for _, line := range strings.Split(labText, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "RESULT" {
			res[f[1]] = f[2]
		}
	}
	for _, name := range []string{"wires", "vpn-ruleset", "sync", "up-ovra", "up-ovs1", "site-up", "site-to-router", "site-to-host", "host-to-site", "site-address",
		"device-up", "device-address", "device-to-host", "device-to-site", "host-to-device", "tunnel-to-web", "sync-keeps-tunnels", "device-stays",
		"server-restarted", "removed-device-refused", "site-survives", "revoked-refused", "revoked-said-so", "good-device-still-ok", "wrong-kind-refused",
		"iso-ruleset", "isolated-site-to-host", "isolated-site-to-router-ping", "tunnel-removed", "remote-removed"} {
		if res[name] != "ok" {
			t.Errorf("%s: %q", name, res[name])
		}
	}
	if t.Failed() {
		t.Logf("the lab said:\n%s", labText)
		for _, n := range []string{"server-pr-ra.log", "server-pr-s1.log", "b-site.log", "c-dev.log"} {
			if b, err := os.ReadFile(filepath.Join(dir, "log", n)); err == nil {
				tail := string(b)
				if len(tail) > 3000 {
					tail = tail[len(tail)-3000:]
				}
				t.Logf("%s:\n%s", n, tail)
			}
		}
	}
}
