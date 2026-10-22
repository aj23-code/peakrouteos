package netapi

import (
	"net/http"
	"peakroute/internal/config"
	"strings"
	"testing"
)

func (e *env) getReservations() reservationsResponse {
	e.t.Helper()
	rec := e.get(e.api.HandleReservations)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	return decodeInto[reservationsResponse](e.t, rec)
}

func TestReservationsStartEmpty(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandleReservations)
	r := decodeInto[reservationsResponse](t, rec)
	if len(r.Reservations) != 0 || r.Pending || !r.Editable {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(rec.Body.String(), `"reservations":[]`) {
		t.Errorf("an empty list must be [] not null: %s", rec.Body)
	}
}

func TestSavingReservationsStagesADraftWithTheNetworkNamed(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleReservations, `{"reservations":[
	 {"mac":"  F8-94-C2-A4-13-A7 ","ip":" 192.168.1.50 ","name":" printer "},
	 {"mac":"48:b0:2d:f1:fe:ca","ip":"192.168.1.60"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decodeInto[reservationsResponse](t, rec)
	if !r.Pending || len(r.Reservations) != 2 {
		t.Fatalf("%+v", r)
	}
	// written the one way, trimmed, with the network the address is in
	if got := r.Reservations[0]; got.MAC != "f8:94:c2:a4:13:a7" || got.IP != "192.168.1.50" || got.Name != "printer" || got.Network != "LAN" {
		t.Fatalf("%+v", got)
	}
	if len(e.mgr.NetworkConfig().Reservations) != 0 {
		t.Fatal("the active configuration changed: saving must only stage")
	}
	n := 0
	for _, c := range e.staged() {
		if c.Field == "reservations" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("staged changes: %d (%+v)", n, e.staged())
	}
}

func TestReservationErrorsNameTheRowAndField(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleReservations, `{"reservations":[
	 {"mac":"f8:94:c2:a4:13:a7","ip":"192.168.1.50"},
	 {"mac":"nonsense","ip":"10.99.0.5","name":"bad name"},
	 {"mac":"f8:94:c2:a4:13:a7","ip":"192.168.1.50"},
	 {"mac":"02:11:22:33:44:55","ip":"192.168.1.1"},
	 {"mac":"02:11:22:33:44:66","ip":"192.168.1.0"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := errorFields(t, rec)
	for _, want := range []string{"reservations[1].mac", "reservations[1].ip", "reservations[1].name", "reservations[2].mac", "reservations[2].ip",
		"reservations[3].ip", "reservations[4].ip"} {
		if !f[want] {
			t.Errorf("no error on %s: %v", want, f)
		}
	}
	if f["reservations[0].mac"] || f["reservations[0].ip"] {
		t.Errorf("the good row was blamed: %v", f)
	}
	if e.stageFileExists() {
		t.Fatal("an invalid save left a draft")
	}
}

func TestAReservationNeedsTheNetworksDhcpServerOn(t *testing.T) {
	e := standard(t)
	if err := e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.DHCP.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	rec := e.putJSON(e.api.HandleReservations, `{"reservations":[{"mac":"f8:94:c2:a4:13:a7","ip":"192.168.1.50"}]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["reservations[0].ip"] || !strings.Contains(rec.Body.String(), "DHCP server") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A change to a network that would leave a reserved address without a DHCP server to keep it (or outside the network) is refused when
// it is made, naming the address, instead of failing later at commit.
func TestNetworkEditsThatStrandAReservationAreRefusedAtOnce(t *testing.T) {
	e := standard(t)
	if rec := e.putJSON(e.api.HandleReservations, `{"reservations":[{"mac":"f8:94:c2:a4:13:a7","ip":"192.168.1.50","name":"printer"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// the default network's DHCP server off
	rec := e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":false,"range_start":"","range_end":"","lease_time":""}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "192.168.1.50") || !strings.Contains(rec.Body.String(), "DHCP page") {
		t.Fatalf("DHCP off: %d %s", rec.Code, rec.Body)
	}
	// moved to another subnet
	rec = e.putJSON(e.api.HandleNetwork, `{"ip":"10.5.0.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"10.5.0.100","range_end":"10.5.0.200","lease_time":"12h"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "192.168.1.50") {
		t.Fatalf("moved: %d %s", rec.Code, rec.Body)
	}
	// nothing was staged by the refused edits: the reservation is still valid in the draft
	if got := e.getReservations(); len(got.Reservations) != 1 {
		t.Fatalf("%+v", got)
	}
	// an edit that leaves it alone is fine
	rec = e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.100","range_end":"192.168.1.150","lease_time":"6h"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a harmless edit: %d %s", rec.Code, rec.Body)
	}
}

func TestANetworkThatHoldsAReservationCannotBeDeleted(t *testing.T) {
	e := standard(t)
	if rec := e.create(`{"name":"IoT","vlan":20,"ip":"10.20.0.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"10.20.0.100","range_end":"10.20.0.200","lease_time":"12h"}}`); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.putJSON(e.api.HandleReservations, `{"reservations":[{"mac":"48:b0:2d:f1:fe:ca","ip":"10.20.0.7","name":"tv"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := e.net(http.MethodDelete, "net2", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "10.20.0.7") || !strings.Contains(rec.Body.String(), "DHCP page") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// without the reservation it goes
	e.putJSON(e.api.HandleReservations, `{"reservations":[]}`)
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
