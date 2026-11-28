// Package lldp reads what lldpd (the LLDP and CDP daemon, docs/device-pages.md phase 2) has heard from the equipment plugged into this
// router's ports: the switch or access point at the other end of each cable, its name, its address and the port it is. lldpd also tells
// the neighbours what this router is, so a switch lists it by name.
package lldp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Neighbour is the device at the other end of one of this router's ports.
type Neighbour struct {
	LocalPort    string   `json:"local_port"`            // the router's port it is plugged into
	Via          string   `json:"via,omitempty"`         // LLDP, CDP, EDP...
	Name         string   `json:"name,omitempty"`        // the system name it announces
	ChassisID    string   `json:"chassis_id,omitempty"`  // usually its hardware address
	PortID       string   `json:"port_id,omitempty"`     // the port on its side ("Gi1/0/12", "port 7")
	PortDesc     string   `json:"port_desc,omitempty"`   // what that port is called by the administrator there
	Description  string   `json:"description,omitempty"` // the system description (make, model, software)
	MgmtIPs      []string `json:"mgmt_ips,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"` // the ones it has switched on: Bridge, Router, Wlan...
	VLAN         string   `json:"vlan,omitempty"`         // the port's own VLAN, when it says
	Age          string   `json:"age,omitempty"`          // how long ago it was first heard, as lldpd words it
}

// Runner runs a program and returns what it printed.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Read asks lldpd for its neighbours. An error means lldpd is not running (or not installed): there is then nothing to show.
func Read(ctx context.Context, r Runner) ([]Neighbour, error) {
	out, err := r.Run(ctx, "lldpcli", "-f", "json", "show", "neighbors", "details")
	if err != nil {
		return nil, err
	}
	return Parse([]byte(out))
}

// lldpd's JSON changes shape with what it holds: one of something is an object and several are a list, a system with no name has no name
// key above its chassis, a lone address is a string. Everything is read through these helpers rather than fixed types.

func asList(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		return t
	default:
		return []any{t}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprint(int64(t))
	case bool:
		return fmt.Sprint(t)
	}
	return ""
}

// Parse reads the output of `lldpcli -f json show neighbors details`.
func Parse(data []byte) ([]Neighbour, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("lldpd's answer cannot be read: %w", err)
	}
	var out []Neighbour
	for _, entry := range asList(asMap(doc["lldp"])["interface"]) {
		for local, v := range asMap(entry) {
			out = append(out, neighbour(local, asMap(v)))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LocalPort != out[j].LocalPort {
			return out[i].LocalPort < out[j].LocalPort
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func neighbour(local string, v map[string]any) Neighbour {
	n := Neighbour{LocalPort: local, Via: str(v["via"]), Age: str(v["age"])}
	// the chassis: {"switch1": {"id": ...}} with a name, {"id": ...} without
	chassis := asMap(v["chassis"])
	if _, direct := chassis["id"]; !direct {
		for name, c := range chassis {
			n.Name = name
			chassis = asMap(c)
			break
		}
	}
	if id := asMap(chassis["id"]); id != nil {
		n.ChassisID = str(id["value"])
	}
	n.Description = str(chassis["descr"])
	for _, ip := range asList(chassis["mgmt-ip"]) {
		if s := str(ip); s != "" {
			n.MgmtIPs = append(n.MgmtIPs, s)
		}
	}
	for _, c := range asList(chassis["capability"]) {
		m := asMap(c)
		if enabled, _ := m["enabled"].(bool); enabled {
			n.Capabilities = append(n.Capabilities, str(m["type"]))
		}
	}
	port := asMap(v["port"])
	if id := asMap(port["id"]); id != nil {
		n.PortID = str(id["value"])
	}
	n.PortDesc = str(port["descr"])
	if n.PortDesc == n.PortID {
		n.PortDesc = ""
	}
	// the port's own VLAN: the entry with "pvid": true, else the first
	var vlans []map[string]any
	for _, e := range asList(v["vlan"]) {
		if m := asMap(e); m != nil {
			vlans = append(vlans, m)
		}
	}
	for _, m := range vlans {
		if pv, _ := m["pvid"].(bool); pv {
			n.VLAN = str(m["vlan-id"])
			break
		}
	}
	if n.VLAN == "" && len(vlans) > 0 {
		n.VLAN = str(vlans[0]["vlan-id"])
	}
	return n
}
