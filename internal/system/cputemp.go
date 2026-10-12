package system

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The CPU temperature. A machine has many sensors and the first one is often not the processor: on many boards thermal_zone0 is
// "acpitz", a placeholder that reads a fixed 25 C. So the sensor is chosen by what it says it is: the zones and hwmon chips that
// belong to a processor, in order of how sure that is; failing those, the hottest of the rest that is not a known placeholder;
// failing that, nothing (the page shows no temperature rather than a wrong one).

// processor sensors in order of preference: thermal zone types, then hwmon chip names
var cpuZoneTypes = []string{"x86_pkg_temp", "coretemp", "k10temp", "zenpower", "cpu_thermal", "cpu-thermal", "soc_thermal", "soc-thermal", "cpu"}
var cpuChipNames = []string{"coretemp", "k10temp", "zenpower", "cpu_thermal", "cpu-thermal", "soc_thermal"}

// zones that are not the processor and would mislead as a fallback
var notTheCPU = []string{"nvme", "iwlwifi", "wifi", "battery", "bat", "amdgpu", "nouveau", "pch", "int3400", "acpitz", "gpu", "ddr", "tmem"}

func plausible(c float64) bool { return c > 1 && c < 125 }

func readMilli(path string) (float64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	m, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		return 0, false
	}
	return m / 1000, true
}

func readText(path string) string {
	raw, _ := os.ReadFile(path)
	return strings.TrimSpace(string(raw))
}

// cpuTemperature looks in sysfs (a parameter so that a test can lay one out) and returns the temperature and whether there is one.
func cpuTemperature(sysfs string) (float64, bool) {
	zones, _ := filepath.Glob(filepath.Join(sysfs, "class/thermal/thermal_zone*"))
	type zone struct {
		kind string
		c    float64
	}
	var all []zone
	for _, z := range zones {
		if c, ok := readMilli(filepath.Join(z, "temp")); ok {
			all = append(all, zone{strings.ToLower(readText(filepath.Join(z, "type"))), c})
		}
	}
	for _, want := range cpuZoneTypes {
		for _, z := range all {
			if z.kind == want && plausible(z.c) {
				return z.c, true
			}
		}
	}

	// hwmon chips: the package sensor when it is labelled, otherwise the hottest the chip has
	chips, _ := filepath.Glob(filepath.Join(sysfs, "class/hwmon/hwmon*"))
	for _, want := range cpuChipNames {
		for _, chip := range chips {
			if strings.ToLower(readText(filepath.Join(chip, "name"))) != want {
				continue
			}
			inputs, _ := filepath.Glob(filepath.Join(chip, "temp*_input"))
			best, have := 0.0, false
			for _, in := range inputs {
				c, ok := readMilli(in)
				if !ok || !plausible(c) {
					continue
				}
				label := strings.ToLower(readText(strings.TrimSuffix(in, "_input") + "_label"))
				if strings.Contains(label, "package") || label == "tctl" || label == "tdie" {
					return c, true
				}
				if !have || c > best {
					best, have = c, true
				}
			}
			if have {
				return best, true
			}
		}
	}

	// nothing says it is a processor: the hottest zone that is not known to be something else
	best, have := 0.0, false
	for _, z := range all {
		skip := false
		for _, n := range notTheCPU {
			if strings.HasPrefix(z.kind, n) {
				skip = true
			}
		}
		if !skip && plausible(z.c) && (!have || z.c > best) {
			best, have = z.c, true
		}
	}
	return best, have
}
