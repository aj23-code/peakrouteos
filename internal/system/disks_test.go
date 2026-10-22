package system

import (
	"errors"
	"testing"
)

const gib = uint64(1) << 30

func fakeStat(fs map[string][3]uint64) func(string) (uint64, uint64, [2]int32, error) {
	return func(path string) (uint64, uint64, [2]int32, error) {
		v, ok := fs[path]
		if !ok {
			return 0, 0, [2]int32{}, errors.New("no such path")
		}
		return v[0], v[1], [2]int32{int32(v[2]), 0}, nil
	}
}

func TestTheRootAndTheDataPartitionAreListedSeparately(t *testing.T) {
	got := getDisks(fakeStat(map[string][3]uint64{
		"/":     {3 * gib, 1 * gib, 1},     // the slot: 3 GB, 2 used
		"/data": {200 * gib, 150 * gib, 2}, // the data partition: 200 GB, 50 used
	}))
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if r := got[0]; r.Name != "Root" || r.Mount != "/" || r.TotalMB != 3072 || r.UsedMB != 2048 {
		t.Errorf("root: %+v", r)
	}
	if d := got[1]; d.Name != "Data" || d.Mount != "/data" || d.TotalMB != 204800 || d.UsedMB != 51200 || d.UsedPercent != 25 {
		t.Errorf("data: %+v", d)
	}
}

func TestDataIsNotListedWhenItIsNotAPartitionOfItsOwn(t *testing.T) {
	// a development machine: no /data at all
	if got := getDisks(fakeStat(map[string][3]uint64{"/": {100 * gib, 40 * gib, 1}})); len(got) != 1 || got[0].Name != "Root" {
		t.Errorf("no /data: %+v", got)
	}
	// /data is only a folder on the root file system
	if got := getDisks(fakeStat(map[string][3]uint64{"/": {100 * gib, 40 * gib, 1}, "/data": {100 * gib, 40 * gib, 1}})); len(got) != 1 {
		t.Errorf("same file system: %+v", got)
	}
	// nothing can be read
	if got := getDisks(fakeStat(nil)); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}
