//go:build unix

package perf

import (
	"syscall"
	"time"
)

// ProcCPU is the user and system time the process has used so far.
func ProcCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
