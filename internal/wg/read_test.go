package wg

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReaderKeepsAnswersForAMoment(t *testing.T) {
	calls := 0
	out := "wgs1\tPRIV\tPUB\t51820\toff\n"
	var err error
	r := &Reader{Keep: 50 * time.Millisecond, Run: func(context.Context) ([]byte, error) { calls++; return []byte(out), err }}
	if i := r.Interfaces(); len(i) != 1 || i[0].Name != "wgs1" || calls != 1 {
		t.Fatalf("%+v %d", i, calls)
	}
	r.Interfaces()
	if calls != 1 {
		t.Fatalf("asked again at once: %d", calls)
	}
	time.Sleep(80 * time.Millisecond)
	err = errors.New("no wg")
	if i := r.Interfaces(); i != nil || calls != 2 {
		t.Fatalf("%+v %d", i, calls)
	}
}

func TestReaderWithoutTheTools(t *testing.T) {
	r := &Reader{Run: func(context.Context) ([]byte, error) { return nil, errors.New("not found") }}
	if r.Interfaces() != nil {
		t.Fatal("something was made up")
	}
}
