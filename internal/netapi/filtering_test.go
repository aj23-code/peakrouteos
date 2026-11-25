package netapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/filtering"
)

func TestTheFilteringPageSaysWhichNetworksFilterNowAndInTheDraft(t *testing.T) {
	e := standard(t)
	if rec := e.create(newGuest); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.api.FilteringStatus = func(context.Context) filtering.Status {
		return filtering.Status{Installed: true, Running: true, Version: "v1.0.0", UIPort: "3000"}
	}
	view := func() filteringResponse { return decodeInto[filteringResponse](t, e.get(e.api.HandleFiltering)) }
	if v := view(); v.Enabled || v.Pending || len(v.Networks) != 2 || !v.Installed || v.Version != "v1.0.0" {
		t.Fatalf("%+v", v)
	}
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","filter":true}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := view()
	if v.Enabled || !v.Pending || !v.Networks[1].Filter || v.Networks[1].Running {
		t.Fatalf("in the draft only: %+v", v)
	}
	e.commit()
	v = view()
	if !v.Enabled || v.Pending || !v.Networks[1].Running {
		t.Fatalf("after the commit: %+v", v)
	}
	if rec := e.net(http.MethodPut, "lan", `{"ip":"192.168.1.1","netmask":"255.255.255.0","filter":true}`); rec.Code != http.StatusOK || !e.draft().LanFilter {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v := view(); !v.Pending || !v.Networks[0].Filter || v.Networks[0].Running {
		t.Fatalf("%+v", v)
	}
}

func TestANetworkThatFiltersRefusesOtherDNSServersAndTheOtherWayRound(t *testing.T) {
	e := standard(t)
	if rec := e.create(newGuest); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","filter":true}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft()
	d.Networks[0].DNSServers = []string{"1.1.1.1"}
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "filters ads") {
		t.Errorf("%v", err)
	}
}
