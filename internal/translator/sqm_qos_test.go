package translator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func qosCfg(set func(*config.NetworkConfig)) config.NetworkConfig {
	cfg := routerCfg() // eth0 is the internet, eth1 the default network
	guest := guestNet()
	cfg.Networks = []config.Network{guest}
	cfg.SQM = &config.SQMConfig{Enabled: true, DownloadKbit: 100000, UploadKbit: 100000, Percent: 100, Link: config.SQMEthernet, PerHost: true}
	if set != nil {
		set(&cfg)
	}
	return cfg
}

func TestTheLineIsDividedByPriorityAndLimitedByCeiling(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SetNetworkQoS(config.DefaultNetworkID, config.NetQoS{Priority: config.QoSHigh})
		c.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSLow, LimitDownKbit: 5000, LimitUpKbit: 2000})
	})
	by := map[string]config.QoSClass{}
	for _, cl := range cfg.QoSClasses() {
		by[cl.ID] = cl
	}
	// weights: other 2, LAN (high) 4, guest (low) 1: a line of 100 000 is shared 2/7, 4/7, 1/7
	if o := by["other"]; o.UpRate != 28571 || o.UpCeil != 100000 || o.Prio != 1 || o.Index != 0 {
		t.Errorf("other %+v", o)
	}
	if l := by[config.DefaultNetworkID]; l.DownRate != 57142 || l.DownCeil != 100000 || l.Prio != 0 || l.Index != 1 {
		t.Errorf("LAN %+v", l)
	}
	// a limit under the assured share is the assured share too
	if g := by["guest"]; g.DownCeil != 5000 || g.DownRate != 5000 || g.UpCeil != 2000 || g.UpRate != 2000 || g.Prio != 2 || g.Index != 2 {
		t.Errorf("guest %+v", g)
	}
}

func TestAnUnshapedDirectionHasNoClasses(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SQM.UploadKbit = 0
		c.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSLow, LimitDownKbit: 5000})
	})
	for _, cl := range cfg.QoSClasses() {
		if cl.UpRate != 0 || cl.UpCeil != 0 || cl.DownRate == 0 {
			t.Errorf("%+v", cl)
		}
	}
}

func TestNothingIsDividedUntilANetworkIsSetToSomething(t *testing.T) {
	cfg := qosCfg(nil)
	if cfg.QoSActive() || strings.Contains(content(t, cfg, PathSQM), "SQM_CLASSES") || strings.Contains(content(t, cfg, PathNftables), "peakroute_qos") {
		t.Error("the line is divided although every network has the defaults")
	}
	// the settings are kept while shaping is off, and do nothing: no marks, no classes, no error
	off := qosCfg(func(c *config.NetworkConfig) {
		c.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSLow, LimitDownKbit: 5000})
		c.SQM.Enabled = false
	})
	if err := off.Validate(); err != nil {
		t.Errorf("%v", err)
	}
	if off.QoSActive() || strings.Contains(content(t, off, PathNftables), "peakroute_qos") {
		t.Error("a network's setting was applied with the shaping off")
	}
	if msg := off.QoSInactive("guest"); !strings.Contains(msg, "smart queue management is off") {
		t.Errorf("%q", msg)
	}
}

func TestTheFilesTellTheQueueAndTheFirewallWhichNetworkIsWhich(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSLow, LimitDownKbit: 5000})
	})
	conf := content(t, cfg, PathSQM)
	for _, want := range []string{"SQM_CLASSES=\"\n", "0x0 2 1 ", "0x10000 11 1 ", "0x20000 12 2 "} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in\n%s", want, conf)
		}
	}
	nft := content(t, cfg, PathNftables)
	tbl := nft[strings.Index(nft, "table inet peakroute_qos"):]
	for _, want := range []string{
		`iifname "eth1" ct mark and 0x00ff0000 == 0 ct mark set ct mark and 0xff00ffff or 0x00010000`,
		`oifname "eth1" ct mark and 0x00ff0000 == 0 ct mark set ct mark and 0xff00ffff or 0x00010000`,
		`iifname "br20" ct mark and 0x00ff0000 == 0 ct mark set ct mark and 0xff00ffff or 0x00020000`,
		`oifname "br20" ct mark and 0x00ff0000 == 0`,
		`meta mark set ct mark and 0x00ff0000`, "hook forward priority mangle",
	} {
		if !strings.Contains(tbl, want) {
			t.Errorf("missing %q in\n%s", want, tbl)
		}
	}
	// switching a priority only reloads the firewall (the marks) and reshapes; networking is not touched
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, cfg))
	cfg.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSHigh, LimitDownKbit: 5000})
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	if got := actionSummary(plan); got != "apply sqm" {
		t.Errorf("a changed priority: %q", got)
	}
	cfg.SetNetworkQoS("guest", config.NetQoS{})
	plan, _ = PlanChanges(root, mustRender(t, cfg))
	if got := actionSummary(plan); got != "reload nftables, apply sqm" {
		t.Errorf("back to the defaults takes the marks away: %q", got)
	}
}

func TestALimitInAnUnshapedDirectionIsKeptAndSaidToDoNothing(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SQM.DownloadKbit = 0
		c.SetNetworkQoS("guest", config.NetQoS{LimitDownKbit: 5000})
	})
	if err := cfg.Validate(); err != nil {
		t.Errorf("%v", err)
	}
	if msg := cfg.QoSInactive("guest"); !strings.Contains(msg, "download limit does nothing") {
		t.Errorf("%q", msg)
	}
	if msg := cfg.QoSInactive(config.DefaultNetworkID); msg != "" {
		t.Errorf("a network with the defaults has nothing to say: %q", msg)
	}
}

func TestPrioritiesAndLimitsHaveTheirOwnValidation(t *testing.T) {
	for name, q := range map[string]config.NetQoS{"priority": {Priority: "urgent"}, "negative": {LimitUpKbit: -1}, "too big": {LimitDownKbit: 99_000_000}} {
		c := qosCfg(func(c *config.NetworkConfig) { c.SetNetworkQoS("guest", q) })
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "networks[0].qos_") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the default network's own fields
	c := qosCfg(func(c *config.NetworkConfig) { c.LanQoSPriority = "loud" })
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "lan_qos_priority") {
		t.Errorf("%v", err)
	}
}

// ---- the real kernel -------------------------------------------------------------------------------------------------------------

// the shaping on a router in a throw-away network namespace, with a client on each of two networks and a server on the internet side
func shapeInKernel(t *testing.T, cfg config.NetworkConfig, scenario string) string {
	t.Helper()
	for _, tool := range []string{"tc", "ip", "nft", "python3", "nsenter", "unshare"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here: %v %s", err, out)
	}
	helper, _ := filepath.Abs("../../image/overlay/usr/lib/peakroute/sqm")
	dir := t.TempDir()
	conf, _ := renderSQM(cfg)
	conf = strings.Replace(conf, "SQM_DEV=eth0", "SQM_DEV=wan0", 1) // the test's name for the internet port
	nft := `
table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "wan0" masquerade
    }
}
` + strings.NewReplacer(`"eth1"`, `"lan1"`, `"br20"`, `"lan2"`).Replace(qosMarkTable(cfg, "eth1"))
	files := map[string]string{
		"conf": conf, "q.nft": nft,
		"sink.py": `import socket,sys,time
import signal
signal.alarm(35)
port=int(sys.argv[1]); mode=sys.argv[2]
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",port)); s.listen(4)
while True:
    c,_=s.accept()
    if mode=="send":
        b=b"x"*65536; t0=time.time()
        try:
            while time.time()-t0<6: c.sendall(b)
        except Exception: pass
    else:
        n=0; t0=time.time(); started=None
        while True:
            d=c.recv(65536)
            if not d: break
            n+=len(d)
            if started is None and time.time()-t0>1.5: started=(time.time(),n)
            if time.time()-t0>5.5: break
        if started: print("%d %.2f"%(port,(n-started[1])*8/(time.time()-started[0])/1e6)); sys.stdout.flush()
    c.close()
`,
		"recv.py": `import socket,sys,time
s=socket.create_connection((sys.argv[1],int(sys.argv[2]))); t0=time.time(); n=0; started=None
while True:
    d=s.recv(65536)
    if not d: break
    n+=len(d)
    if started is None and time.time()-t0>1.5: started=(time.time(),n)
    if time.time()-t0>5.5: break
if started: print("%s %.2f"%(sys.argv[2],(n-started[1])*8/(time.time()-started[0])/1e6))
`,
		"send.py": `import socket,sys,time
s=socket.create_connection((sys.argv[1],int(sys.argv[2]))); b=b"x"*65536; t0=time.time()
try:
    while time.time()-t0<6: s.sendall(b)
except Exception: pass
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`
set -e
SINKS=""
trap 'kill $C1 $C2 $S $SINKS 2>/dev/null || true' EXIT
export SQM_CONF=%[1]s/conf SQM_STATE=%[1]s/state
ip link set lo up
unshare -n sleep 40 & C1=$!
unshare -n sleep 40 & C2=$!
unshare -n sleep 40 & S=$!
sleep 0.5
ip link add lan1 type veth peer name c1; ip link set c1 netns $C1
ip link add lan2 type veth peer name c2; ip link set c2 netns $C2
ip link add wan0 type veth peer name s1; ip link set s1 netns $S
ip addr add 192.168.1.1/24 dev lan1; ip link set lan1 up
ip addr add 10.20.0.1/24 dev lan2; ip link set lan2 up
ip addr add 192.0.2.1/24 dev wan0; ip link set wan0 up
nsenter -t $C1 -n sh -c "ip link set lo up; ip addr add 192.168.1.2/24 dev c1; ip link set c1 up; ip route add default via 192.168.1.1"
nsenter -t $C2 -n sh -c "ip link set lo up; ip addr add 10.20.0.2/24 dev c2; ip link set c2 up; ip route add default via 10.20.0.1"
nsenter -t $S -n sh -c "ip link set lo up; ip addr add 192.0.2.2/24 dev s1; ip link set s1 up; ip route add 192.168.1.0/24 via 192.0.2.1; ip route add 10.20.0.0/24 via 192.0.2.1"
sysctl -qw net.ipv4.ip_forward=1
nft -f %[1]s/q.nft
sh %[2]s start
export P=%[1]s
%[3]s
`, dir, helper, scenario)
	out, err := exec.Command("unshare", "-rn", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

// rates reads the lines "<port> <Mbit/s>" a scenario printed
func rates(t *testing.T, out string) map[int]float64 {
	t.Helper()
	m := map[int]float64{}
	for _, l := range regexp.MustCompile(`(?m)^(\d+) ([0-9.]+)$`).FindAllStringSubmatch(out, -1) {
		port, _ := strconv.Atoi(l[1])
		v, _ := strconv.ParseFloat(l[2], 64)
		m[port] = v
	}
	return m
}

// the sink on the server side, on a port: it either sends to whoever connects (a download for the client) or reads what is sent (an upload)
const serve = `nsenter -t $S -n python3 $P/sink.py %d %s & SINKS="$SINKS $!"; `

func TestALimitHoldsANetworkToItsCeilingInBothDirectionsAndLeavesTheOthersAlone(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SQM.DownloadKbit, c.SQM.UploadKbit = 60000, 40000
		c.SetNetworkQoS("guest", config.NetQoS{LimitDownKbit: 8000, LimitUpKbit: 6000})
	})
	// the guest network's client downloads, then uploads; the default network's client does the same, alone each time
	out := shapeInKernel(t, cfg, fmt.Sprintf(serve+serve+serve+serve+`sleep 0.6
nsenter -t $C2 -n python3 $P/recv.py 192.0.2.2 5001
nsenter -t $C1 -n python3 $P/recv.py 192.0.2.2 5002
nsenter -t $C2 -n python3 $P/send.py 192.0.2.2 5003 &
sleep 7
nsenter -t $C1 -n python3 $P/send.py 192.0.2.2 5004 &
sleep 7
tc -s class show dev ifb-wan | grep -c "class htb"
`, 5001, "send", 5002, "send", 5003, "recv", 5004, "recv"))
	r := rates(t, out)
	t.Logf("measured %v\n%s", r, out)
	within := func(got, want, frac float64) bool { return got > want*(1-frac) && got < want*(1+frac) }
	if !within(r[5001], 8, 0.2) {
		t.Errorf("the limited network's download: %.1f Mbit/s, want about 8", r[5001])
	}
	if r[5002] < 40 || r[5002] > 61 {
		t.Errorf("the other network's download: %.1f Mbit/s, want most of the 60 the line is shaped to", r[5002])
	}
	if !within(r[5003], 6, 0.25) {
		t.Errorf("the limited network's upload: %.1f Mbit/s, want about 6", r[5003])
	}
	if r[5004] < 25 || r[5004] > 41 {
		t.Errorf("the other network's upload: %.1f Mbit/s, want most of the 40 the line is shaped to", r[5004])
	}
}

func TestWhenTheLineIsFullAHighPriorityNetworkGetsMoreOfIt(t *testing.T) {
	cfg := qosCfg(func(c *config.NetworkConfig) {
		c.SQM.DownloadKbit, c.SQM.UploadKbit = 20000, 20000
		c.SetNetworkQoS(config.DefaultNetworkID, config.NetQoS{Priority: config.QoSHigh})
		c.SetNetworkQoS("guest", config.NetQoS{Priority: config.QoSLow})
	})
	// both clients download as fast as they can at the same time over a line of 20 Mbit/s
	out := shapeInKernel(t, cfg, fmt.Sprintf(serve+serve+`sleep 0.6
nsenter -t $C1 -n python3 $P/recv.py 192.0.2.2 5001 &
nsenter -t $C2 -n python3 $P/recv.py 192.0.2.2 5002 &
sleep 8
`, 5001, "send", 5002, "send"))
	r := rates(t, out)
	t.Logf("measured %v\n%s", r, out)
	high, low := r[5001], r[5002]
	if high == 0 || low == 0 {
		t.Fatalf("a client got nothing: %v\n%s", r, out)
	}
	// weights 4 and 1 of the line that is left after "everything else" (weight 2, idle): about 4 to 1 in favour of the high priority one
	if high < 2.5*low {
		t.Errorf("high %.1f Mbit/s, low %.1f: the high priority network should get several times as much", high, low)
	}
	if high+low < 14 || high+low > 21 {
		t.Errorf("together %.1f Mbit/s: the line is 20", high+low)
	}
}
