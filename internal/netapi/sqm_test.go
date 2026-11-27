package netapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"peakroute/internal/network"
	"peakroute/internal/speedtest"
)

func TestTheShapingStartsOffAndIsSavedToTheDraftInMbit(t *testing.T) {
	e := standard(t)
	v := decodeInto[sqmResponse](t, e.get(e.api.HandleSQM))
	if v.Enabled || v.Pending || v.Running || !v.PerHost || v.Link != "ethernet" || len(v.Links) != 4 || v.Status != nil || !v.Editable {
		t.Fatalf("%+v", v)
	}
	rec := e.do(e.api.HandleSQM, http.MethodPut, `{"enabled":true,"download_mbit":500,"upload_mbit":40,"link":"docsis"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft()
	if s := d.SQMSettings(); !s.Enabled || s.DownloadKbit != 500000 || s.UploadKbit != 40000 || s.Link != "docsis" || !s.PerHost || s.Percent != 0 {
		t.Fatalf("%+v", s)
	}
	v = decodeInto[sqmResponse](t, e.get(e.api.HandleSQM))
	// the plan's speeds, the default 95 % of them, and what the router will shape to
	if !v.Pending || v.Running || v.DownloadMbit != 500 || v.UploadMbit != 40 || v.Percent != 95 || v.DefaultPercent != 95 || v.ShapedDownMbit != 475 || v.ShapedUpMbit != 38 {
		t.Fatalf("%+v", v)
	}
	if rec := e.do(e.api.HandleSQM, http.MethodPut, `{"percent":90}`, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v = decodeInto[sqmResponse](t, e.get(e.api.HandleSQM)); v.Percent != 90 || v.ShapedDownMbit != 450 || e.draft().SQMSettings().Percent != 90 {
		t.Fatalf("%+v", v)
	}
	// saying the default is not saved as a setting of its own
	e.do(e.api.HandleSQM, http.MethodPut, `{"percent":95}`, "application/json")
	if e.draft().SQM.Percent != 0 {
		t.Errorf("the default percentage was written: %d", e.draft().SQM.Percent)
	}
	if rec := e.do(e.api.HandleSQM, http.MethodPut, `{"percent":30}`, "application/json"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"percent"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	// a request that says nothing about a setting leaves it alone
	if rec := e.do(e.api.HandleSQM, http.MethodPut, `{"per_host":false}`, "application/json"); rec.Code != http.StatusOK || e.draft().SQMSettings().DownloadKbit != 500000 || e.draft().SQMSettings().PerHost {
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

func TestTheSpeedTestRunsOnceAtATimeAndChangesNothingInTheConfiguration(t *testing.T) {
	e := standard(t)
	if rec := e.do(e.api.HandleSQMSpeedTest, http.MethodPost, ``, ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("no tester: %d", rec.Code)
	}
	release := make(chan struct{})
	e.api.SpeedTest = func(ctx context.Context, progress func(string)) (speedtest.Result, error) {
		progress("download")
		<-release
		return speedtest.Result{DownloadMbit: 468.2, UploadMbit: 37.1, Server: "x"}, nil
	}
	if v := decodeInto[speedResponse](t, e.get(e.api.HandleSQMSpeedTest)); v.State != "idle" {
		t.Fatalf("%+v", v)
	}
	rec := e.do(e.api.HandleSQMSpeedTest, http.MethodPost, ``, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.do(e.api.HandleSQMSpeedTest, http.MethodPost, ``, ""); rec.Code != http.StatusConflict {
		t.Errorf("a second test at once: %d", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := decodeInto[speedResponse](t, e.get(e.api.HandleSQMSpeedTest)); v.State == "running" && v.Phase == "download" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	var v speedResponse
	for time.Now().Before(deadline) {
		if v = decodeInto[speedResponse](t, e.get(e.api.HandleSQMSpeedTest)); v.State == "done" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v.State != "done" || v.Result == nil || v.Result.DownloadMbit != 468.2 || v.Result.UploadMbit != 37.1 {
		t.Fatalf("%+v", v)
	}
	if e.draft().SQM != nil {
		t.Errorf("a speed test changed the configuration: %+v", e.draft().SQM)
	}
	// a failure is reported, and the next test may start
	e.api.SpeedTest = func(context.Context, func(string)) (speedtest.Result, error) {
		return speedtest.Result{}, errors.New("no route")
	}
	e.do(e.api.HandleSQMSpeedTest, http.MethodPost, ``, "")
	for time.Now().Before(deadline.Add(time.Second)) {
		if v = decodeInto[speedResponse](t, e.get(e.api.HandleSQMSpeedTest)); v.State == "error" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v.State != "error" || !strings.Contains(v.Error, "no route") || v.Result != nil {
		t.Errorf("%+v", v)
	}
}
