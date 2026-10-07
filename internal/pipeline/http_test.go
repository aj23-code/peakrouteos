package pipeline

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func call(t *testing.T, h http.HandlerFunc, method string) (int, http.Header, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, "/x", nil))
	res := rec.Result()
	body := rec.Body.Bytes()
	return res.StatusCode, res.Header, body
}

func decode(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("response is not the expected JSON: %v\n%s", err, body)
	}
}

func TestStatusEndpointReportsTheState(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	code, hdr, body := call(t, e.svc.HandleStatus, http.MethodGet)
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", code, hdr)
	}
	var st Status
	decode(t, body, &st)
	if st.State != StateIdle || st.HasChanges {
		t.Errorf("status = %+v", st)
	}

	e.stage(lanMoved)
	_, _, body = call(t, e.svc.HandleStatus, http.MethodGet)
	decode(t, body, &st)
	if !st.HasChanges || st.ChangeCount == 0 {
		t.Errorf("the banner should be on: %+v", st)
	}
}

func TestStagedEndpointReturnsTheReview(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)

	code, _, body := call(t, e.svc.HandleStaged, http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var rev Review
	decode(t, body, &rev)
	if !rev.Valid || len(rev.Changes) == 0 || rev.Plan == nil || !rev.NeedsConfirm || rev.ConfirmSeconds != 60 {
		t.Errorf("review = %+v", rev)
	}
	foundIP := false
	for _, c := range rev.Changes {
		if c.Field == "lan_ip" && c.Old == "192.168.1.1" && c.New == "10.0.0.1" && c.Label == "LAN IP address" {
			foundIP = true
		}
	}
	if !foundIP {
		t.Errorf("changes = %+v", rev.Changes)
	}
}

func TestCommitEndpointAcceptsThenAppliesInTheBackground(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)

	code, _, body := call(t, e.svc.HandleCommit, http.MethodPost)
	if code != http.StatusAccepted {
		t.Fatalf("commit = %d: %s", code, body)
	}
	var accepted struct {
		Accepted bool   `json:"accepted"`
		Status   Status `json:"status"`
	}
	decode(t, body, &accepted)
	if !accepted.Accepted {
		t.Errorf("body = %s", body)
	}
	e.svc.Wait()

	_, _, body = call(t, e.svc.HandleStatus, http.MethodGet)
	var st Status
	decode(t, body, &st)
	if st.State != StateAwaitingConfirm || st.Confirm == nil || st.Confirm.SecondsLeft != 60 {
		t.Errorf("status after the apply = %+v", st)
	}
}

func TestCommitEndpointErrors(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	// nothing staged
	code, _, body := call(t, e.svc.HandleCommit, http.MethodPost)
	var eb errorBody
	decode(t, body, &eb)
	if code != http.StatusConflict || eb.Error == "" {
		t.Errorf("nothing staged: %d %s", code, body)
	}

	// invalid: 400 with the per-field problems
	e.stage(func(c *config.NetworkConfig) { c.LanIP = "192.168.1.255" })
	code, _, body = call(t, e.svc.HandleCommit, http.MethodPost)
	eb = errorBody{}
	decode(t, body, &eb)
	if code != http.StatusBadRequest || len(eb.Errors) == 0 || eb.Errors[0].Field != "lan_ip" {
		t.Errorf("invalid: %d %s", code, body)
	}
	if e.mgr.NetworkConfig().LanIP != "192.168.1.1" {
		t.Error("an invalid commit changed the active config")
	}

	// unsupported mode
	if err := e.mgr.DiscardNetwork(); err != nil {
		t.Fatal(err)
	}
	e.stage(leaseChanged)
	e.mode = "AP"
	code, _, body = call(t, e.svc.HandleCommit, http.MethodPost)
	eb = errorBody{}
	decode(t, body, &eb)
	if code != http.StatusBadRequest || !strings.Contains(eb.Error, "not supported") {
		t.Errorf("unsupported mode: %d %s", code, body)
	}
}

func TestConfirmRevertAndDiscardEndpoints(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	// nothing to confirm or revert yet
	for name, h := range map[string]http.HandlerFunc{"confirm": e.svc.HandleConfirm, "revert": e.svc.HandleRevert} {
		if code, _, _ := call(t, h, http.MethodPost); code != http.StatusConflict {
			t.Errorf("%s with nothing waiting = %d, want 409", name, code)
		}
	}

	e.stage(lanMoved)
	call(t, e.svc.HandleCommit, http.MethodPost)
	e.svc.Wait()

	// busy: a discard is refused while a change awaits confirmation
	if code, _, _ := call(t, e.svc.HandleDiscard, http.MethodPost); code != http.StatusConflict {
		t.Errorf("discard while awaiting confirmation = %d, want 409", code)
	}

	code, _, body := call(t, e.svc.HandleConfirm, http.MethodPost)
	var st Status
	decode(t, body, &st)
	if code != http.StatusOK || st.State != StateIdle || st.Last == nil || st.Last.Kind != "confirmed" {
		t.Errorf("confirm = %d %+v", code, st)
	}

	// a second change, reverted by hand
	e.stage(func(c *config.NetworkConfig) { c.WanInterface = "eth5" })
	call(t, e.svc.HandleCommit, http.MethodPost)
	e.svc.Wait()
	code, _, _ = call(t, e.svc.HandleRevert, http.MethodPost)
	e.svc.Wait()
	if code != http.StatusAccepted {
		t.Errorf("revert = %d, want 202", code)
	}
	if e.mgr.NetworkConfig().WanInterface != "eth0" {
		t.Error("the WAN interface should be back to eth0")
	}

	// discard the edit that came back as pending
	code, _, body = call(t, e.svc.HandleDiscard, http.MethodPost)
	decode(t, body, &st)
	if code != http.StatusOK || st.HasChanges {
		t.Errorf("discard = %d %+v", code, st)
	}
}

func TestStateChangingEndpointsOnlyAcceptPOSTAndReadsOnlyGET(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	posts := map[string]http.HandlerFunc{
		"commit": e.svc.HandleCommit, "confirm": e.svc.HandleConfirm,
		"revert": e.svc.HandleRevert, "discard": e.svc.HandleDiscard,
	}
	for name, h := range posts {
		for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			code, hdr, _ := call(t, h, m)
			if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != http.MethodPost {
				t.Errorf("%s via %s = %d (Allow %q), want 405 with Allow: POST", name, m, code, hdr.Get("Allow"))
			}
		}
	}
	gets := map[string]http.HandlerFunc{"status": e.svc.HandleStatus, "staged": e.svc.HandleStaged}
	for name, h := range gets {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			code, hdr, _ := call(t, h, m)
			if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != http.MethodGet {
				t.Errorf("%s via %s = %d (Allow %q), want 405 with Allow: GET", name, m, code, hdr.Get("Allow"))
			}
		}
	}
	// and a refused method must not have done anything
	if e.svc.Status().State != StateIdle || len(e.run.log()) != 0 {
		t.Error("a rejected method changed state")
	}
}
