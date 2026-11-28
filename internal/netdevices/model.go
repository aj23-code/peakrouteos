// Package netdevices watches the network equipment that is not the router itself: managed switches and access points (docs/device-pages.md,
// phase 3). Each watched device is polled through a driver that fills in one record, whatever the protocol: PeakRoute equipment through its
// own API, anything else through SNMP (read-only). The pages read the record, so the SDN controller that is to come is one more driver.
package netdevices

import (
	"context"
	"time"
)

// Kinds of watched device: which driver reads it.
const (
	KindSNMP = "snmp"
	KindAP   = "peakroute-ap"
)

// Device is a piece of equipment to watch. The secrets in it are never sent to a page (the API says only that they are set).
type Device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Address string    `json:"address"` // an IPv4 address or a host name
	Kind    string    `json:"kind"`
	Enabled bool      `json:"enabled"`
	SNMP    *SNMPAuth `json:"snmp,omitempty"`
	AP      *APAuth   `json:"ap,omitempty"`
}

// SNMPAuth is how to ask a device over SNMP: v2c with a community (sent in clear text) or v3 with a user and, usually, authentication and
// encryption.
type SNMPAuth struct {
	Version   string `json:"version"` // "2c" or "3"
	Port      int    `json:"port,omitempty"`
	Community string `json:"community,omitempty"`
	User      string `json:"user,omitempty"`
	AuthProto string `json:"auth_proto,omitempty"` // "", "MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512"
	AuthPass  string `json:"auth_pass,omitempty"`
	PrivProto string `json:"priv_proto,omitempty"` // "", "DES", "AES", "AES192", "AES256"
	PrivPass  string `json:"priv_pass,omitempty"`
}

// APAuth is how to ask a PeakRoute AP: its monitoring token, and the fingerprint of its certificate (it is checked against that, not
// against a certificate authority: the AP makes its own).
type APAuth struct {
	Port        int    `json:"port,omitempty"`
	Token       string `json:"token,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"` // SHA-256 of the certificate, hex
}

// Neighbour is what a port's far end has said it is (LLDP).
type Neighbour struct {
	Name         string   `json:"name,omitempty"`
	ChassisID    string   `json:"chassis_id,omitempty"`
	PortID       string   `json:"port_id,omitempty"`
	PortDesc     string   `json:"port_desc,omitempty"`
	Description  string   `json:"description,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"` // Bridge, Router, WLAN AP...
}

// Bridging says whether the neighbour forwards for others (a switch, a router, an access point): what is behind it is further away.
func (n *Neighbour) Bridging() bool {
	if n == nil {
		return false
	}
	for _, c := range n.Capabilities {
		switch c {
		case "Bridge", "Router", "WLAN AP", "Wlan":
			return true
		}
	}
	return false
}

// Port is one port of a watched device (or, for an access point, one radio).
type Port struct {
	Index     int        `json:"index"`
	Name      string     `json:"name"`
	Alias     string     `json:"alias,omitempty"` // what the administrator called it there
	Up        bool       `json:"up"`
	SpeedMbps int        `json:"speed_mbps,omitempty"`
	VLAN      int        `json:"vlan,omitempty"`
	InBytes   uint64     `json:"in_bytes"`
	OutBytes  uint64     `json:"out_bytes"`
	Errors    uint64     `json:"errors,omitempty"`
	MACs      int        `json:"macs"` // how many hardware addresses it has learned
	Neighbour *Neighbour `json:"neighbour,omitempty"`
}

// Client is a device a watched device can see, and where: on which port (a switch) or radio (an access point).
type Client struct {
	MAC    string `json:"mac"`
	Port   string `json:"port"` // the port's name, or the radio's
	VLAN   int    `json:"vlan,omitempty"`
	SSID   string `json:"ssid,omitempty"`
	Band   string `json:"band,omitempty"`
	Signal *int   `json:"signal_dbm,omitempty"`
}

// Record is what one poll found.
type Record struct {
	Name        string    `json:"name,omitempty"` // what the device calls itself
	Description string    `json:"description,omitempty"`
	UptimeSec   int64     `json:"uptime_seconds,omitempty"`
	Ports       []Port    `json:"ports"`
	Clients     []Client  `json:"clients"`
	PolledAt    time.Time `json:"polled_at"`
}

// Driver reads a device.
type Driver interface {
	Poll(ctx context.Context, d Device) (Record, error)
}
