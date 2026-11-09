package punch

import (
	"testing"
	"time"
)

// The limits of services.md §6.4.1 are part of the wire contract: a merge must not quietly relax
// them. The modes and the pacing loop use these constants, so this test pins their values.
func TestBirthdayLimitsMatchSpec(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"dial sockets", MaxDialSockets, 256},
		{"mutual sockets", MaxMutualSockets, 64},
		{"spray datagrams", MaxSprayPackets, 900},
		{"punching datagrams", MaxPunchPackets, 960},
		{"attempts per node", MaxConcurrent, 2},
		{"narrow range", NarrowRange, 8000},
		{"spray margin", SprayMargin, 4000},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, spec says %d", c.name, c.got, c.want)
		}
	}
	if PunchWindow != 3*time.Second {
		t.Errorf("punch window = %v, spec says 3s", PunchWindow)
	}
	if PunchRate != 320 {
		t.Errorf("punch rate = %d, spec says 320 datagrams per second", PunchRate)
	}
}
