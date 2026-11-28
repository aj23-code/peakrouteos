package netdevices

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The SNMP driver reads only: GET and BULKWALK, never SET. What it asks for is standard (IF-MIB, BRIDGE-MIB and Q-BRIDGE-MIB, LLDP-MIB), so any
// managed switch or access point that speaks them is read the same way; what a device does not have is left out of its record.

// OIDs, without the leading dot.
const (
	oidSysDescr  = "1.3.6.1.2.1.1.1.0"
	oidSysUpTime = "1.3.6.1.2.1.1.3.0"
	oidSysName   = "1.3.6.1.2.1.1.5.0"

	oidIfDescr      = "1.3.6.1.2.1.2.2.1.2"
	oidIfType       = "1.3.6.1.2.1.2.2.1.3"
	oidIfSpeed      = "1.3.6.1.2.1.2.2.1.5"
	oidIfOperStatus = "1.3.6.1.2.1.2.2.1.8"
	oidIfInErrors   = "1.3.6.1.2.1.2.2.1.14"
	oidIfOutErrors  = "1.3.6.1.2.1.2.2.1.20"
	oidIfName       = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCIn       = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOut      = "1.3.6.1.2.1.31.1.1.1.10"
	oidIfHighSpeed  = "1.3.6.1.2.1.31.1.1.1.15"
	oidIfAlias      = "1.3.6.1.2.1.31.1.1.1.18"

	oidBasePortIfIndex = "1.3.6.1.2.1.17.1.4.1.2"
	oidTpFdbPort       = "1.3.6.1.2.1.17.4.3.1.2"
	oidTpFdbStatus     = "1.3.6.1.2.1.17.4.3.1.3"
	oidQTpFdbPort      = "1.3.6.1.2.1.17.7.1.2.2.1.2"
	oidQPvid           = "1.3.6.1.2.1.17.7.1.4.5.1.1"

	oidLldpRemChassisType = "1.0.8802.1.1.2.1.4.1.1.4"
	oidLldpRemChassisID   = "1.0.8802.1.1.2.1.4.1.1.5"
	oidLldpRemPortType    = "1.0.8802.1.1.2.1.4.1.1.6"
	oidLldpRemPortID      = "1.0.8802.1.1.2.1.4.1.1.7"
	oidLldpRemPortDesc    = "1.0.8802.1.1.2.1.4.1.1.8"
	oidLldpRemSysName     = "1.0.8802.1.1.2.1.4.1.1.9"
	oidLldpRemSysDesc     = "1.0.8802.1.1.2.1.4.1.1.10"
	oidLldpRemSysCap      = "1.0.8802.1.1.2.1.4.1.1.12"
)

// snmpValue is one answer: a number or bytes.
type snmpValue struct {
	Num   uint64
	Int   int64
	Bytes []byte
	Text  bool // the value is bytes (an octet string)
}

func (v snmpValue) str() string { return strings.TrimSpace(strings.Trim(string(v.Bytes), "\x00")) }

// snmpSession is what the driver needs of a connection to one device (the real one is gosnmp; the tests use a table).
type snmpSession interface {
	Get(oids ...string) (map[string]snmpValue, error)
	// Walk calls fn for every entry under root, with its OID (no leading dot).
	Walk(root string, fn func(oid string, v snmpValue)) error
	Close() error
}

// SNMPDriver reads a device over SNMP.
type SNMPDriver struct {
	// Dial opens a session (nil: the real one, DialSNMP).
	Dial func(ctx context.Context, d Device) (snmpSession, error)
	Now  func() time.Time
}

func (s *SNMPDriver) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// walkTable collects a column by the index after its OID.
func walkTable(sess snmpSession, root string) map[string]snmpValue {
	out := map[string]snmpValue{}
	sess.Walk(root, func(oid string, v snmpValue) {
		if rest, ok := strings.CutPrefix(oid, root+"."); ok {
			out[rest] = v
		}
	})
	return out
}

func macFromIndex(parts []string) string {
	if len(parts) != 6 {
		return ""
	}
	b := make([]string, 6)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return ""
		}
		b[i] = fmt.Sprintf("%02x", n)
	}
	return strings.Join(b, ":")
}

func macText(b []byte) string {
	if len(b) != 6 {
		return ""
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// ifTypes that are ports a person plugs a cable (or a radio) into: Ethernet, Fast and Gigabit Ethernet, 802.11, link aggregation.
var portTypes = map[int64]bool{6: true, 62: true, 69: true, 71: true, 117: true, 161: true}

// Poll reads the device. An error means it did not answer, or answered nothing that is a switch.
func (s *SNMPDriver) Poll(ctx context.Context, d Device) (Record, error) {
	dial := s.Dial
	if dial == nil {
		dial = DialSNMP
	}
	sess, err := dial(ctx, d)
	if err != nil {
		return Record{}, err
	}
	defer sess.Close()

	rec := Record{Ports: []Port{}, Clients: []Client{}, PolledAt: s.now()}
	sys, err := sess.Get(oidSysName, oidSysDescr, oidSysUpTime)
	if err != nil {
		return Record{}, fmt.Errorf("it did not answer over SNMP (the address, the port, or the community or user and passwords): %w", err)
	}
	rec.Name, rec.Description = sys[oidSysName].str(), sys[oidSysDescr].str()
	if t, ok := sys[oidSysUpTime]; ok {
		rec.UptimeSec = int64(t.Num / 100)
	}

	// ports
	names, descrs := walkTable(sess, oidIfName), walkTable(sess, oidIfDescr)
	if len(descrs) == 0 && len(names) == 0 {
		return Record{}, errors.New("it answered, but has no interface table (is this the right kind of device?)")
	}
	types, oper := walkTable(sess, oidIfType), walkTable(sess, oidIfOperStatus)
	hiSpeed, speed := walkTable(sess, oidIfHighSpeed), walkTable(sess, oidIfSpeed)
	alias, hcIn, hcOut := walkTable(sess, oidIfAlias), walkTable(sess, oidIfHCIn), walkTable(sess, oidIfHCOut)
	errIn, errOut := walkTable(sess, oidIfInErrors), walkTable(sess, oidIfOutErrors)

	byIndex := map[int]*Port{}
	var order []int
	for idx, t := range types {
		if !portTypes[t.Int] && !portTypes[int64(t.Num)] {
			continue
		}
		i, err := strconv.Atoi(idx)
		if err != nil {
			continue
		}
		p := &Port{Index: i, Name: names[idx].str()}
		if p.Name == "" {
			p.Name = descrs[idx].str()
		}
		p.Alias = alias[idx].str()
		if p.Alias == p.Name {
			p.Alias = ""
		}
		p.Up = oper[idx].Int == 1 || oper[idx].Num == 1
		if v, ok := hiSpeed[idx]; ok && v.Num > 0 {
			p.SpeedMbps = int(v.Num)
		} else if v, ok := speed[idx]; ok {
			p.SpeedMbps = int(v.Num / 1_000_000)
		}
		p.InBytes, p.OutBytes = hcIn[idx].Num, hcOut[idx].Num
		p.Errors = errIn[idx].Num + errOut[idx].Num
		byIndex[i] = p
		order = append(order, i)
	}
	sort.Ints(order)

	// which bridge port is which interface
	baseToIf := map[int]int{}
	for k, v := range walkTable(sess, oidBasePortIfIndex) {
		if b, err := strconv.Atoi(k); err == nil {
			baseToIf[b] = int(v.Num)
		}
	}
	portName := func(bridgePort int) (*Port, bool) {
		ifIdx, ok := baseToIf[bridgePort]
		if !ok {
			ifIdx = bridgePort // many devices number them the same
		}
		p, ok := byIndex[ifIdx]
		return p, ok
	}

	// the hardware addresses learned on each port: the 802.1Q table (with the VLAN), else the plain bridge table (learned entries only)
	seen := map[string]bool{}
	add := func(mac string, bridgePort, vlan int) {
		p, ok := portName(bridgePort)
		if !ok || mac == "" || seen[mac+"|"+p.Name] {
			return
		}
		seen[mac+"|"+p.Name] = true
		p.MACs++
		rec.Clients = append(rec.Clients, Client{MAC: mac, Port: p.Name, VLAN: vlan})
	}
	q := walkTable(sess, oidQTpFdbPort)
	if len(q) > 0 {
		keys := make([]string, 0, len(q))
		for k := range q {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts := strings.Split(k, ".")
			if len(parts) != 7 {
				continue
			}
			vlan, _ := strconv.Atoi(parts[0])
			add(macFromIndex(parts[1:]), int(q[k].Num), vlan)
		}
	} else {
		status := walkTable(sess, oidTpFdbStatus)
		plain := walkTable(sess, oidTpFdbPort)
		keys := make([]string, 0, len(plain))
		for k := range plain {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if st, ok := status[k]; ok && st.Num != 3 && st.Int != 3 { // 3: learned
				continue
			}
			add(macFromIndex(strings.Split(k, ".")), int(plain[k].Num), 0)
		}
	}
	// each port's own VLAN, where the device says
	for k, v := range walkTable(sess, oidQPvid) {
		if b, err := strconv.Atoi(k); err == nil {
			if p, ok := portName(b); ok {
				p.VLAN = int(v.Num)
			}
		}
	}

	// who is at the far end of each port (LLDP)
	chassis, chassisType := walkTable(sess, oidLldpRemChassisID), walkTable(sess, oidLldpRemChassisType)
	portID, portType := walkTable(sess, oidLldpRemPortID), walkTable(sess, oidLldpRemPortType)
	portDesc, sysName := walkTable(sess, oidLldpRemPortDesc), walkTable(sess, oidLldpRemSysName)
	sysDesc, caps := walkTable(sess, oidLldpRemSysDesc), walkTable(sess, oidLldpRemSysCap)
	for k := range chassis {
		parts := strings.Split(k, ".") // timeMark.localPortNum.remIndex
		if len(parts) < 3 {
			continue
		}
		local, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		p, ok := byIndex[local]
		if !ok {
			continue
		}
		n := &Neighbour{Name: sysName[k].str(), PortDesc: portDesc[k].str(), Description: sysDesc[k].str()}
		n.ChassisID = lldpID(chassis[k].Bytes, int(chassisType[k].Num+uint64(chassisType[k].Int)), true)
		n.PortID = lldpID(portID[k].Bytes, int(portType[k].Num+uint64(portType[k].Int)), false)
		n.Capabilities = lldpCaps(caps[k].Bytes)
		if n.PortDesc == n.PortID {
			n.PortDesc = ""
		}
		p.Neighbour = n
	}

	for _, i := range order {
		rec.Ports = append(rec.Ports, *byIndex[i])
	}
	return rec, nil
}

// lldpID reads a chassis or port id: the address when it is one (subtype 4 for a chassis, 3 for a port), else text, else hex.
func lldpID(b []byte, subtype int, isChassis bool) string {
	if len(b) == 6 && ((isChassis && subtype == 4) || (!isChassis && subtype == 3)) {
		return macText(b)
	}
	printable := len(b) > 0
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			printable = false
		}
	}
	if printable {
		return string(b)
	}
	if len(b) == 6 {
		return macText(b)
	}
	return fmt.Sprintf("%x", b)
}

// lldpCaps names the capabilities a neighbour has switched on (the first octet of lldpRemSysCapEnabled, most significant bit first).
func lldpCaps(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	names := []string{"Other", "Repeater", "Bridge", "WLAN AP", "Router", "Telephone", "DOCSIS", "Station"}
	var out []string
	for i, n := range names {
		if b[0]&(0x80>>uint(i)) != 0 {
			out = append(out, n)
		}
	}
	return out
}
