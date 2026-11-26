package netapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/network"
)

func TestTheShapingStartsOffAndIsSavedToTheDraftInMbit(t *testing.T) {
	e := standard(t)
	v := decodeInto[sqmResponse](t, e.get(e.api.HandleSQM))
	if v.Enabled || v.Pending || v.Running || !v.PerHost || v.Link != "ethernet" || len(v.Links) != 4 || v.Status != nil || !v.Editable {
		t.Fatalf("%+v", v)
	}
	rec := e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true,"download_mbit":450,"upload_mbit":37.5,"link":"docsis"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft()
	if s := d.SQMSettings(); !s.Enabled || s.DownloadKbit != 450000 || s.UploadKbit != 37500 || s.Link != "docsis" || !s.PerHost {
		t.Fatalf("%+v", s)
	}
	v = decodeInto[sqmResponse](t, e.get(e.api.HandleSQM))
	if !v.Pending || v.Running || v.DownloadMbit != 450 || v.UploadMbit != 37.5 {
		t.Fatalf("%+v", v)
	}
	// a request that says nothing about a setting leaves it alone
	if rec := e.do(e.api.HandleSQM, http.MethodPut, `{"per_host":false}`, "application/json"); rec.Code != http.StatusOK || e.draft().SQMSettings().DownloadKbit != 450000 || e.draft().SQMSettings().PerHost {
		t.Errorf("%d", rec.Code)
	}
}

func TestShapingWithNoSpeedAtAllIsRefusedOnTheDownloadField(t *testing.T) {
	e := standard(t)
	rec := e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true}`, "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"download_mbit"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true,"download_mbit":20000}`, "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "download_mbit") || !strings.Contains(rec.Body.String(), "10000 Mbit/s") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	rec = e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true,"upload_mbit":10,"link":"pigeon"}`, "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"link"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestTheShapingStatusIsOnlyAskedForWhileItRuns(t *testing.T) {
	e := standard(t)
	asked := 0
	e.api.SQMStatus = func(context.Context) network.SQMStatus {
		asked++
		return network.SQMStatus{Upload: network.SQMDirection{Active: true, BandwidthKbit: 40000}}
	}
	e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true,"upload_mbit":40}`, "application/json")
	if v := decodeInto[sqmResponse](t, e.get(e.api.HandleSQM)); v.Status != nil || asked != 0 {
		t.Fatalf("a draft is not running: %+v asked %d", v, asked)
	}
	e.commit()
	v := decodeInto[sqmResponse](t, e.get(e.api.HandleSQM))
	if !v.Running || v.Pending || v.Status == nil || !v.Status.Upload.Active || v.Status.Upload.BandwidthKbit != 40000 || asked != 1 {
		t.Fatalf("%+v asked %d", v, asked)
	}
}
