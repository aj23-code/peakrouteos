package system

import (
	"os"
	"path/filepath"
	"testing"
)

// sysfs lays out a fake /sys: zones as type=milli-degrees, chips as name and their inputs (file name to content).
type chip struct {
	name   string
	inputs map[string]string
}

func sysfs(t *testing.T, zones map[string]string, chips ...chip) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	i := 0
	// a map has no order: name them by their type so that "zone0" is not always the same one
	for _, kind := range sortedKeys(zones) {
		z := filepath.Join(root, "class/thermal", "thermal_zone"+string(rune('0'+i)))
		write(filepath.Join(z, "type"), kind)
		write(filepath.Join(z, "temp"), zones[kind])
		i++
	}
	for n, c := range chips {
		d := filepath.Join(root, "class/hwmon", "hwmon"+string(rune('0'+n)))
		write(filepath.Join(d, "name"), c.name)
		for f, v := range c.inputs {
			write(filepath.Join(d, f), v)
		}
	}
	return root
}

func sortedKeys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	for i := range k {
		for j := i + 1; j < len(k); j++ {
			if k[j] < k[i] {
				k[i], k[j] = k[j], k[i]
			}
		}
	}
	return k
}

func TestThePlaceholderZoneIsNotTheCPU(t *testing.T) {
	// the Dell: zone 0 is acpitz at a fixed 25 C; the package sensor is another zone
	root := sysfs(t, map[string]string{"acpitz": "25000", "pch_skylake": "45000", "iwlwifi_1": "41000", "x86_pkg_temp": "52000", "INT3400 Thermal": "20000"})
	if c, ok := cpuTemperature(root); !ok || c != 52 {
		t.Fatalf("%v %v", c, ok)
	}
	// zones are looked for in order of how sure they are
	root = sysfs(t, map[string]string{"cpu_thermal": "61000", "x86_pkg_temp": "55000"})
	if c, _ := cpuTemperature(root); c != 55 {
		t.Fatalf("%v", c)
	}
	root = sysfs(t, map[string]string{"soc_thermal": "48500"})
	if c, ok := cpuTemperature(root); !ok || c != 48.5 {
		t.Fatalf("%v %v", c, ok)
	}
	// a zone that reads nonsense (0, or 200 degrees, or not a number) is not the answer
	for _, v := range []string{"0", "200000", "-5000", "x"} {
		root = sysfs(t, map[string]string{"x86_pkg_temp": v})
		if c, ok := cpuTemperature(root); ok {
			t.Errorf("%q gave %v", v, c)
		}
	}
	root = sysfs(t, map[string]string{"x86_pkg_temp": "0", "cpu_thermal": "47000"})
	if c, _ := cpuTemperature(root); c != 47 {
		t.Fatalf("the next sensor was not tried: %v", c)
	}
}

func TestHwmonChipsOfAProcessor(t *testing.T) {
	// no thermal zone for the processor: the coretemp chip, its package sensor first
	root := sysfs(t, map[string]string{"acpitz": "25000"},
		chip{"nvme", map[string]string{"temp1_input": "38000"}},
		chip{"coretemp", map[string]string{"temp1_input": "60000", "temp1_label": "Package id 0", "temp2_input": "63000", "temp2_label": "Core 0"}})
	if c, ok := cpuTemperature(root); !ok || c != 60 {
		t.Fatalf("%v %v", c, ok)
	}
	// without labels: the hottest the chip has
	root = sysfs(t, nil, chip{"coretemp", map[string]string{"temp1_input": "50000", "temp2_input": "58000", "temp3_input": "0"}})
	if c, _ := cpuTemperature(root); c != 58 {
		t.Fatalf("%v", c)
	}
	// AMD
	root = sysfs(t, nil, chip{"k10temp", map[string]string{"temp1_input": "45500", "temp1_label": "Tctl", "temp3_input": "40000", "temp3_label": "Tccd1"}})
	if c, _ := cpuTemperature(root); c != 45.5 {
		t.Fatalf("%v", c)
	}
}

func TestWithoutASensorThereIsNoTemperature(t *testing.T) {
	// only the placeholder, a disk and a wifi card: nothing says "processor", so nothing is claimed
	root := sysfs(t, map[string]string{"acpitz": "25000", "iwlwifi_1": "41000"}, chip{"nvme", map[string]string{"temp1_input": "39000"}}, chip{"BAT0", map[string]string{"temp1_input": "30000"}})
	if c, ok := cpuTemperature(root); ok {
		t.Fatalf("%v", c)
	}
	if _, ok := cpuTemperature(t.TempDir()); ok {
		t.Fatal("a machine with no sysfs has a temperature")
	}
	// zones that are not known to be something else are the fallback: the hottest
	root = sysfs(t, map[string]string{"acpitz": "25000", "SEN1": "41000", "SEN2": "47000", "nvme": "70000"})
	if c, ok := cpuTemperature(root); !ok || c != 47 {
		t.Fatalf("%v %v", c, ok)
	}
}

func TestTheRealMachine(t *testing.T) {
	// whatever this machine has, the answer is a plausible number or no answer, never a made-up zero
	if c, ok := cpuTemperature("/sys"); ok && !plausible(c) {
		t.Fatalf("%v", c)
	}
}

func TestMemoryIsWhatIsAvailable(t *testing.T) {
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "meminfo")
		os.WriteFile(p, []byte(content), 0644)
		return p
	}
	// a kernel that says MemAvailable: that is what is free, whatever the sum of the easy parts is
	with := write("MemTotal:       8000000 kB\nMemFree:         500000 kB\nMemAvailable:   5000000 kB\nBuffers:         100000 kB\nCached:         1000000 kB\nSReclaimable:    200000 kB\nSwapTotal:      2048000 kB\nSwapFree:       1024000 kB\n")
	var s SystemStats
	if err := parseMemInfoFile(with, &s); err != nil {
		t.Fatal(err)
	}
	if s.MemoryTotalMB != 7812 || s.MemoryFreeMB != 4882 || s.MemoryUsedMB != 2929 || s.SwapTotalMB != 2000 || s.SwapUsedMB != 1000 {
		t.Fatalf("%+v", s)
	}
	// one that does not: the sum of free, buffers, cache and what can be reclaimed
	without := write("MemTotal:       8000000 kB\nMemFree:         500000 kB\nBuffers:         100000 kB\nCached:         1000000 kB\nSReclaimable:    200000 kB\n")
	s = SystemStats{}
	parseMemInfoFile(without, &s)
	if want := uint64(1800000 / 1024); s.MemoryFreeMB != want {
		t.Fatalf("%d want %d", s.MemoryFreeMB, want)
	}
	// an estimate larger than the memory is not believed
	odd := write("MemTotal:  1000 kB\nMemAvailable:  9000 kB\n")
	s = SystemStats{}
	parseMemInfoFile(odd, &s)
	if s.MemoryUsedMB != 0 || s.MemoryFreeMB != s.MemoryTotalMB {
		t.Fatalf("%+v", s)
	}
	if err := parseMemInfoFile(filepath.Join(t.TempDir(), "nothing"), &s); err == nil {
		t.Fatal("a missing file is no error")
	}
}
