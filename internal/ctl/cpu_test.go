package ctl

import "testing"

// nproc reports host cores because it reads the affinity mask, not the CFS
// quota: `--cpus=2` on a 64-core host previously produced 64 osm2pgsql threads
// and 64 Gunicorn workers, exhausting the database connection limit.
func TestParseCPUMax(t *testing.T) {
	cases := map[string]int{
		"200000 100000":   2,
		"100000 100000":   1,
		"150000 100000":   2, // rounds up: half a CPU still needs a worker
		"max 100000":      0,
		"":                0,
		"garbage":         0,
		"200000":          0,
		"0 100000":        0,
		"200000 0":        0,
		"400000 100000\n": 4,
	}
	for in, want := range cases {
		if got := parseCPUMax(in); got != want {
			t.Errorf("parseCPUMax(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAvailableCPUsIsAtLeastOne(t *testing.T) {
	if n := AvailableCPUs(); n < 1 {
		t.Fatalf("AvailableCPUs() = %d", n)
	}
}
