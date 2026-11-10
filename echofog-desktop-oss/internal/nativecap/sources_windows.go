//go:build windows

package nativecap

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modUser32                = windows.NewLazySystemDLL("user32.dll")
	procEnumDisplayMonitors  = modUser32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW      = modUser32.NewProc("GetMonitorInfoW")
	procGetWindowTextW       = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW = modUser32.NewProc("GetWindowTextLengthW")
	procIsIconic             = modUser32.NewProc("IsIconic")
	procGetWindowLongPtrW    = modUser32.NewProc("GetWindowLongPtrW")
	procGetWindow            = modUser32.NewProc("GetWindow")
	procEnumDisplaySettingsW = modUser32.NewProc("EnumDisplaySettingsW")
	procMonitorFromWindow    = modUser32.NewProc("MonitorFromWindow")
	procFindWindowExW        = modUser32.NewProc("FindWindowExW")
)

// captureTarget is what Windows.Graphics.Capture is pointed at: a window or a monitor.
type captureTarget struct {
	hwnd    windows.HWND
	monitor uintptr
}

// devMode is DEVMODEW, of which only the display fields are read.
type devMode struct {
	DeviceName                                    [32]uint16
	SpecVersion, DriverVersion, Size, DriverExtra uint16
	Fields                                        uint32
	PositionX, PositionY                          int32
	Orientation, FixedOutput                      uint32
	Color, Duplex, YResolution, TTOption, Collate int16
	FormName                                      [32]uint16
	LogPixels                                     uint16
	BitsPerPel, PelsWidth, PelsHeight             uint32
	DisplayFlags, DisplayFrequency                uint32
	_                                             [8]uint32
}

// monitorInfo is MONITORINFOEXW.
type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
	Device  [32]uint16
}

// Enumeration callbacks are made once: a process can only ever make a couple of thousand.
var (
	enumMu       sync.Mutex
	enumMonitors []uintptr
	enumWindows  []windows.HWND
	monitorProc  = syscall.NewCallback(func(hmon, hdc, r, data uintptr) uintptr {
		enumMonitors = append(enumMonitors, hmon)
		return 1
	})
	windowProc = syscall.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		enumWindows = append(enumWindows, h)
		return 1
	})
)

// monitors lists the monitors' handles, in the system's order (the one monitor IDs count by).
func monitors() []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumMonitors = nil
	procEnumDisplayMonitors.Call(0, 0, monitorProc, 0)
	out := enumMonitors
	enumMonitors = nil
	return out
}

// topWindows lists the top-level windows, front to back.
func topWindows() []windows.HWND {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumWindows = nil
	_ = windows.EnumWindows(windowProc, nil)
	out := enumWindows
	enumWindows = nil
	return out
}

// Sources lists what can be shared: every monitor, then the windows a person would pick from
// (visible, titled, not tool windows, not this app's own).
func Sources() []Source {
	var out []Source
	for i, hmon := range monitors() {
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(hmon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			continue
		}
		s := Source{
			ID: "monitor:" + strconv.Itoa(i), Kind: "monitor", Title: strings.TrimPrefix(windows.UTF16ToString(mi.Device[:]), `\\.\`),
			Width: int(mi.Monitor.Right - mi.Monitor.Left), Height: int(mi.Monitor.Bottom - mi.Monitor.Top), Primary: mi.Flags&1 != 0,
		}
		// the mode is in real pixels whatever the scaling, and has the refresh rate
		if dm, ok := displayMode(&mi); ok {
			s.Width, s.Height, s.RefreshHz = int(dm.PelsWidth), int(dm.PelsHeight), int(dm.DisplayFrequency)
		}
		out = append(out, s)
	}
	self := uint32(os.Getpid())
	shell := windows.GetShellWindow()
	for _, h := range topWindows() {
		if s, ok := windowSource(h, self, shell); ok {
			out = append(out, s)
		}
	}
	return out
}

// displayMode is a monitor's current display mode, in real pixels, with its refresh rate.
func displayMode(mi *monitorInfo) (devMode, bool) {
	dm := devMode{Size: uint16(unsafe.Sizeof(devMode{}))}
	r, _, _ := procEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(&mi.Device[0])), uintptr(0xFFFFFFFF), uintptr(unsafe.Pointer(&dm)))
	return dm, r != 0
}

// refreshRate is the refresh rate of the monitor a target is on (a window: the one most of it is
// on), 0 when it is not known.
func refreshRate(tg captureTarget) int {
	mon := tg.monitor
	if tg.hwnd != 0 {
		mon, _, _ = procMonitorFromWindow.Call(uintptr(tg.hwnd), 2 /* MONITOR_DEFAULTTONEAREST */)
	}
	mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return 0
	}
	dm, ok := displayMode(&mi)
	if !ok || dm.DisplayFrequency <= 1 { // 0 and 1 stand for the hardware's default
		return 0
	}
	return int(dm.DisplayFrequency)
}

// DisplayRate is the refresh rate of the screen this app's window is on, 0 when it is not known:
// a viewer tells it to the streams it watches, which pick a frame rate it shows evenly.
func DisplayRate() int {
	self := uint32(os.Getpid())
	for _, h := range topWindows() {
		var pid uint32
		_, _ = windows.GetWindowThreadProcessId(h, &pid)
		if pid != self || !windows.IsWindowVisible(h) {
			continue
		}
		if owner, _, _ := procGetWindow.Call(uintptr(h), 4 /* GW_OWNER */); owner == 0 {
			return refreshRate(captureTarget{hwnd: h})
		}
	}
	return 0
}

// windowSource describes a top-level window as a source, or reports false for one a person would not
// pick (hidden, owned, a tool window, on another virtual desktop, untitled, this app's own, the shell's).
func windowSource(h windows.HWND, self uint32, shell windows.HWND) (Source, bool) {
	if h == shell || !windows.IsWindowVisible(h) {
		return Source{}, false
	}
	if owner, _, _ := procGetWindow.Call(uintptr(h), 4 /* GW_OWNER */); owner != 0 {
		return Source{}, false
	}
	exStyle, _, _ := procGetWindowLongPtrW.Call(uintptr(h), uintptr(0xFFFFFFEC) /* GWL_EXSTYLE */)
	if exStyle&0x80 != 0 { // WS_EX_TOOLWINDOW
		return Source{}, false
	}
	var cloaked uint32
	if windows.DwmGetWindowAttribute(h, 14 /* DWMWA_CLOAKED */, unsafe.Pointer(&cloaked), 4) == nil && cloaked != 0 {
		return Source{}, false // on another virtual desktop, or a suspended store app
	}
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(h))
	if n == 0 {
		return Source{}, false
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), n+1)
	var pid uint32
	_, _ = windows.GetWindowThreadProcessId(h, &pid)
	if pid == self {
		return Source{}, false
	}
	var r rect // in real pixels, without the invisible resize borders
	_ = windows.DwmGetWindowAttribute(h, 9 /* DWMWA_EXTENDED_FRAME_BOUNDS */, unsafe.Pointer(&r), uint32(unsafe.Sizeof(r)))
	minimized, _, _ := procIsIconic.Call(uintptr(h))
	if minimized == 0 && (r.Right <= r.Left || r.Bottom <= r.Top) {
		return Source{}, false // a window with no area: a hidden host of something else
	}
	return Source{
		ID: "window:" + strconv.FormatUint(uint64(h), 10), Kind: "window", Title: windows.UTF16ToString(buf),
		App: processName(pid), Width: int(r.Right - r.Left), Height: int(r.Bottom - r.Top), Minimized: minimized != 0,
	}, true
}

// processName is the file name of a process's program, or "" when it cannot be read.
func processName(pid uint32) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if windows.QueryFullProcessImageName(p, 0, &buf[0], &n) != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:n]))
}

// target resolves a source ID from Sources.
func target(id string) (captureTarget, error) {
	kind, val, _ := strings.Cut(id, ":")
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return captureTarget{}, fmt.Errorf("bad capture source %q", id)
	}
	switch kind {
	case "monitor":
		mons := monitors()
		if int(n) >= len(mons) {
			return captureTarget{}, fmt.Errorf("monitor %d is not connected", n)
		}
		return captureTarget{monitor: mons[n]}, nil
	case "window":
		h := windows.HWND(n)
		if !windows.IsWindow(h) {
			return captureTarget{}, fmt.Errorf("the window is closed")
		}
		return captureTarget{hwnd: h}, nil
	}
	return captureTarget{}, fmt.Errorf("bad capture source %q", id)
}
