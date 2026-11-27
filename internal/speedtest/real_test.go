package speedtest

import (
	"context"
	"os"
	"testing"
	"time"
)

// An opt-in run against the real Cloudflare endpoints with a small cap (PEAKROUTE_REAL_SPEEDTEST=1): it checks that TLS, HTTP/1.1 and the
// acknowledged-bytes count work against the real server. It is not run by default (it uses the network).
func TestAgainstTheRealServer(t *testing.T) {
	if os.Getenv("PEAKROUTE_REAL_SPEEDTEST") == "" {
		t.Skip("set PEAKROUTE_REAL_SPEEDTEST=1 to run against speed.cloudflare.com")
	}
	tester := &Tester{Duration: 4 * time.Second, MaxBytes: 25 << 20}
	res, err := tester.Run(context.Background(), func(p string) { t.Log("phase:", p) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("download %.1f Mbit/s (%d bytes), upload %.1f Mbit/s (%d bytes)", res.DownloadMbit, res.BytesDown, res.UploadMbit, res.BytesUp)
	if res.DownloadMbit <= 0 || res.UploadMbit <= 0 {
		t.Errorf("%+v", res)
	}
}
