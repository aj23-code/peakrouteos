package network

import (
	"os"
	"strings"
	"testing"
)

// testdata/nft-ruleset.json is the real output of `nft -j list ruleset` (nft 1.0.9) with the rules of the firewall
// design loaded.
func TestParseFirewallCountersOfARealRuleset(t *testing.T) {
	data, err := os.ReadFile("testdata/nft-ruleset.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseFirewallCounters(data)
	if err != nil {
		t.Fatal(err)
	}
	// fw1, fw2, fw3, fw4 (in two chains), pf1, pf2 (two rules each): every id with a counter
	for _, id := range []string{"fw1", "fw2", "fw3", "fw4", "pf1", "pf2"} {
		if _, ok := got[id]; !ok {
			t.Errorf("no counter for %s: %v", id, got)
		}
	}
	if _, ok := got["fw5"]; ok {
		t.Error("a switched-off rule is not in the ruleset")
	}
	if len(got) != 6 {
		t.Errorf("%v", got)
	}
}

func TestParseFirewallCountersAddsUpARuleInTwoChains(t *testing.T) {
	js := `{"nftables": [
	  {"rule": {"comment": "fw:fw4", "chain": "input",   "expr": [{"match": {}}, {"counter": {"packets": 3, "bytes": 300}}, {"reject": null}]}},
	  {"rule": {"comment": "fw:fw4", "chain": "forward", "expr": [{"counter": {"packets": 4, "bytes": 400}}, {"reject": null}]}},
	  {"rule": {"comment": "fw:pf1", "expr": [{"counter": {"packets": 7, "bytes": 70}}, {"dnat": {}}]}},
	  {"rule": {"comment": "someone else's", "expr": [{"counter": {"packets": 99, "bytes": 99}}]}},
	  {"rule": {"expr": [{"counter": {"packets": 98, "bytes": 98}}]}},
	  {"rule": {"comment": "fw:nocounter", "expr": [{"accept": null}]}},
	  {"table": {"name": "x"}}
	]}`
	got, err := ParseFirewallCounters([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if got["fw4"] != (RuleCounter{7, 700}) || got["pf1"] != (RuleCounter{7, 70}) || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestParseFirewallCountersRefusesJunk(t *testing.T) {
	if _, err := ParseFirewallCounters([]byte("not json")); err == nil {
		t.Fatal("junk accepted")
	}
	if got, err := ParseFirewallCounters([]byte(`{"nftables": []}`)); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if !strings.Contains(string(mustRead(t, "testdata/nft-ruleset.json")), "fw:fw1") {
		t.Fatal("the sample is not a firewall ruleset")
	}
}

func mustRead(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
