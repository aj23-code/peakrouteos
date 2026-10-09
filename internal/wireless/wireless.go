package wireless

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Runner runs a command and returns what it printed.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// Adapter is a Wi-Fi adapter and, when `iw` is available, what it can do.
type Adapter struct {
	Name              string   `json:"name"` // the network interface, e.g. wlp2s0
	Phy               string   `json:"phy"`  // the radio behind it, e.g. phy0
	MAC               string   `json:"mac"`
	Driver            string   `json:"driver"`
	State             string   `json:"state"` // the link state the kernel reports
	CapabilitiesKnown bool     `json:"capabilities_known"`
	APCapable         bool     `json:"ap_capable"`
	Modes             []string `json:"modes"`
	Bands             []Band   `json:"bands"`
}

// Report is everything the Wireless page shows.
type Report struct {
	Adapters     []Adapter `json:"adapters"`
	IWAvailable  bool      `json:"iw_available"` // false: the tool that reads capabilities is not installed
	Country      string    `json:"country"`      // the regulatory domain in effect ("00" is the world domain)
	CountryKnown bool      `json:"country_known"`
}

// Detector finds adapters under SysClassNet (the kernel's list of network interfaces).
type Detector struct {
	SysClassNet string
	Run         Runner
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func linkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// Detect lists the Wi-Fi adapters: the interfaces that have a wireless side (a "wireless" folder or a
// "phy80211" link in the kernel's description of them).
func (d Detector) Detect(ctx context.Context) Report {
	var rep Report
	rep.Adapters = []Adapter{}

	entries, _ := os.ReadDir(d.SysClassNet)
	for _, e := range entries {
		base := filepath.Join(d.SysClassNet, e.Name())
		_, errW := os.Stat(filepath.Join(base, "wireless"))
		_, errP := os.Lstat(filepath.Join(base, "phy80211"))
		if errW != nil && errP != nil {
			continue
		}
		rep.Adapters = append(rep.Adapters, Adapter{
			Name:   e.Name(),
			Phy:    linkBase(filepath.Join(base, "phy80211")),
			MAC:    readTrim(filepath.Join(base, "address")),
			Driver: linkBase(filepath.Join(base, "device", "driver")),
			State:  readTrim(filepath.Join(base, "operstate")),
			Modes:  []string{},
			Bands:  []Band{},
		})
	}
	sort.Slice(rep.Adapters, func(i, j int) bool { return rep.Adapters[i].Name < rep.Adapters[j].Name })

	if d.Run == nil {
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for i := range rep.Adapters {
		a := &rep.Adapters[i]
		if a.Phy == "" {
			continue
		}
		out, err := d.Run(ctx, "iw", "phy", a.Phy, "info")
		if err != nil {
			continue
		}
		rep.IWAvailable = true
		phy := ParsePhyInfo(out)
		a.CapabilitiesKnown = true
		a.APCapable = phy.CanAP()
		if phy.Modes != nil {
			a.Modes = phy.Modes
		}
		if phy.Bands != nil {
			a.Bands = phy.Bands
		}
	}
	if len(rep.Adapters) > 0 {
		if out, err := d.Run(ctx, "iw", "reg", "get"); err == nil {
			rep.IWAvailable = true
			rep.Country, rep.CountryKnown = ParseCountry(out)
		}
	}
	return rep
}
