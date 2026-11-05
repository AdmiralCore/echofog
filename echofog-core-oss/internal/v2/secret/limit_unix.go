//go:build unix

package secret

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// lockLimit returns the process's locked-memory limit (RLIMIT_MEMLOCK), if the system has one.
func lockLimit() (uint64, bool) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rl); err != nil {
		return 0, false
	}
	return rl.Cur, true
}

// checkLockBudget refuses to use locked memory when the limit leaves no headroom for the keys a
// node keeps at once. memguard panics when a later lock hits the limit, and on Android the limit
// is often 64 KiB: a probe of one page would succeed and the panic would come later, in the middle
// of a handshake.
func checkLockBudget(limit uint64, known bool) error {
	if !known || limit == unix.RLIM_INFINITY || limit >= minLockBytes {
		return nil
	}
	return fmt.Errorf("secret: locked-memory limit is %d bytes, below the %d-byte budget", limit, minLockBytes)
}
