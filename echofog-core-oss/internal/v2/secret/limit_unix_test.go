//go:build unix

package secret

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestCheckLockBudget(t *testing.T) {
	cases := []struct {
		name  string
		limit uint64
		known bool
		ok    bool
	}{
		{"android 64 KiB", 64 << 10, true, false},
		{"just below the budget", minLockBytes - 1, true, false},
		{"at the budget", minLockBytes, true, true},
		{"usual desktop", 8 << 20, true, true},
		{"unlimited", unix.RLIM_INFINITY, true, true},
		{"unknown limit", 0, false, true},
	}
	for _, c := range cases {
		if err := checkLockBudget(c.limit, c.known); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
