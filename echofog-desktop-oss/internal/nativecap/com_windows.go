//go:build windows

package nativecap

import (
	"fmt"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// COM and WinRT are called through their vtables, without cgo: a COM object is a pointer to a
// pointer to a table of functions, each taking the object first.

var (
	modD3D11   = windows.NewLazySystemDLL("d3d11.dll")
	modMFPlat  = windows.NewLazySystemDLL("mfplat.dll")
	modCombase = windows.NewLazySystemDLL("combase.dll")
	modOle32   = windows.NewLazySystemDLL("ole32.dll")
	modKernel  = windows.NewLazySystemDLL("kernel32.dll")

	procD3D11CreateDevice                    = modD3D11.NewProc("D3D11CreateDevice")
	procCreateDirect3D11DeviceFromDXGIDevice = modD3D11.NewProc("CreateDirect3D11DeviceFromDXGIDevice")
	procRoInitialize                         = modCombase.NewProc("RoInitialize")
	procRoGetActivationFactory               = modCombase.NewProc("RoGetActivationFactory")
	procWindowsCreateString                  = modCombase.NewProc("WindowsCreateString")
	procWindowsDeleteString                  = modCombase.NewProc("WindowsDeleteString")
	procCoTaskMemFree                        = modOle32.NewProc("CoTaskMemFree")
	procQueryPerformanceCounter              = modKernel.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency            = modKernel.NewProc("QueryPerformanceFrequency")
	procMFStartup                            = modMFPlat.NewProc("MFStartup")
	procMFTEnumEx                            = modMFPlat.NewProc("MFTEnumEx")
	procMFCreateMediaType                    = modMFPlat.NewProc("MFCreateMediaType")
	procMFCreateSample                       = modMFPlat.NewProc("MFCreateSample")
	procMFCreateDXGISurfaceBuffer            = modMFPlat.NewProc("MFCreateDXGISurfaceBuffer")
	procMFCreateDXGIDeviceManager            = modMFPlat.NewProc("MFCreateDXGIDeviceManager")
	procMFCreateMemoryBuffer                 = modMFPlat.NewProc("MFCreateMemoryBuffer")
)

// hresult is a COM status: negative is a failure.
type hresult int32

const (
	sOK          hresult = 0
	sFalse       hresult = 1
	eNoInterface hresult = -2147467262 // 0x80004002
	ePointer     hresult = -2147467261 // 0x80004003
	eNotImpl     hresult = -2147467263 // 0x80004001
)

// comError is a failed COM call: what was called and the HRESULT it returned.
type comError struct {
	op string
	hr hresult
}

func (e *comError) Error() string { return fmt.Sprintf("%s: %s", e.op, hrName(e.hr)) }

// HRESULT reports the code, for callers that tell failures apart.
func (e *comError) HRESULT() int32 { return int32(e.hr) }

func hrName(hr hresult) string {
	known := map[uint32]string{
		0x80004002: "E_NOINTERFACE",
		0x80004001: "E_NOTIMPL",
		0x80004005: "E_FAIL",
		0x80070057: "E_INVALIDARG",
		0x8007000E: "E_OUTOFMEMORY",
		0x887A0005: "DXGI_ERROR_DEVICE_REMOVED",
		0x887A0007: "DXGI_ERROR_DEVICE_RESET",
		0x887A0001: "DXGI_ERROR_INVALID_CALL",
		0x887A0004: "DXGI_ERROR_UNSUPPORTED",
		0xC00D36B4: "MF_E_INVALIDMEDIATYPE",
		0xC00D6D72: "MF_E_TRANSFORM_NEED_MORE_INPUT",
		0xC00D6D61: "MF_E_TRANSFORM_STREAM_CHANGE",
		0xC00D36B2: "MF_E_INVALIDREQUEST",
		0xC00D36B5: "MF_E_INVALIDSTREAMNUMBER",
		0xC00D3E85: "MF_E_NO_MORE_TYPES",
		0xC00D36E6: "MF_E_ATTRIBUTENOTFOUND",
		0xC00D6D60: "MF_E_TRANSFORM_TYPE_NOT_SET",
		0xC00D6D6C: "MF_E_TRANSFORM_ASYNC_LOCKED",
		0xC00D36FA: "MF_E_NOTACCEPTING",
		0xC00D36B9: "MF_E_UNSUPPORTED_D3D_TYPE",
	}
	if name, ok := known[uint32(hr)]; ok {
		return name
	}
	return "HRESULT 0x" + strconv.FormatUint(uint64(uint32(hr)), 16)
}

func (hr hresult) err(op string) error {
	if hr < 0 {
		return &comError{op: op, hr: hr}
	}
	return nil
}

// comCall calls method idx of the COM object obj. Pointers passed as uintptr stay valid for the
// call: uintptrescapes moves what they point to onto the heap and keeps it alive.
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) hresult {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(obj))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return hresult(r)
}

// procCall calls a DLL function that returns an HRESULT.
//
//go:uintptrescapes
func procCall(p *windows.LazyProc, args ...uintptr) hresult {
	if err := p.Find(); err != nil {
		return hresult(-2147024770) // 0x8007007E ERROR_MOD_NOT_FOUND
	}
	r, _, _ := syscall.SyscallN(p.Addr(), args...)
	return hresult(r)
}

const (
	idxQueryInterface = 0
	idxAddRef         = 1
	idxRelease        = 2
)

func release(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, idxRelease)
	}
}

func addRef(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, idxAddRef)
	}
}

func queryInterface(obj unsafe.Pointer, iid *windows.GUID, op string) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	if err := comCall(obj, idxQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))).err(op); err != nil {
		return nil, err
	}
	return out, nil
}

// guid parses "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx" at init; a typo panics at start, not later.
func guid(s string) windows.GUID {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		panic("nativecap: bad GUID " + s)
	}
	hex := func(part string) uint64 {
		v, err := strconv.ParseUint(part, 16, 64)
		if err != nil {
			panic("nativecap: bad GUID " + s)
		}
		return v
	}
	g := windows.GUID{Data1: uint32(hex(s[0:8])), Data2: uint16(hex(s[9:13])), Data3: uint16(hex(s[14:18]))}
	tail := s[19:23] + s[24:]
	for i := 0; i < 8; i++ {
		g.Data4[i] = byte(hex(tail[2*i : 2*i+2]))
	}
	return g
}

// hstring is a WinRT string.
type hstring uintptr

func newHString(s string) (hstring, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var h hstring
	if err := procCall(procWindowsCreateString, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h))).err("WindowsCreateString"); err != nil {
		return 0, err
	}
	return h, nil
}

func (h hstring) free() {
	if h != 0 {
		procCall(procWindowsDeleteString, uintptr(h))
	}
}

// activationFactory returns the factory of a WinRT class, as the interface iid.
func activationFactory(class string, iid *windows.GUID) (unsafe.Pointer, error) {
	h, err := newHString(class)
	if err != nil {
		return nil, err
	}
	defer h.free()
	var f unsafe.Pointer
	if err := procCall(procRoGetActivationFactory, uintptr(h), uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&f))).err("RoGetActivationFactory " + class); err != nil {
		return nil, err
	}
	return f, nil
}

var qpcFreq = func() int64 {
	var f int64
	procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f)))
	return max(f, 1)
}()

// qpcNow is the system's performance counter as a duration: the clock capture times are on.
func qpcNow() time.Duration {
	var c int64
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&c)))
	return time.Duration(c/qpcFreq)*time.Second + time.Duration(c%qpcFreq)*time.Second/time.Duration(qpcFreq)
}

// mtaThread pins the calling goroutine to its OS thread and joins that thread to the
// multithreaded apartment, where every object of this package lives. The returned func undoes it.
func mtaThread() (func(), error) {
	runtime.LockOSThread()
	hr := procCall(procRoInitialize, 1) // RO_INIT_MULTITHREADED; S_FALSE: it already was
	if hr == hresult(-2147417850) {     // RPC_E_CHANGED_MODE
		// Some library made this thread single-threaded. It stays locked, so it ends with the
		// goroutine instead of going back to the scheduler.
		return nil, hr.err("RoInitialize (the thread is single-threaded)")
	}
	if hr < 0 {
		runtime.UnlockOSThread()
		return nil, hr.err("RoInitialize")
	}
	return runtime.UnlockOSThread, nil
}
