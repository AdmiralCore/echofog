//go:build windows

package node

import (
	"syscall"
)

// memguard locks every secret buffer into memory (VirtualLock), and Windows allows a process only as many
// locked pages as its minimum working set (200 KB by default): the profiles these tests register run out of
// it after a few nodes ("could not acquire lock ... Insufficient quota") and memguard then deadlocks. A test
// binary asks for room to hold them; this changes the test process only.
func init() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	self, _, _ := k32.NewProc("GetCurrentProcess").Call()
	const mib = 1 << 20
	// SetProcessWorkingSetSize(process, min, max)
	k32.NewProc("SetProcessWorkingSetSize").Call(self, uintptr(128*mib), uintptr(512*mib))
}
