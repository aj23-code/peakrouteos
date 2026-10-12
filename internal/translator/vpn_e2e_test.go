package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/wg"
)

// wgTools finds wg and wg-quick: on the PATH, or in a directory a developer unpacked them into (WG_BIN_DIR).
func wgTools(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{os.Getenv("WG_BIN_DIR"), "/tmp/wgdeb/root/usr/bin"} {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "wg-quick")); err == nil {
			return dir
		}
	}
	if p, err := exec.LookPath("wg-quick"); err == nil {
		return filepath.Dir(p)
	}
	t.Skip("wg and wg-quick are not installed here")
	return ""
}

// e2eConfig is the router of the test: its internet connection is wan0, its network lan0 (192.168.1.0/24), a tunnel to a branch
// office and a remote access server.
func e2eConfig(t *testing.T, isolated bool) (config.NetworkConfig, map[string]string) {
	t.Helper()
	privA, pubA, _ := wg.GenerateKeyPair()
	privB, pubB, _ := wg.GenerateKeyPair()
	privRA, pubRA, _ := wg.GenerateKeyPair()
	privC, pubC, _ := wg.GenerateKeyPair()
	cfg := wanCfg(func(c *config.NetworkConfig) {
		c.WanInterface = "wan0"
		c.LanInterfaces = []string{"lan0"}
	})
	site := config.VPNSite{ID: "s1", Name: "Branch", Engine: config.EngineWireGuard, PrivateKey: privA, PeerPublicKey: pubB, ListenPort: 51820, Address: "10.99.1.1/30",
		RemoteNetworks: []string{"10.50.0.0/24"}, Isolated: isolated}
	ra := &config.WGRemote{PrivateKey: privRA, ListenPort: 51821, Network: "10.8.0.0/24", Endpoint: "192.0.2.1", Isolated: isolated,
		Clients: []config.VPNClient{{ID: "c1", Name: "Laptop", PublicKey: pubC, Address: "10.8.0.2/32"}}}
	cfg.VPN = &config.VPNConfig{Sites: []config.VPNSite{site}, WGRemote: ra}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// the other end of the tunnel and the client are written the way their routers and apps would
	other := config.VPNSite{ID: "s1", Name: "Main office", Engine: config.EngineWireGuard, PrivateKey: privB, PeerPublicKey: pubA, ListenPort: 51822, Address: "10.99.1.2/30",
		Endpoint: "192.0.2.1:51820", Keepalive: 25,
		// the remote access clients live behind the main office too: the branch must accept their addresses from the tunnel
		RemoteNetworks: []string{"192.168.1.0/24", "10.8.0.0/24"}}
	client := wg.ClientConfig(wg.ClientParams{Name: "Laptop", PrivateKey: privC, Address: "10.8.0.2/32", ServerPublicKey: pubRA, AllowedIPs: []string{"192.168.1.0/24", "10.8.0.0/24", "10.50.0.0/24"},
		Endpoint: "192.0.2.1:51821", Keepalive: 25})
	return cfg, map[string]string{"b.conf": renderWGSite(other), "c.conf": client}
}

const e2eScript = `set -u
D="$1"; WGBIN="$2"
export PATH="$D/bin:$WGBIN:$PATH"
mkdir -p "$D/bin" "$D/wg" "$D/state"
sed "s|/etc/wireguard|$D/wg|g" "$WGBIN/wg-quick" > "$D/bin/wg-quick"; chmod +x "$D/bin/wg-quick"
sed "s|/etc/wireguard|$D/wg|g; s|/run/peakroute-wg|$D/state|g; s|^PATH=|PATH=$D/bin:$WGBIN:|" "$D/sync.sh" > "$D/sync-local.sh"
say() { echo "RESULT $1 $2"; }
ip link set lo up
mkns() { unshare -n sh -c "echo \$\$ > $D/$1.pid; exec sleep 300" & i=0; while [ ! -s "$D/$1.pid" ] && [ $i -lt 100 ]; do sleep 0.05; i=$((i+1)); done; }
mkns inet; mkns b; mkns c; mkns h
pid() { cat "$D/$1.pid"; }
ns() { n="$1"; shift; nsenter -t "$(pid $n)" -n "$@"; }
trap 'for n in inet b c h; do kill "$(pid $n)" 2>/dev/null; done' EXIT
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
# the internet works between the three
ns b ping -c1 -W2 192.0.2.1 >/dev/null 2>&1 && say wires ok || say wires fail
# A: the plain ruleset first (nothing for the VPN): the tunnel cannot come up
nft -f "$D/plain.nft" && say plain-ruleset ok || say plain-ruleset fail
sh "$D/sync-local.sh" 2>"$D/sync1.err" && say sync ok || { say sync fail; cat "$D/sync1.err"; }
cp "$D/b.conf" "$D/b/wgs1.conf" 2>/dev/null || { mkdir -p "$D/b"; cp "$D/b.conf" "$D/b/wgs1.conf"; }
ns b wg-quick up "$D/b/wgs1.conf" >"$D/bup.out" 2>&1 && say b-up ok || { say b-up fail; cat "$D/bup.out"; }
ns b ping -c1 -W3 -I 10.50.0.1 10.99.1.1 >/dev/null 2>&1 && say control-no-rule fail || say control-no-rule ok
# A: the ruleset with the VPN
nft -f "$D/vpn.nft" && say vpn-ruleset ok || say vpn-ruleset fail
ns b wg-quick down "$D/b/wgs1.conf" >/dev/null 2>&1
ns b wg-quick up "$D/b/wgs1.conf" >/dev/null 2>&1
ns b ping -c3 -W3 -I 10.50.0.1 10.99.1.1 >/dev/null 2>&1 && say site-to-tunnel-address ok || say site-to-tunnel-address fail
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.1 >/dev/null 2>&1 && say site-to-router ok || say site-to-router fail
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.50 >/dev/null 2>&1 && say site-to-host ok || say site-to-host fail
ping -c2 -W3 -I 192.168.1.1 10.50.0.1 >/dev/null 2>&1 && say router-to-site ok || say router-to-site fail
ns h ping -c2 -W3 10.50.0.1 >/dev/null 2>&1 && say host-to-site ok || say host-to-site fail
# remote access
mkdir -p "$D/c"; cp "$D/c.conf" "$D/c/wgc.conf"
ns c wg-quick up "$D/c/wgc.conf" >"$D/cup.out" 2>&1 && say c-up ok || { say c-up fail; cat "$D/cup.out"; }
ns c ping -c3 -W3 10.8.0.1 >/dev/null 2>&1 && say client-to-router ok || say client-to-router fail
ns c ping -c2 -W3 192.168.1.50 >/dev/null 2>&1 && say client-to-host ok || say client-to-host fail
ns c ping -c2 -W3 10.50.0.1 >/dev/null 2>&1 && say client-to-site ok || say client-to-site fail
ns h ping -c2 -W3 10.8.0.2 >/dev/null 2>&1 && say host-to-client ok || say host-to-client fail
# what wg says
wg show wgs1 latest-handshakes | awk '{ if ($2 > 0) print "RESULT site-handshake ok"; else print "RESULT site-handshake fail" }'
wg show wgra latest-handshakes | awk '{ if ($2 > 0) print "RESULT ra-handshake ok"; else print "RESULT ra-handshake fail" }'
# the script again: nothing changed, nothing restarted
before=$(wg show wgs1 latest-handshakes)
sh "$D/sync-local.sh" 2>/dev/null; after=$(wg show wgs1 latest-handshakes)
[ "$before" = "$after" ] && say sync-keeps-tunnels ok || say sync-keeps-tunnels fail
# isolation: the same tunnels, isolated
nft -f "$D/iso.nft" && say iso-ruleset ok || say iso-ruleset fail
ns b ping -c1 -W2 -I 10.50.0.1 192.168.1.50 >/dev/null 2>&1 && say isolated-site-to-host fail || say isolated-site-to-host ok
ns c ping -c1 -W2 192.168.1.50 >/dev/null 2>&1 && say isolated-client-to-host fail || say isolated-client-to-host ok
ns c ping -c2 -W3 10.8.0.1 >/dev/null 2>&1 && say isolated-client-to-router-ping ok || say isolated-client-to-router-ping fail
ns b ping -c2 -W3 -I 10.50.0.1 192.168.1.1 >/dev/null 2>&1 && say isolated-site-to-router-ping ok || say isolated-site-to-router-ping fail
# only DNS and ping reach the router from a tunnel: its web interface does not
ns c sh -c 'exec 3<>/dev/tcp/10.8.0.1/443' >/dev/null 2>&1 && say tunnel-to-web fail || say tunnel-to-web ok
# a tunnel taken out: its file goes, the script takes it down
rm -f "$D/wg/wgs1.conf"
sh "$D/sync-local.sh" 2>/dev/null
ip link show wgs1 >/dev/null 2>&1 && say tunnel-removed fail || say tunnel-removed ok
ip link show wgra >/dev/null 2>&1 && say other-tunnel-stays ok || say other-tunnel-stays fail
echo DONE
`

func TestRealTunnelsWorkThroughTheGeneratedFiles(t *testing.T) {
	wgbin := wgTools(t)
	for _, tool := range []string{"unshare", "nsenter", "nft", "ip", "ping", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if os.Getenv("PEAKROUTE_VPN_E2E_CHILD") != "" {
		return
	}
	dir := t.TempDir()
	cfg, others := e2eConfig(t, false)
	iso, _ := e2eConfig(t, true)
	// the isolated variant must differ only in the switches: same keys, so that the running tunnels stay valid
	iso.VPN.Sites[0].PrivateKey, iso.VPN.Sites[0].PeerPublicKey = cfg.VPN.Sites[0].PrivateKey, cfg.VPN.Sites[0].PeerPublicKey
	iso.VPN.WGRemote.PrivateKey, iso.VPN.WGRemote.Clients = cfg.VPN.WGRemote.PrivateKey, cfg.VPN.WGRemote.Clients

	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	for n, c := range others {
		write(n, c, 0600)
	}
	write("vpn.nft", text(t, cfg, PathNftables), 0644)
	write("iso.nft", text(t, iso, PathNftables), 0644)
	plain := cfg
	plain.VPN = nil
	write("plain.nft", text(t, plain, PathNftables), 0644)
	os.MkdirAll(filepath.Join(dir, "wg"), 0700)
	write("wg/wgs1.conf", text(t, cfg, "/etc/wireguard/wgs1.conf"), 0600)
	write("wg/wgra.conf", text(t, cfg, "/etc/wireguard/wgra.conf"), 0600)
	write("sync.sh", wgSyncScript, 0755)
	script := filepath.Join(dir, "e2e.sh")
	write("e2e.sh", e2eScript, 0755)

	// the output goes to a file, not a pipe: the processes the lab leaves behind would keep a pipe open
	logFile, err := os.Create(filepath.Join(dir, "lab.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	args := []string{"-rmn", "sh"}
	if os.Getenv("PEAKROUTE_VPN_TRACE") != "" {
		args = append(args, "-x")
	}
	cmd := exec.Command("unshare", append(args, script, dir, wgbin)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), "PEAKROUTE_VPN_E2E_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(150 * time.Second):
		cmd.Process.Kill()
		<-done
		partial, _ := os.ReadFile(filepath.Join(dir, "lab.log"))
		t.Fatalf("the lab took too long:\n%s", partial)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "lab.log"))
	text := string(out)
	if !strings.Contains(text, "DONE") {
		if strings.Contains(text, "Operation not permitted") || strings.Contains(text, "Permission denied") && !strings.Contains(text, "RESULT") {
			t.Skipf("cannot build the lab here: %s (%v)", text, err)
		}
		t.Fatalf("the lab did not finish: %v\n%s", err, text)
	}
	res := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "RESULT" {
			res[f[1]] = f[2]
		}
	}
	for _, name := range []string{"wires", "plain-ruleset", "sync", "b-up", "control-no-rule", "vpn-ruleset", "site-to-tunnel-address", "site-to-router", "site-to-host", "router-to-site", "host-to-site",
		"c-up", "client-to-router", "client-to-host", "client-to-site", "host-to-client", "site-handshake", "ra-handshake", "sync-keeps-tunnels", "iso-ruleset",
		"isolated-site-to-host", "isolated-client-to-host", "isolated-client-to-router-ping", "isolated-site-to-router-ping", "tunnel-to-web", "tunnel-removed", "other-tunnel-stays"} {
		if res[name] != "ok" {
			t.Errorf("%s: %q", name, res[name])
		}
	}
	if t.Failed() {
		t.Logf("the lab said:\n%s", text)
	}
}

func TestTheCheckBeforeTheCommitReadsTheTunnelFilesLikeWgQuick(t *testing.T) {
	wgbin := wgTools(t)
	cmd := serviceCommands[SvcWireGuard]
	check := preChecks[SvcWireGuard]
	if check.name != "sh" || len(check.args) != 2 || cmd.name != "sh" || cmd.args[0] != PathWGSync {
		t.Fatalf("%+v %+v", check, cmd)
	}
	dir := t.TempDir()
	script := strings.ReplaceAll(check.args[1], PathWireGuardDir, dir)
	run := func() (string, error) {
		// wg-quick wants to be root, and the check makes an interface (inside a user and network namespace we may)
		c := exec.Command("unshare", "-rn", "sh", "-c", script)
		c.Env = []string{"PATH=" + wgbin + ":/usr/bin:/bin"}
		out, err := c.CombinedOutput()
		return string(out), err
	}
	// no files at all: nothing to check
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cfg, _ := e2eConfig(t, false)
	for _, n := range []string{"wgs1", "wgra"} {
		if err := os.WriteFile(filepath.Join(dir, n+".conf"), []byte(text(t, cfg, "/etc/wireguard/"+n+".conf")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := run(); err != nil {
		t.Fatalf("good files refused: %v %s", err, out)
	}
	// a file wg-quick cannot read stops the commit
	os.WriteFile(filepath.Join(dir, "wgs1.conf"), []byte("[Interface]\nPrivateKey = not a key\nBogus = 1\n"), 0600)
	if out, err := run(); err == nil {
		t.Fatalf("a broken file was accepted: %s", out)
	}
	// a file that is not a tunnel's (no wg prefix) is not looked at
	os.WriteFile(filepath.Join(dir, "wgs1.conf"), []byte(text(t, cfg, "/etc/wireguard/wgs1.conf")), 0600)
	os.WriteFile(filepath.Join(dir, "other.conf"), []byte("nonsense"), 0600)
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}
