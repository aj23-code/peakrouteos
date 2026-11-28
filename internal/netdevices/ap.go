package netdevices

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validateAP checks the access point part of a device.
func validateAP(d Device, add func(field, msg string)) {
	a := d.AP
	if a == nil {
		add("ap", "required for an access point")
		return
	}
	if a.Port < 0 || a.Port > 65535 {
		add("ap.port", "must be between 1 and 65535")
	}
	if len(a.Token) < 20 || len(a.Token) > 200 || !plain.MatchString(a.Token) {
		add("ap.token", "required: the monitoring token from the access point's page")
	}
	if a.Fingerprint != "" && !fingerprintPattern.MatchString(a.Fingerprint) {
		add("ap.fingerprint", "must be 64 hexadecimal characters")
	}
}

// APDriver reads a PeakRoute access point through its status API (docs/device-pages.md, section 4a): HTTPS with the monitoring token as a
// bearer token. The access point makes its own certificate, so it is checked against the fingerprint remembered from the first answer
// (trust on first use) and not against an authority; a certificate that later differs is refused until the page is told to trust it.
type APDriver struct {
	// Pin is called with the fingerprint of the certificate seen on the first successful answer, to be stored with the device.
	Pin func(id, fingerprint string)
	// Timeout is how long a poll may take (default 8 s).
	Timeout time.Duration
}

// apStatus is the part of the access point's GET /api/v1/ap/status that is used.
type apStatus struct {
	Ports []struct {
		Name      string  `json:"name"`
		Kind      string  `json:"kind"`
		State     string  `json:"state"`
		Speed     string  `json:"speed"`
		Connected *bool   `json:"connected"`
		SSID      string  `json:"ssid"`
		SignalDBm *int    `json:"signal_dbm"`
		Band      string  `json:"band"`
		RxMbps    float64 `json:"rx_mbps"`
	} `json:"ports"`
	Radios []struct {
		Name    string `json:"name"`
		Band    string `json:"band"`
		Channel int    `json:"channel"`
		Up      bool   `json:"up"`
		Clients int    `json:"clients"`
	} `json:"radios"`
	Devices []struct {
		MAC       string `json:"mac"`
		Interface string `json:"interface"`
		SSID      string `json:"ssid"`
		Band      string `json:"band"`
		Signal    *int   `json:"signal_dbm"`
		RxBytes   uint64 `json:"rx_bytes"`
		TxBytes   uint64 `json:"tx_bytes"`
	} `json:"devices"`
	DevicesKnown bool `json:"devices_known"`
}

// ErrCertificateChanged is what a poll says when the access point shows a different certificate than the one remembered.
var ErrCertificateChanged = errors.New("the access point's certificate is not the one that was trusted (it may have been renewed: trust the new one on the device's page)")

// Poll asks the access point for its status.
func (p *APDriver) Poll(ctx context.Context, d Device) (Record, error) {
	a := d.AP
	if a == nil {
		return Record{}, errors.New("no access point settings")
	}
	port := a.Port
	if port == 0 {
		port = 443
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	var seen string
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // the certificate is checked below, against the fingerprint
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				if len(raw) == 0 {
					return errors.New("no certificate")
				}
				sum := sha256.Sum256(raw[0])
				seen = hex.EncodeToString(sum[:])
				if a.Fingerprint != "" && subtle.ConstantTimeCompare([]byte(seen), []byte(a.Fingerprint)) != 1 {
					return ErrCertificateChanged
				}
				return nil
			},
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort(d.Address, strconv.Itoa(port))+"/api/v1/ap/status", nil)
	if err != nil {
		return Record{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		if errors.Is(err, ErrCertificateChanged) {
			return Record{}, ErrCertificateChanged
		}
		return Record{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Record{}, errors.New("the access point refused the monitoring token (it was revoked or replaced)")
	case resp.StatusCode != http.StatusOK:
		return Record{}, fmt.Errorf("the access point answered %d", resp.StatusCode)
	}
	var st apStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st); err != nil {
		return Record{}, fmt.Errorf("the access point's answer could not be read: %w", err)
	}
	if a.Fingerprint == "" && seen != "" && p.Pin != nil {
		p.Pin(d.ID, seen) // the token was accepted over this certificate: remember it
	}
	return apRecord(st), nil
}

// speedMbps reads a link speed such as "1000Mb/s" or "2.5Gb/s".
func speedMbps(s string) int {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	if strings.HasPrefix(strings.ToLower(s[i:]), "g") {
		v *= 1000
	}
	return int(v)
}

// radioOf is the adapter a station's interface belongs to (its first network is on the adapter's own name, the others on name_1, name_2...).
func radioOf(iface string) string {
	if i := strings.LastIndex(iface, "_"); i > 0 {
		if _, err := strconv.Atoi(iface[i+1:]); err == nil {
			return iface[:i]
		}
	}
	return iface
}

func apRecord(st apStatus) Record {
	r := Record{Ports: []Port{}, Clients: []Client{}}
	for i, p := range st.Ports {
		port := Port{Index: i + 1, Name: p.Name, SpeedMbps: speedMbps(p.Speed)}
		if p.Kind == "link" {
			port.Up = p.Connected != nil && *p.Connected
			port.Alias = "Wi-Fi link to " + p.SSID
		} else {
			port.Up = strings.EqualFold(p.State, "up")
		}
		r.Ports = append(r.Ports, port)
	}
	counts := map[string]int{}
	for _, d := range st.Devices {
		counts[radioOf(d.Interface)]++
	}
	for _, rd := range st.Radios {
		alias := rd.Band
		if rd.Channel > 0 {
			alias += fmt.Sprintf(" channel %d", rd.Channel)
		}
		r.Ports = append(r.Ports, Port{Index: len(r.Ports) + 1, Name: rd.Name, Alias: strings.TrimSpace(alias), Up: rd.Up, MACs: counts[rd.Name]})
	}
	for _, d := range st.Devices {
		mac := strings.ToLower(d.MAC)
		if mac == "" {
			continue
		}
		r.Clients = append(r.Clients, Client{MAC: mac, Port: radioOf(d.Interface), SSID: d.SSID, Band: d.Band, Signal: d.Signal})
	}
	return r
}
