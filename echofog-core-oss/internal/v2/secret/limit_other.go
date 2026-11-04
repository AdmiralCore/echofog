//go:build !unix

package secret

// lockLimit reports no system limit: Windows has no RLIMIT_MEMLOCK, and VirtualLock fails as a
// whole when the working-set quota is exhausted, which probeLock sees directly.
func lockLimit() (uint64, bool) { return 0, false }

func checkLockBudget(uint64, bool) error { return nil }
