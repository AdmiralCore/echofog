package simulation

import "syscall"

// increaseWorkingSetQuota lets one process host many nodes without Windows trimming its working set.
func increaseWorkingSetQuota() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	proc := k32.NewProc("SetProcessWorkingSetSize")
	hProcess, _, _ := k32.NewProc("GetCurrentProcess").Call()
	_, _, _ = proc.Call(hProcess, 512*1024*1024, 1024*1024*1024)
}
