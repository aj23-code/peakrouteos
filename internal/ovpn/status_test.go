package ovpn

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const statusFile = `TITLE,OpenVPN 2.6.14 x86_64-pc-linux-gnu
TIME,2026-10-11 21:00:00,1791752400
HEADER,CLIENT_LIST,Common Name,Real Address,Virtual Address,Virtual IPv6 Address,Bytes Received,Bytes Sent,Connected Since,Connected Since (time_t),Username,Client ID,Peer ID,Data Channel Cipher
CLIENT_LIST,ra-c1,203.0.113.2:36510,10.9.0.2,,1234,5678,2026-10-11 20:55:00,1791752100,UNDEF,0,0,AES-256-GCM
CLIENT_LIST,ra-c3,198.51.100.9:4000,10.9.0.4,,1,2,2026-10-11 20:56:00,1791752160,UNDEF,1,1,AES-256-GCM
HEADER,ROUTING_TABLE,Virtual Address,Common Name,Real Address,Last Ref,Last Ref (time_t)
ROUTING_TABLE,10.9.0.2,ra-c1,203.0.113.2:36510,2026-10-11 20:59:59,1791752399
GLOBAL_STATS,Max bcast/mcast queue length,0
END
`

func TestTheStatusFile(t *testing.T) {
	got := ParseStatus(statusFile)
	if len(got) != 2 || got[0].CommonName != "ra-c1" || got[0].RealAddress != "203.0.113.2:36510" || got[0].VirtualAddress != "10.9.0.2" ||
		got[0].BytesReceived != 1234 || got[0].BytesSent != 5678 || got[0].Since.Unix() != 1791752100 || got[1].CommonName != "ra-c3" {
		t.Fatalf("%+v", got)
	}
	if len(ParseStatus("")) != 0 || len(ParseStatus("CLIENT_LIST,x\nnonsense\n")) != 0 {
		t.Fatal("something was made up")
	}
	// a Windows line end, a time that is not one
	got = ParseStatus("CLIENT_LIST,a,1.2.3.4:5,10.9.0.2,,1,2,x,notatime,UNDEF,0,0,AES\r\n")
	if len(got) != 1 || !got[0].Since.IsZero() {
		t.Fatalf("%+v", got)
	}
}

func TestTheReaderKeepsAnswersForAMoment(t *testing.T) {
	dir := t.TempDir()
	r := &Reader{Dir: dir, Keep: 60 * time.Millisecond}
	if _, ok := r.Connections("pr-ra"); ok {
		t.Fatal("a server that is not running has connections")
	}
	path := filepath.Join(dir, "status-pr-ra.log")
	os.WriteFile(path, []byte(statusFile), 0644)
	if _, ok := r.Connections("pr-ra"); ok {
		t.Fatal("the answer was not kept")
	}
	time.Sleep(90 * time.Millisecond)
	list, ok := r.Connections("pr-ra")
	if !ok || len(list) != 2 {
		t.Fatalf("%v %v", list, ok)
	}
	os.WriteFile(path, []byte("END\n"), 0644)
	if list, _ := r.Connections("pr-ra"); len(list) != 2 {
		t.Fatal("asked again at once")
	}
	time.Sleep(90 * time.Millisecond)
	if list, ok := r.Connections("pr-ra"); !ok || len(list) != 0 {
		t.Fatalf("%v %v", list, ok)
	}
	// each server has its own
	if _, ok := r.Connections("pr-s1"); ok {
		t.Fatal("another server's answer")
	}
}
