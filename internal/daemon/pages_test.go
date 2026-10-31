package daemon

import (
	"html/template"
	"strings"
	"testing"

	"peakroute/web"
)

func render(t *testing.T, page string, data map[string]any) string {
	t.Helper()
	tmpl, err := template.ParseFS(web.Files, "templates/*")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, page, data); err != nil {
		t.Fatalf("%s: %v", page, err)
	}
	return b.String()
}

// apViews are the views an access point's menu reaches.
var apViews = []string{"ap-home", "wireless", "settings", "ap", "admins", "certificates", "backup", "updates", "logs", "diagnostics"}

// view is the part of the dashboard that is one view (up to the next one).
func view(t *testing.T, page, name string) string {
	t.Helper()
	start := strings.Index(page, `<div id="view-`+name+`"`)
	if start < 0 {
		t.Fatalf("no view %s", name)
	}
	rest := page[start+1:]
	if end := strings.Index(rest, `<div id="view-`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// The pages call the device what it is: PeakRoute AP and an access point in the access point build (and an access point in the
// router's access point mode), a router otherwise.
func TestThePagesNameTheDevice(t *testing.T) {
	dash := func(mode string, apOnly bool) string {
		product := "PeakRoute OS"
		if apOnly {
			product = "PeakRoute AP"
		}
		return render(t, "dashboard.html", map[string]any{"SystemMode": mode, "CSRFToken": "x", "Username": "u", "Product": product, "APOnly": apOnly})
	}
	ap := dash("AP", true)
	for _, want := range []string{"<title>PeakRoute AP — Dashboard</title>", `class="brand-title">PeakRoute AP<`, "this access point&rsquo;s web interface",
		"set this access point up again", "the network this access point is part of", "how it joins the network that another router runs"} {
		if !strings.Contains(ap, want) {
			t.Errorf("the AP build's dashboard lacks %q", want)
		}
	}
	// the router's own views are in the page too, unreachable from an access point's menu: only the views it has are read
	var shown strings.Builder
	for _, v := range apViews {
		shown.WriteString(view(t, ap, v))
	}
	for _, not := range []string{"this router", "the router", "your Local Networks", "Whether this device is a router", "certificates of the VPN", "Firewall, Logs"} {
		if strings.Contains(shown.String(), not) {
			t.Errorf("the AP build's dashboard says %q", not)
		}
	}
	if modeAP := dash("AP", false); !strings.Contains(modeAP, "set this access point up again") || !strings.Contains(modeAP, "Whether this device is a router") {
		t.Error("the router in access point mode: the device is an access point and the mode can still be switched")
	}
	router := dash("ROUTER", false)
	for _, want := range []string{`class="brand-title">PeakRoute OS<`, "this router&rsquo;s web interface", "your Local Networks", "certificates of the VPN"} {
		if !strings.Contains(router, want) {
			t.Errorf("the router's dashboard lacks %q", want)
		}
	}

	if w := render(t, "first_run.html", map[string]any{"Product": "PeakRoute AP", "APOnly": true}); !strings.Contains(w, "Set up this access point") || strings.Contains(w, "router") {
		t.Error("the AP build's wizard page still names a router")
	}
	if w := render(t, "login.html", map[string]any{"Product": "PeakRoute AP"}); !strings.Contains(w, "<h1>PeakRoute AP</h1>") {
		t.Error("the AP build's login page is not named PeakRoute AP")
	}
}
