package netdevices

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// snmpTimeout is how long one question waits for its answer (the tests shorten it).
var snmpTimeout = 4 * time.Second

// DialSNMP opens a real SNMP session (v2c or v3) to a device.
func DialSNMP(ctx context.Context, d Device) (snmpSession, error) {
	a := d.SNMP
	if a == nil {
		return nil, errors.New("no SNMP settings")
	}
	port := a.Port
	if port == 0 {
		port = 161
	}
	g := &gosnmp.GoSNMP{
		Target: d.Address, Port: uint16(port), Transport: "udp", Timeout: snmpTimeout, Retries: 1, MaxRepetitions: 12, Context: ctx,
	}
	switch a.Version {
	case "2c", "":
		g.Version, g.Community = gosnmp.Version2c, a.Community
	case "3":
		g.Version, g.SecurityModel = gosnmp.Version3, gosnmp.UserSecurityModel
		usm := &gosnmp.UsmSecurityParameters{UserName: a.User}
		g.MsgFlags = gosnmp.NoAuthNoPriv
		if a.AuthProto != "" {
			proto, ok := map[string]gosnmp.SnmpV3AuthProtocol{"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA224": gosnmp.SHA224, "SHA256": gosnmp.SHA256, "SHA384": gosnmp.SHA384, "SHA512": gosnmp.SHA512}[strings.ToUpper(a.AuthProto)]
			if !ok {
				return nil, fmt.Errorf("unknown authentication protocol %q", a.AuthProto)
			}
			usm.AuthenticationProtocol, usm.AuthenticationPassphrase = proto, a.AuthPass
			g.MsgFlags = gosnmp.AuthNoPriv
			if a.PrivProto != "" {
				priv, ok := map[string]gosnmp.SnmpV3PrivProtocol{"DES": gosnmp.DES, "AES": gosnmp.AES, "AES192": gosnmp.AES192, "AES256": gosnmp.AES256, "AES192C": gosnmp.AES192C, "AES256C": gosnmp.AES256C}[strings.ToUpper(a.PrivProto)]
				if !ok {
					return nil, fmt.Errorf("unknown encryption protocol %q", a.PrivProto)
				}
				usm.PrivacyProtocol, usm.PrivacyPassphrase = priv, a.PrivPass
				g.MsgFlags = gosnmp.AuthPriv
			}
		}
		g.SecurityParameters = usm
	default:
		return nil, fmt.Errorf("SNMP version %q is not supported (use 2c or 3)", a.Version)
	}
	if err := g.Connect(); err != nil {
		return nil, err
	}
	return &gosnmpSession{g: g}, nil
}

type gosnmpSession struct{ g *gosnmp.GoSNMP }

func (s *gosnmpSession) Close() error { return s.g.Conn.Close() }

func convert(p gosnmp.SnmpPDU) (snmpValue, bool) {
	switch p.Type {
	case gosnmp.OctetString:
		b, _ := p.Value.([]byte)
		return snmpValue{Bytes: b, Text: true}, true
	case gosnmp.Integer, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Uinteger32, gosnmp.Counter64:
		// the library hands back int, uint, uint32 or uint64 according to the kind and its own version
		switch n := p.Value.(type) {
		case int:
			return snmpValue{Int: int64(n), Num: uint64(max(n, 0))}, true
		case int64:
			return snmpValue{Int: n, Num: uint64(max(n, 0))}, true
		case uint:
			return snmpValue{Num: uint64(n), Int: int64(n)}, true
		case uint32:
			return snmpValue{Num: uint64(n), Int: int64(n)}, true
		case uint64:
			return snmpValue{Num: n, Int: int64(n)}, true
		}
	}
	return snmpValue{}, false // no such object, end of the MIB, and kinds we do not read
}

func (s *gosnmpSession) Get(oids ...string) (map[string]snmpValue, error) {
	req := make([]string, len(oids))
	for i, o := range oids {
		req[i] = "." + o
	}
	res, err := s.g.Get(req)
	if err != nil {
		return nil, err
	}
	out := map[string]snmpValue{}
	for _, p := range res.Variables {
		if v, ok := convert(p); ok {
			out[strings.TrimPrefix(p.Name, ".")] = v
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no answer to the system questions")
	}
	return out, nil
}

func (s *gosnmpSession) Walk(root string, fn func(oid string, v snmpValue)) error {
	return s.g.BulkWalk("."+root, func(p gosnmp.SnmpPDU) error {
		if v, ok := convert(p); ok {
			fn(strings.TrimPrefix(p.Name, "."), v)
		}
		return nil
	})
}
