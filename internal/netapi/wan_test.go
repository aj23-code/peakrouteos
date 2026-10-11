package netapi

import (
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/config"
)

const pppoeWAN = `{"mode":"pppoe","pppoe":{"username":"user@isp.example","password":"s3cret-pass"}}`

func (e *env) wan() Uplink {
	e.t.Helper()
	return decodeInto[Uplink](e.t, e.uplink(http.MethodGet, ""))
}

func (e *env) saveWAN(body string) Uplink {
	e.t.Helper()
	rec := e.uplink(http.MethodPut, body)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("%s: %d %s", body, rec.Code, rec.Body)
	}
	return decodeInto[Uplink](e.t, rec)
}

func TestTheUplinkShowsItsCarrierSettingsWithTheirDefaults(t *testing.T) {
	u := standard(t).wan()
	if u.Vlan != 0 || u.MTU != 0 || u.UsualMTU != 1500 || u.MAC != "" || u.PPPoE != nil || u.Device != "eth0" {
		t.Fatalf("%+v", u)
	}
}

func TestCarrierSettingsAreSavedTrimmedAndNormalised(t *testing.T) {
	e := standard(t)
	u := e.saveWAN(`{"mode":"dhcp","vlan":835,"mtu":1400,"mac":" 02-11-22-33-44-AA "}`)
	if u.Vlan != 835 || u.MTU != 1400 || u.MAC != "02:11:22:33:44:aa" || u.Device != "eth0.835" || !u.Pending {
		t.Fatalf("%+v", u)
	}
	got := map[string]bool{}
	for _, c := range e.staged() {
		got[c.Field] = true
	}
	for _, f := range []string{"wan_vlan", "wan_mtu", "wan_mac"} {
		if !got[f] {
			t.Errorf("no review line for %s: %+v", f, e.staged())
		}
	}
	if e.mgr.NetworkConfig().WanVlan != 0 {
		t.Fatal("the active configuration changed")
	}
}

func TestWhatIsLeftOutStaysAndZeroClears(t *testing.T) {
	e := standard(t)
	e.saveWAN(`{"mode":"dhcp","vlan":835,"mtu":1400,"mac":"02:11:22:33:44:aa"}`)
	u := e.saveWAN(`{"mode":"dhcp"}`)
	if u.Vlan != 835 || u.MTU != 1400 || u.MAC != "02:11:22:33:44:aa" {
		t.Fatalf("left out, still changed: %+v", u)
	}
	u = e.saveWAN(`{"mode":"dhcp","vlan":0,"mtu":0,"mac":""}`)
	if u.Vlan != 0 || u.MTU != 0 || u.MAC != "" {
		t.Fatalf("not cleared: %+v", u)
	}
	if e.stageFileExists() {
		t.Fatal("putting everything back left a draft")
	}
}

func TestCarrierSettingErrorsNameTheirFields(t *testing.T) {
	e := standard(t)
	for name, tc := range map[string]struct{ body, field string }{
		"vlan too big":       {`{"mode":"dhcp","vlan":4095}`, "vlan"},
		"negative vlan":      {`{"mode":"dhcp","vlan":-3}`, "vlan"},
		"mtu too small":      {`{"mode":"dhcp","mtu":100}`, "mtu"},
		"mtu too big":        {`{"mode":"dhcp","mtu":10000}`, "mtu"},
		"group mac":          {`{"mode":"dhcp","mac":"01:11:22:33:44:55"}`, "mac"},
		"short mac":          {`{"mode":"dhcp","mac":"02:11:22"}`, "mac"},
		"dns override":       {`{"mode":"dhcp","dns":["nope"]}`, "dns"},
		"pppoe no user":      {`{"mode":"pppoe","pppoe":{"password":"x"}}`, "pppoe.username"},
		"pppoe no password":  {`{"mode":"pppoe","pppoe":{"username":"u"}}`, "pppoe.password"},
		"pppoe nothing":      {`{"mode":"pppoe"}`, "pppoe.username"},
		"pppoe bad user":     {`{"mode":"pppoe","pppoe":{"username":"a\"b","password":"x"}}`, "pppoe.username"},
		"pppoe bad password": {`{"mode":"pppoe","pppoe":{"username":"u","password":"a\\b"}}`, "pppoe.password"},
		"pppoe bad service":  {`{"mode":"pppoe","pppoe":{"username":"u","password":"p","service":"a\"b"}}`, "pppoe.service"},
		"pppoe mtu":          {`{"mode":"pppoe","mtu":1500,"pppoe":{"username":"u","password":"p"}}`, "mtu"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.uplink(http.MethodPut, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if f := errorFields(t, rec); !f[tc.field] {
				t.Fatalf("errors %v lack %q", f, tc.field)
			}
			if e.stageFileExists() {
				t.Fatal("an invalid save left a draft behind")
			}
		})
	}
}

func TestSavingAPPPoELoginStagesItAndNeverSendsThePasswordBack(t *testing.T) {
	e := standard(t)
	rec := e.uplink(http.MethodPut, pppoeWAN)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("the password came back: %s", rec.Body)
	}
	u := decodeInto[Uplink](t, rec)
	if u.Mode != "pppoe" || u.PPPoE == nil || u.PPPoE.Username != "user@isp.example" || !u.PPPoE.PasswordSet || u.Device != "ppp0" || u.UsualMTU != 1492 || !u.Pending {
		t.Fatalf("%+v", u)
	}
	for _, rec := range []string{e.get(e.api.HandleUplinks).Body.String(), e.uplink(http.MethodGet, "").Body.String()} {
		if strings.Contains(rec, "s3cret") {
			t.Fatalf("the password is readable: %s", rec)
		}
	}
	d := e.draft()
	if d.WanPPPoE == nil || d.WanPPPoE.Password != "s3cret-pass" || d.WanMode != "pppoe" {
		t.Fatalf("%+v", d)
	}
	// the review shows it as a password that is set, not what it is
	for _, c := range e.staged() {
		if strings.Contains(c.Old+c.New, "s3cret") {
			t.Fatalf("the password is in the review: %+v", c)
		}
	}
}

func TestThePPPoEPasswordStaysWhenLeftOutAndChangesWhenSent(t *testing.T) {
	e := standard(t)
	e.saveWAN(pppoeWAN)
	u := e.saveWAN(`{"mode":"pppoe","pppoe":{"service":"Internet"}}`)
	if u.PPPoE.Username != "user@isp.example" || u.PPPoE.Service != "Internet" || !u.PPPoE.PasswordSet {
		t.Fatalf("%+v", u.PPPoE)
	}
	if e.draft().WanPPPoE.Password != "s3cret-pass" {
		t.Fatal("the password was lost")
	}
	e.saveWAN(`{"mode":"pppoe","pppoe":{"password":"another-one"}}`)
	if e.draft().WanPPPoE.Password != "another-one" {
		t.Fatal("the password did not change")
	}
	e.saveWAN(`{"mode":"pppoe"}`) // nothing about the login: nothing changes
	if p := e.draft().WanPPPoE; p.Password != "another-one" || p.Username != "user@isp.example" {
		t.Fatalf("%+v", p)
	}
}

func TestSwitchingTheTypeDropsWhatBelongsToTheOldOne(t *testing.T) {
	e := standard(t)
	e.saveWAN(staticWAN)
	u := e.saveWAN(pppoeWAN)
	if u.IP != "" || u.Netmask != "" || u.Gateway != "" || len(u.DNS) != 0 {
		t.Fatalf("static values kept for PPPoE: %+v", u)
	}

	// PPPoE to DHCP: the login goes
	u = e.saveWAN(`{"mode":"dhcp"}`)
	if u.PPPoE != nil {
		t.Fatalf("%+v", u)
	}
	if e.draft().WanPPPoE != nil {
		t.Fatal("the login stayed in the draft")
	}

	u = e.saveWAN(staticWAN)
	if u.Mode != "static" {
		t.Fatalf("%+v", u)
	}
	// and the PPPoE settings sent with another type are dropped as well
	if u = e.saveWAN(`{"mode":"dhcp","pppoe":{"username":"u","password":"p"}}`); u.PPPoE != nil {
		t.Fatalf("%+v", u)
	}
}

func TestDNSServersCanReplaceTheProvidersWithDHCPAndPPPoE(t *testing.T) {
	e := standard(t)
	u := e.saveWAN(`{"mode":"dhcp","dns":[" 9.9.9.9 ",""," 149.112.112.112"]}`)
	if len(u.DNS) != 2 || u.DNS[0] != "9.9.9.9" || u.DNS[1] != "149.112.112.112" {
		t.Fatalf("%+v", u.DNS)
	}
	// left out: they stay
	if u = e.saveWAN(`{"mode":"dhcp","vlan":5}`); len(u.DNS) != 2 {
		t.Fatalf("%+v", u.DNS)
	}
	// they follow a switch to PPPoE when it says nothing
	if u = e.saveWAN(pppoeWAN); len(u.DNS) != 2 {
		t.Fatalf("%+v", u.DNS)
	}
	// an empty list clears them
	if u = e.saveWAN(`{"mode":"pppoe","dns":[]}`); len(u.DNS) != 0 {
		t.Fatalf("%+v", u.DNS)
	}
}

func TestStaticServersDoNotSilentlyBecomeAnOverrideWhenLeavingStatic(t *testing.T) {
	e := standard(t)
	e.saveWAN(staticWAN)
	if u := e.saveWAN(`{"mode":"dhcp"}`); len(u.DNS) != 0 {
		t.Fatalf("static servers kept as an override: %+v", u.DNS)
	}
	e.saveWAN(staticWAN)
	if u := e.saveWAN(`{"mode":"dhcp","dns":["1.1.1.1"]}`); len(u.DNS) != 1 || u.DNS[0] != "1.1.1.1" {
		t.Fatalf("%+v", u.DNS)
	}
}

func TestOnlyACarrierChangeMakesTheUplinkPending(t *testing.T) {
	c := baseline()
	e := newEnv(t, &c)
	e.saveWAN(`{"mode":"dhcp","vlan":835}`)
	e.commit()
	if e.wan().Pending {
		t.Fatal("pending right after the commit")
	}
	if !e.saveWAN(`{"mode":"dhcp","mtu":1400}`).Pending {
		t.Fatal("an MTU change is not pending")
	}
	if e.saveWAN(`{"mode":"dhcp","mtu":0}`).Pending {
		t.Fatal("put back, still pending")
	}
	// the password counts too
	e.saveWAN(pppoeWAN)
	e.commit()
	if e.wan().Pending || !e.saveWAN(`{"mode":"pppoe","pppoe":{"password":"changed-pw"}}`).Pending {
		t.Fatal("a password change must make the uplink pending")
	}
}

func TestTheLiveViewAsksAboutTheActiveConfiguration(t *testing.T) {
	c := baseline()
	c.WanVlan = 835
	e := newEnv(t, &c)
	var got config.NetworkConfig
	e.api.Live = func(active config.NetworkConfig) *Live {
		got = active
		return &Live{Up: true, Device: active.WanDevice(), MAC: "02:11:22:33:44:aa", MTU: 1500}
	}
	e.saveWAN(pppoeWAN) // a draft: not what is live
	u := e.wan()
	if got.WanVlan != 835 || got.WanKind() != "dhcp" || u.Live == nil || u.Live.Device != "eth0.835" || u.Live.MAC != "02:11:22:33:44:aa" || u.Live.MTU != 1500 {
		t.Fatalf("%+v %+v", got, u.Live)
	}
	if u.Device != "ppp0" {
		t.Fatalf("the draft's device is %q", u.Device)
	}
}

func TestPPPoEAndCarrierEditsAreRefusedWhileApplying(t *testing.T) {
	e := standard(t)
	e.editable = false
	if rec := e.uplink(http.MethodPut, pppoeWAN); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTheUplinkRequestRefusesUnknownPPPoEFields(t *testing.T) {
	e := standard(t)
	// the host name belongs to System, Settings now
	if rec := e.uplink(http.MethodPut, `{"mode":"dhcp","hostname":"gw"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("host name: %d", rec.Code)
	}
	if rec := e.uplink(http.MethodPut, `{"mode":"pppoe","pppoe":{"username":"u","password":"p","vlan":5}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
}

func TestSwitchingStraightBetweenPPPoEAndStaticOrFromDHCP(t *testing.T) {
	e := standard(t)
	e.saveWAN(pppoeWAN)
	u := e.saveWAN(staticWAN) // PPPoE to static in one step
	if u.Mode != "static" || u.PPPoE != nil || e.draft().WanPPPoE != nil {
		t.Fatalf("%+v", u)
	}
	e.saveWAN(`{"mode":"dhcp"}`)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.Hostname = "gw"; return nil })
	u = e.saveWAN(pppoeWAN) // DHCP to PPPoE in one step
	// the router's host name is not the connection's: it stays, and is simply not sent without DHCP
	if u.Mode != "pppoe" || e.draft().Hostname != "gw" {
		t.Fatalf("%+v %q", u, e.draft().Hostname)
	}
}

func TestThePasswordIsReportedSetOnlyWhenOneIsSaved(t *testing.T) {
	e := standard(t)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanMode = "pppoe"
		c.WanPPPoE = &config.PPPoEConfig{Username: "u"} // a draft that bypassed validation
		return nil
	})
	if u := e.wan(); u.PPPoE == nil || u.PPPoE.PasswordSet || u.PPPoE.Username != "u" {
		t.Fatalf("%+v", u.PPPoE)
	}
}
