//go:build windows

package nativecap

import (
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows.Graphics.Capture (Windows 10 1903+): one path for windows and monitors, with the cursor,
// and frames that stay on the GPU. IIDs and method order come from the system's WinMetadata.
var (
	iidIUnknown                   = guid("00000000-0000-0000-c000-000000000046")
	iidIAgileObject               = guid("94ea2b94-e9cc-49e0-c0ff-ee64ca8f5b90")
	iidIClosable                  = guid("30d5a829-7fa4-4026-83bb-d75bae4ea99e")
	iidGraphicsCaptureItemInterop = guid("3628e81b-3cac-4c60-b7f4-23ce0e0c3356")
	iidIGraphicsCaptureItem       = guid("79c3f95b-31f7-4ec2-a464-632ef5d30760")
	iidFramePoolStatics2          = guid("589b103f-6bbc-5df5-a991-02e28b3b66d5")
	iidSessionStatics             = guid("2224a540-5974-49aa-b232-0882536f4cb5")
	iidSession2                   = guid("2c39ae40-7d2e-5044-804e-8b6799d4cf9e")
	iidSession5                   = guid("67c0ea62-1f85-5061-925a-239be0ac09cb")
	iidSession6                   = guid("d7419236-be20-5e9f-bcd6-c4e98fd6afdc")
	// TypedEventHandler<Direct3D11CaptureFramePool, Object>, a parameterized IID (see pinterfaceIID)
	iidFrameArrivedHandler = guid("51a947f7-79cf-5a3e-a3a5-1289cfa6dfe8")
)

// vtable slots after IInspectable's six
const (
	interopCreateForWindow  = 3
	interopCreateForMonitor = 4
	itemGetSize             = 7
	poolStaticsCreateFree   = 6
	poolRecreate            = 6
	poolTryGetNextFrame     = 7
	poolAddFrameArrived     = 8
	poolRemoveFrameArrived  = 9
	poolCreateSession       = 10
	frameGetSurface         = 6
	frameGetContentSize     = 8
	sessionStart            = 6
	sessionStaticsSupported = 6
	putBoolOrInterval       = 7 // put_IsCursorCaptureEnabled, put_MinUpdateInterval, put_IncludeSecondaryWindows
	closableClose           = 6
)

const poolBuffers = 3

// captureSupported reports whether this Windows has Windows.Graphics.Capture.
func captureSupported() bool {
	statics, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureSession", &iidSessionStatics)
	if err != nil {
		return false
	}
	defer release(statics)
	var ok uint8
	if comCall(statics, sessionStaticsSupported, uintptr(unsafe.Pointer(&ok))) < 0 {
		return false
	}
	return ok != 0
}

// frameHandler is the FrameArrived delegate, a COM object made in Go. The pool calls it on a
// thread of its own; it only wakes the capture loop.
type frameHandler struct {
	vtbl    *[4]uintptr
	refs    atomic.Int32
	arrived chan struct{}
}

var (
	frameHandlerVtbl = [4]uintptr{
		syscall.NewCallback(func(this *frameHandler, iid *windows.GUID, out *unsafe.Pointer) uintptr {
			if *iid == iidIUnknown || *iid == iidFrameArrivedHandler || *iid == iidIAgileObject {
				this.refs.Add(1)
				*out = unsafe.Pointer(this)
				return 0
			}
			*out = nil
			return uintptr(0x80004002) // E_NOINTERFACE
		}),
		syscall.NewCallback(func(this *frameHandler) uintptr { return uintptr(this.refs.Add(1)) }),
		syscall.NewCallback(func(this *frameHandler) uintptr { return uintptr(this.refs.Add(-1)) }),
		syscall.NewCallback(func(this *frameHandler, sender, args uintptr) uintptr {
			select {
			case this.arrived <- struct{}{}:
			default:
			}
			return 0
		}),
	}
	// handlers keeps every delegate the system may still call reachable for the GC.
	handlers sync.Map
)

// wgcCapture captures one window or monitor into a BGRA texture that is always the latest frame.
type wgcCapture struct {
	d                   *device
	item, pool, session unsafe.Pointer
	token               int64
	handler             *frameHandler
	poolW, poolH        int32
	hwnd                windows.HWND // window captures: closing the window ends the capture

	// latest holds a copy of the newest frame, of the picture's size
	latest             unsafe.Pointer
	contentW, contentH uint32
}

func newWGCCapture(d *device, src captureTarget, cursor bool, minInterval time.Duration) (*wgcCapture, error) {
	c := &wgcCapture{d: d, handler: &frameHandler{vtbl: &frameHandlerVtbl, arrived: make(chan struct{}, 1)}, hwnd: src.hwnd}
	fail := func(err error) (*wgcCapture, error) { c.close(); return nil, err }

	interop, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureItem", &iidGraphicsCaptureItemInterop)
	if err != nil {
		return nil, err
	}
	if src.hwnd != 0 {
		err = comCall(interop, interopCreateForWindow, uintptr(src.hwnd), uintptr(unsafe.Pointer(&iidIGraphicsCaptureItem)), uintptr(unsafe.Pointer(&c.item))).err("CreateForWindow")
	} else {
		err = comCall(interop, interopCreateForMonitor, uintptr(src.monitor), uintptr(unsafe.Pointer(&iidIGraphicsCaptureItem)), uintptr(unsafe.Pointer(&c.item))).err("CreateForMonitor")
	}
	release(interop)
	if err != nil {
		return fail(err)
	}
	var size [2]int32
	if err := comCall(c.item, itemGetSize, uintptr(unsafe.Pointer(&size))).err("GraphicsCaptureItem.Size"); err != nil {
		return fail(err)
	}
	if size[0] <= 0 || size[1] <= 0 {
		return fail(errors.New("the window has no picture (minimized?)"))
	}
	c.poolW, c.poolH = size[0], size[1]
	c.contentW, c.contentH = uint32(size[0]), uint32(size[1])

	statics, err := activationFactory("Windows.Graphics.Capture.Direct3D11CaptureFramePool", &iidFramePoolStatics2)
	if err != nil {
		return fail(err)
	}
	err = comCall(statics, poolStaticsCreateFree, uintptr(d.winrt), dxgiFormatB8G8R8A8, poolBuffers, packSize(size[0], size[1]), uintptr(unsafe.Pointer(&c.pool))).err("CreateFreeThreaded")
	release(statics)
	if err != nil {
		return fail(err)
	}
	if c.latest, err = c.newLatest(); err != nil {
		return fail(err)
	}
	handlers.Store(c.handler, struct{}{})
	if err := comCall(c.pool, poolAddFrameArrived, uintptr(unsafe.Pointer(c.handler)), uintptr(unsafe.Pointer(&c.token))).err("FrameArrived"); err != nil {
		return fail(err)
	}
	if err := comCall(c.pool, poolCreateSession, uintptr(c.item), uintptr(unsafe.Pointer(&c.session))).err("CreateCaptureSession"); err != nil {
		return fail(err)
	}
	if s2, err := queryInterface(c.session, &iidSession2, "IGraphicsCaptureSession2"); err == nil {
		comCall(s2, putBoolOrInterval, boolArg(cursor))
		release(s2)
	}
	// Without it (Windows 11 24H2 and older) the system decides how often frames come.
	if s5, err := queryInterface(c.session, &iidSession5, "IGraphicsCaptureSession5"); err == nil {
		comCall(s5, putBoolOrInterval, uintptr(minInterval/100)) // TimeSpan counts 100 ns
		release(s5)
	}
	if src.hwnd != 0 {
		// menus and dialogs of the shared window belong in the picture
		if s6, err := queryInterface(c.session, &iidSession6, "IGraphicsCaptureSession6"); err == nil {
			comCall(s6, putBoolOrInterval, 1)
			release(s6)
		}
	}
	if err := comCall(c.session, sessionStart).err("StartCapture"); err != nil {
		return fail(err)
	}
	return c, nil
}

func packSize(w, h int32) uintptr { return uintptr(uint32(w)) | uintptr(uint32(h))<<32 }

func boolArg(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

func (c *wgcCapture) newLatest() (unsafe.Pointer, error) {
	return c.d.texture(texture2DDesc{Width: uint32(c.poolW), Height: uint32(c.poolH), Format: dxgiFormatB8G8R8A8,
		BindFlags: d3d11BindRenderTarget | d3d11BindShaderResource})
}

// arrived fires when the pool may hold a new frame.
func (c *wgcCapture) arrived() <-chan struct{} { return c.handler.arrived }

// next takes the newest frame waiting in the pool into c.latest. It reports false when none was
// waiting, and resized when the picture changed size (c.latest is a new texture then).
func (c *wgcCapture) next() (got, resized bool, err error) {
	for {
		var frame unsafe.Pointer
		if err := comCall(c.pool, poolTryGetNextFrame, uintptr(unsafe.Pointer(&frame))).err("TryGetNextFrame"); err != nil {
			return got, resized, err
		}
		if frame == nil {
			return got, resized, nil
		}
		copied, r, err := c.take(frame)
		closeObject(frame)
		release(frame)
		if err != nil {
			return got, resized, err
		}
		got = got || copied
		resized = resized || r
	}
}

// take copies one frame into c.latest, unless the picture changed size: then the pool and
// c.latest are made anew for the new size and the frame is dropped.
func (c *wgcCapture) take(frame unsafe.Pointer) (copied, resized bool, err error) {
	var size [2]int32
	comCall(frame, frameGetContentSize, uintptr(unsafe.Pointer(&size)))
	if size[0] <= 0 || size[1] <= 0 {
		return false, false, nil
	}
	if size[0] != c.poolW || size[1] != c.poolH {
		if err := comCall(c.pool, poolRecreate, uintptr(c.d.winrt), dxgiFormatB8G8R8A8, poolBuffers, packSize(size[0], size[1])).err("Recreate"); err != nil {
			return false, false, err
		}
		c.poolW, c.poolH = size[0], size[1]
		latest, err := c.newLatest()
		if err != nil {
			return false, false, err
		}
		release(c.latest)
		c.latest = latest
		c.contentW, c.contentH = uint32(size[0]), uint32(size[1])
		return false, true, nil
	}
	var surface unsafe.Pointer
	if err := comCall(frame, frameGetSurface, uintptr(unsafe.Pointer(&surface))).err("Frame.Surface"); err != nil {
		return false, false, err
	}
	tex, err := textureFromSurface(surface)
	release(surface)
	if err != nil {
		return false, false, err
	}
	desc := textureDesc(tex)
	if desc.Width != uint32(c.poolW) || desc.Height != uint32(c.poolH) {
		release(tex)
		return false, false, nil // a frame of the old pool, after a resize
	}
	c.d.copyResource(c.latest, tex)
	release(tex)
	return true, false, nil
}

// closeObject calls IClosable.Close, which a WinRT object needs to give its resources back now.
func closeObject(obj unsafe.Pointer) {
	if obj == nil {
		return
	}
	if cl, err := queryInterface(obj, &iidIClosable, "IClosable"); err == nil {
		comCall(cl, closableClose)
		release(cl)
	}
}

// gone reports whether the captured window was closed.
func (c *wgcCapture) gone() bool { return c.hwnd != 0 && !windows.IsWindow(c.hwnd) }

func (c *wgcCapture) close() {
	if c.pool != nil && c.token != 0 {
		comCall(c.pool, poolRemoveFrameArrived, uintptr(c.token))
		c.token = 0
	}
	closeObject(c.session)
	release(c.session)
	closeObject(c.pool)
	release(c.pool)
	release(c.item)
	release(c.latest)
	c.session, c.pool, c.item, c.latest = nil, nil, nil, nil
	// the pool may still be inside the delegate for a moment
	h := c.handler
	time.AfterFunc(5*time.Second, func() { handlers.Delete(h) })
}
