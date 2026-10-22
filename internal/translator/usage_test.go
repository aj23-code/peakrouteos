package translator

import (
	"strings"
	"testing"
)

// withoutUsageTable is a ruleset without the table that counts what each device uses, for the tests that are about the rules.
func withoutUsageTable(ruleset string) string {
	if i := strings.Index(ruleset, "\ntable inet "+UsageTable); i >= 0 {
		return ruleset[:i]
	}
	return ruleset
}

func TestTheRulesetCountsWhatEachDeviceUses(t *testing.T) {
	got := content(t, routerCfg(), PathNftables)
	for _, want := range []string{
		"table inet peakroute_acct {",
		"set up4 { type ipv4_addr; flags dynamic,timeout; timeout 10m; size 8192; counter; }",
		"set down6 { type ipv6_addr; flags dynamic,timeout; timeout 10m; size 8192; counter; }",
		"type filter hook forward priority 10; policy accept;",
		`iifname != "eth0" oifname "eth0" add @up4 { ip saddr }`,
		`iifname "eth0" oifname != "eth0" add @down4 { ip daddr }`,
		`iifname != "eth0" oifname "eth0" add @up6 { ip6 saddr }`,
		`iifname "eth0" oifname != "eth0" add @down6 { ip6 daddr }`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the ruleset:\n%s", want, got)
		}
	}
	// after the firewall, so what the rules drop is not counted
	if strings.Index(got, "table inet filter") > strings.Index(got, "table inet peakroute_acct") {
		t.Errorf("the usage table comes before the firewall")
	}
}

func TestAnAccessPointCountsNothing(t *testing.T) {
	if strings.Contains(text(t, apCfg(nil), PathNftables), UsageTable) {
		t.Error("an access point forwards nothing and has nothing to count")
	}
}
