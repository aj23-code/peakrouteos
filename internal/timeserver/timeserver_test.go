package timeserver

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

var noon = time.Date(2026, 11, 24, 12, 0, 0, 500_000_000, time.UTC)

// a client's question as a phone or a computer sends it: version 4, mode 3, its own time in the transmit field
func question(version byte) []byte {
	q := make([]byte, packetSize)
	q[0] = version<<3 | modeClient
	q[2] = 6
	copy(q[40:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	return q
}

func synced() State {
	return State{Synchronized: true, Stratum: 3, Upstream: net.ParseIP("162.159.200.1"), LastSync: noon.Add(-time.Minute)}
}

func TestTheAnswerSaysWhoIsAskedWhereTheTimeCameFromAndWhatTimeItIs(t *testing.T) {
	r, ok := Reply(question(4), noon, synced())
	if !ok || len(r) != 48 {
		t.Fatalf("%v %d", ok, len(r))
	}
	if r[0] != 0<<6|4<<3|4 {
		t.Errorf("leap, version and mode: %#x", r[0])
	}
	if r[1] != 3 || r[2] != 6 {
		t.Errorf("stratum %d poll %d", r[1], r[2])
	}
	if got := net.IP(r[12:16]).String(); got != "162.159.200.1" {
		t.Errorf("reference id %s", got)
	}
	if string(r[24:32]) != string([]byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("origin %v is not the client's transmit time", r[24:32])
	}
	sec := binary.BigEndian.Uint32(r[40:44])
	if want := uint32(noon.Unix() + ntpEpoch); sec != want {
		t.Errorf("transmit seconds %d, want %d", sec, want)
	}
	if frac := binary.BigEndian.Uint32(r[44:48]); frac < 0x7FFFFF00 || frac > 0x80000100 { // .5 s
		t.Errorf("transmit fraction %#x", frac)
	}
	if ref := binary.BigEndian.Uint32(r[16:20]); ref != uint32(noon.Add(-time.Minute).Unix()+ntpEpoch) {
		t.Errorf("reference time %d", ref)
	}
}

func TestAClockThatFollowsNoOneIsAnsweredAsNotSet(t *testing.T) {
	for _, st := range []State{{}, {Synchronized: true, Stratum: 0}, {Synchronized: true, Stratum: 16}, {Stratum: 3}} {
		r, ok := Reply(question(4), noon, st)
		if !ok || r[0]>>6 != 3 || r[1] != 16 {
			t.Errorf("%+v: leap %d stratum %d (clients must be told the clock is not set)", st, r[0]>>6, r[1])
		}
	}
}

func TestOldClientsGetTheirOwnVersionBack(t *testing.T) {
	for _, v := range []byte{1, 2, 3, 4} {
		r, ok := Reply(question(v), noon, synced())
		if !ok || r[0]>>3&7 != v {
			t.Errorf("version %d: %v %#x", v, ok, r)
		}
	}
}

func TestOnlyClientQuestionsAreAnswered(t *testing.T) {
	short := question(4)[:47]
	peer := question(4)
	peer[0] = 4<<3 | 1 // symmetric active
	ctl := question(4)
	ctl[0] = 2<<3 | 6 // control message
	future := question(4)
	future[0] = 7<<3 | modeClient
	old := question(4)
	old[0] = 0<<3 | modeClient
	for name, p := range map[string][]byte{"short": short, "peer": peer, "control": ctl, "version 7": future, "version 0": old, "empty": nil} {
		if r, ok := Reply(p, noon, synced()); ok || r != nil {
			t.Errorf("%s was answered", name)
		}
	}
	// an answer is never longer than the question: no use as an amplifier
	long := append(question(4), make([]byte, 100)...)
	if r, ok := Reply(long, noon, synced()); !ok || len(r) != 48 {
		t.Errorf("%v %d", ok, len(r))
	}
}

func TestAnIPv6UpstreamIsNamedByAHash(t *testing.T) {
	st := synced()
	st.Upstream = net.ParseIP("2606:4700:f1::1")
	r, _ := Reply(question(4), noon, st)
	if string(r[12:16]) == "\x00\x00\x00\x00" {
		t.Error("no reference id")
	}
	st.Upstream = nil
	r, _ = Reply(question(4), noon, st)
	if string(r[12:16]) != "\x00\x00\x00\x00" {
		t.Errorf("%v", r[12:16])
	}
}

func askOver(t *testing.T, addr string) []byte {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(question(4)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100)
	n, err := conn.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func TestTheServerAnswersOnTheAddressesItIsGivenAndOnlyThose(t *testing.T) {
	var mu sync.Mutex
	addrs := []string{"127.0.0.1"}
	s := &Server{
		Addrs: func() []string { mu.Lock(); defer mu.Unlock(); return addrs },
		Clock: func(context.Context) State { return synced() },
		Every: 20 * time.Millisecond,
		Now:   func() time.Time { return noon },
		Port:  "31123",
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	var r []byte
	for i := 0; i < 50 && r == nil; i++ {
		time.Sleep(20 * time.Millisecond)
		r = askOver(t, "127.0.0.1:31123")
	}
	if len(r) != 48 || r[1] != 3 {
		t.Fatalf("no answer: %v", r)
	}
	if s.Asked() < 1 {
		t.Error("the question was not counted")
	}

	// the network stopped serving the time: the address is let go
	mu.Lock()
	addrs = nil
	mu.Unlock()
	// (the server looks every 20 ms; on a busy machine that can take a while, so the port is waited for)
	var conn net.PacketConn
	var err error
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		if conn, err = net.ListenPacket("udp", "127.0.0.1:31123"); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the port was not freed: %v", err)
	}
	conn.Close()
}

func TestNothingListensWhileNoNetworkServesTheTime(t *testing.T) {
	s := &Server{Addrs: func() []string { return nil }, Every: 10 * time.Millisecond, Port: "31124",
		Clock: func(context.Context) State { t.Error("the clock was read for nobody"); return State{} }}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if len(s.listeners) != 0 {
		t.Error("listeners")
	}
}
