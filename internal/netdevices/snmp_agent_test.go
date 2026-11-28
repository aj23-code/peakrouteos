package netdevices

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A very small SNMP v2c agent (GET, GETNEXT, GETBULK over a fixed table), so that the real client library is exercised over real UDP: the
// community, the bulk walk, and the kinds of value (integers, octet strings, gauges, 64-bit counters, time ticks).

type avalue struct {
	kind byte // 0x02 integer, 0x04 octet string, 0x41 counter32, 0x42 gauge32, 0x43 time ticks, 0x46 counter64
	n    uint64
	b    []byte
}

type agent struct {
	conn      *net.UDPConn
	community string
	oids      [][]int
	vals      map[string]avalue
}

func parseOID(s string) []int {
	var o []int
	for _, p := range strings.Split(s, ".") {
		n, _ := strconv.Atoi(p)
		o = append(o, n)
	}
	return o
}

func oidLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func newAgent(t *testing.T, community string, tb table, kinds map[string]byte) *agent {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no UDP here: %v", err)
	}
	a := &agent{conn: conn, community: community, vals: map[string]avalue{}}
	for k, v := range tb {
		kind := kinds[k]
		if kind == 0 {
			kind = 0x02
			if v.Text {
				kind = 0x04
			} else if strings.HasPrefix(k, oidIfHCIn) || strings.HasPrefix(k, oidIfHCOut) {
				kind = 0x46
			} else if k == oidSysUpTime {
				kind = 0x43
			} else if strings.HasPrefix(k, oidIfHighSpeed) || strings.HasPrefix(k, oidIfSpeed) {
				kind = 0x42
			}
		}
		a.vals[k] = avalue{kind: kind, n: v.Num, b: v.Bytes}
		a.oids = append(a.oids, parseOID(k))
	}
	sort.Slice(a.oids, func(i, j int) bool { return oidLess(a.oids[i], a.oids[j]) })
	go a.serve()
	t.Cleanup(func() { conn.Close() })
	return a
}

func (a *agent) port() int { return a.conn.LocalAddr().(*net.UDPAddr).Port }

func oidString(o []int) string {
	s := make([]string, len(o))
	for i, n := range o {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// ---- BER

func tlv(tag byte, content []byte) []byte {
	n := len(content)
	var l []byte
	switch {
	case n < 128:
		l = []byte{byte(n)}
	case n < 256:
		l = []byte{0x81, byte(n)}
	default:
		l = []byte{0x82, byte(n >> 8), byte(n)}
	}
	return append(append([]byte{tag}, l...), content...)
}

func readTLV(b []byte) (tag byte, content, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	tag, n, i := b[0], int(b[1]), 2
	if b[1]&0x80 != 0 {
		k := int(b[1] & 0x7f)
		if len(b) < 2+k {
			return 0, nil, nil, false
		}
		n = 0
		for _, c := range b[2 : 2+k] {
			n = n<<8 | int(c)
		}
		i = 2 + k
	}
	if len(b) < i+n {
		return 0, nil, nil, false
	}
	return tag, b[i : i+n], b[i+n:], true
}

func encInt(v int64) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		v >>= 8
		if (v == 0 && b[0]&0x80 == 0) || (v == -1 && b[0]&0x80 != 0) {
			break
		}
	}
	return b
}

func encUint(v uint64) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v)}, b...)
		v >>= 8
		if v == 0 {
			break
		}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return b
}

func decInt(b []byte) int {
	n := 0
	if len(b) > 0 && b[0]&0x80 != 0 {
		n = -1
	}
	for _, c := range b {
		n = n<<8 | int(c)
	}
	return n
}

func encOID(o []int) []byte {
	b := []byte{byte(o[0]*40 + o[1])}
	for _, n := range o[2:] {
		var part []byte
		part = append(part, byte(n&0x7f))
		for n >>= 7; n > 0; n >>= 7 {
			part = append([]byte{byte(n&0x7f) | 0x80}, part...)
		}
		b = append(b, part...)
	}
	return b
}

func decOID(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	o := []int{int(b[0]) / 40, int(b[0]) % 40}
	n := 0
	for _, c := range b[1:] {
		n = n<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			o = append(o, n)
			n = 0
		}
	}
	return o
}

func encValue(v avalue) []byte {
	switch v.kind {
	case 0x04:
		return tlv(0x04, v.b)
	case 0x02:
		return tlv(0x02, encInt(int64(v.n)))
	default:
		return tlv(v.kind, encUint(v.n))
	}
}

func (a *agent) next(after []int) ([]int, bool) {
	i := sort.Search(len(a.oids), func(i int) bool { return oidLess(after, a.oids[i]) })
	if i < len(a.oids) {
		return a.oids[i], true
	}
	return nil, false
}

func (a *agent) serve() {
	buf := make([]byte, 65535)
	for {
		n, from, err := a.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if resp := a.handle(buf[:n]); resp != nil {
			a.conn.WriteToUDP(resp, from)
		}
	}
}

func (a *agent) handle(msg []byte) []byte {
	_, body, _, ok := readTLV(msg)
	if !ok {
		return nil
	}
	_, ver, body, _ := readTLV(body)
	_, comm, body, _ := readTLV(body)
	if decInt(ver) != 1 || string(comm) != a.community {
		return nil // not v2c, or the wrong community: no answer, as a real agent
	}
	pduTag, pdu, _, _ := readTLV(body)
	_, reqid, pdu, _ := readTLV(pdu)
	_, f1, pdu, _ := readTLV(pdu)
	_, f2, pdu, _ := readTLV(pdu)
	_, vbl, _, _ := readTLV(pdu)
	var asked [][]int
	for len(vbl) > 0 {
		_, vb, rest, ok := readTLV(vbl)
		if !ok {
			break
		}
		_, oid, _, _ := readTLV(vb)
		asked = append(asked, decOID(oid))
		vbl = rest
	}
	var out []byte
	bind := func(o []int, val []byte) { out = append(out, tlv(0x30, append(tlv(0x06, encOID(o)), val...))...) }
	end := []byte{0x82, 0x00}
	switch pduTag {
	case 0xa0: // GET
		for _, o := range asked {
			if v, ok := a.vals[oidString(o)]; ok {
				bind(o, encValue(v))
			} else {
				bind(o, []byte{0x80, 0x00})
			}
		}
	case 0xa1: // GETNEXT
		for _, o := range asked {
			if nx, ok := a.next(o); ok {
				bind(nx, encValue(a.vals[oidString(nx)]))
			} else {
				bind(o, end)
			}
		}
	case 0xa5: // GETBULK: non-repeaters, max-repetitions
		nonrep, maxrep := decInt(f1), decInt(f2)
		for i, o := range asked {
			if i < nonrep {
				if nx, ok := a.next(o); ok {
					bind(nx, encValue(a.vals[oidString(nx)]))
				} else {
					bind(o, end)
				}
				continue
			}
			cur := o
			for r := 0; r < maxrep; r++ {
				nx, ok := a.next(cur)
				if !ok {
					bind(cur, end)
					break
				}
				bind(nx, encValue(a.vals[oidString(nx)]))
				cur = nx
			}
		}
	default:
		return nil
	}
	resp := tlv(0xa2, append(append(append(tlv(0x02, reqid), tlv(0x02, encInt(0))...), tlv(0x02, encInt(0))...), tlv(0x30, out)...))
	return tlv(0x30, append(append(tlv(0x02, ver), tlv(0x04, comm)...), resp...))
}

func TestARealSNMPClientReadsASwitchOverUDP(t *testing.T) {
	old := snmpTimeout
	snmpTimeout = 400 * time.Millisecond
	defer func() { snmpTimeout = old }()

	ag := newAgent(t, "s3cret", eightPortSwitch(), nil)
	dev := Device{ID: "nd1", Name: "hall", Address: "127.0.0.1", Kind: KindSNMP, SNMP: &SNMPAuth{Version: "2c", Community: "s3cret", Port: ag.port()}}
	drv := &SNMPDriver{}
	rec, err := drv.Poll(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "switch-hall" || rec.UptimeSec != 86400 || len(rec.Ports) != 8 || len(rec.Clients) != 6 {
		t.Fatalf("%+v", rec)
	}
	p1, p8 := rec.Ports[0], rec.Ports[7]
	if p1.Name != "Port 1" || !p1.Up || p1.SpeedMbps != 1000 || p1.InBytes != 1000 || p1.OutBytes != 2000 || p8.MACs != 3 {
		t.Errorf("%+v %+v", p1, p8)
	}
	if n := rec.Ports[6].Neighbour; n == nil || n.Name != "ap-kitchen" || n.ChassisID != "aa:bb:cc:00:00:09" || !n.Bridging() {
		t.Errorf("%+v", n)
	}

	// the wrong community gets no answer: a clear error and not a hang
	bad := dev
	bad.SNMP = &SNMPAuth{Version: "2c", Community: "wrong", Port: ag.port()}
	start := time.Now()
	if _, err := drv.Poll(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	// nothing is listening: the same
	closed := dev
	closed.SNMP = &SNMPAuth{Version: "2c", Community: "s3cret", Port: 1}
	if _, err := drv.Poll(context.Background(), closed); err == nil {
		t.Error("a device that is not there answered")
	}
	// settings that cannot be used are refused before anything is sent
	for _, a := range []*SNMPAuth{{Version: "1"}, {Version: "3", User: "u", AuthProto: "ROT13", AuthPass: "x"}, {Version: "3", User: "u", AuthProto: "SHA", AuthPass: "x", PrivProto: "ROT13", PrivPass: "y"}, nil} {
		d := dev
		d.SNMP = a
		if _, err := DialSNMP(context.Background(), d); err == nil {
			t.Errorf("%+v was accepted", a)
		}
	}
	_ = fmt.Sprint
}
