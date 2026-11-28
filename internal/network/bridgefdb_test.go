package network

import (
	"context"
	"errors"
	"testing"
)

var errNoBridge = errors.New("no bridge")

const fdbSample = `[
 {"mac":"01:80:c2:00:00:0e","dev":"eth1","vlan":1,"flags":[],"state":"permanent"},
 {"mac":"52:54:00:aa:bb:cc","ifname":"eth1","master":"br0","state":"permanent","flags":[]},
 {"mac":"a8:bb:cc:00:00:01","ifname":"eth1","master":"br0","vlan":1,"flags":["master"],"state":""},
 {"mac":"a8:bb:cc:00:00:02","ifname":"eth2","master":"br0","vlan":20,"flags":["master"],"state":""},
 {"mac":"a8:bb:cc:00:00:03","ifname":"wlan0","master":"br0","vlan":1,"flags":["master"],"state":"static"},
 {"mac":"33:33:00:00:00:01","ifname":"eth1","master":"br0","state":"static","flags":[]},
 {"mac":"ff:ff:ff:ff:ff:ff","ifname":"eth1","master":"br0","state":"static","flags":[]},
 {"mac":"a8:bb:cc:00:00:04","ifname":"br0","master":"br0","state":"permanent","flags":[]},
 {"mac":"a8:bb:cc:00:00:05","ifname":"eth3","flags":["self"],"state":""}
]`

func TestTheBridgeTableNamesThePortEachLearnedAddressIsBehind(t *testing.T) {
	m := ParseBridgeFDB(fdbSample)
	if got := m["a8:bb:cc:00:00:01"]; got.Port != "eth1" || got.Bridge != "br0" || got.VLAN != 1 {
		t.Errorf("%+v", got)
	}
	if got := m["a8:bb:cc:00:00:02"]; got.Port != "eth2" || got.VLAN != 20 {
		t.Errorf("%+v", got)
	}
	if got := m["a8:bb:cc:00:00:03"]; got.Port != "wlan0" {
		t.Errorf("%+v", got)
	}
	// the ports' own addresses, multicast and broadcast, the bridge itself and entries that no bridge learned are not devices
	for _, mac := range []string{"01:80:c2:00:00:0e", "52:54:00:aa:bb:cc", "33:33:00:00:00:01", "ff:ff:ff:ff:ff:ff", "a8:bb:cc:00:00:04", "a8:bb:cc:00:00:05"} {
		if _, ok := m[mac]; ok {
			t.Errorf("%s is listed: %+v", mac, m[mac])
		}
	}
	if len(m) != 3 {
		t.Errorf("%+v", m)
	}
	if ParseBridgeFDB("not json") != nil || len(ParseBridgeFDB("[]")) != 0 {
		t.Error("garbage or nothing")
	}
}

type failRun struct{}

func (failRun) Run(ctx context.Context, name string, args ...string) (string, error) {
	return "", errNoBridge
}

func TestAMachineWithoutABridgeHasNoAttachments(t *testing.T) {
	if m := ReadBridgeFDB(context.Background(), failRun{}); m == nil || len(m) != 0 {
		t.Errorf("%+v", m)
	}
}
