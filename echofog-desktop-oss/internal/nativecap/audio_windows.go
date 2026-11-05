//go:build windows

package nativecap

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The sound of a shared screen. For a window it is what that window's program plays, with the
// processes it started (a browser plays from a child process), so a notification or music from
// another window stays out, as other apps share a window's sound. For a monitor it is everything
// the computer plays except this app's process tree, so viewers hear the game or the film but not
// the call, which the webview (a child of this process) plays. WASAPI process loopback (Windows 10
// 2004 and newer) delivers it as PCM; the interface plays it into the screen-sound track, which
// the webview encodes as Opus like a captured tab's.

var (
	modMmdevapi                     = windows.NewLazySystemDLL("mmdevapi.dll")
	procActivateAudioInterfaceAsync = modMmdevapi.NewProc("ActivateAudioInterfaceAsync")

	iidIAudioClient        = guid("1cb9ad4c-dbfa-4c32-b178-c2f568a703b2")
	iidIAudioCaptureClient = guid("c8adbd64-e71e-48a0-a4de-185c395cd317")
	iidActivateHandler     = guid("41d949ab-9862-444a-80f6-c261334da5eb")
)

// vtable slots
const (
	asyncOpGetActivateResult = 3
	clientInitialize         = 3
	clientStart              = 10
	clientStop               = 11
	clientSetEventHandle     = 13
	clientGetService         = 14
	captureGetBuffer         = 3
	captureReleaseBuffer     = 4
	captureGetNextPacketSize = 5
)

// activateHandler is the completion handler ActivateAudioInterfaceAsync calls; it has to be
// agile, so it says it is.
type activateHandler struct {
	vtbl *[4]uintptr
	refs atomic.Int32
	done chan struct{}
}

var activateHandlerVtbl = [4]uintptr{
	syscall.NewCallback(func(this *activateHandler, iid *windows.GUID, out *unsafe.Pointer) uintptr {
		if *iid == iidIUnknown || *iid == iidActivateHandler || *iid == iidIAgileObject {
			this.refs.Add(1)
			*out = unsafe.Pointer(this)
			return 0
		}
		*out = nil
		return uintptr(0x80004002) // E_NOINTERFACE
	}),
	syscall.NewCallback(func(this *activateHandler) uintptr { return uintptr(this.refs.Add(1)) }),
	syscall.NewCallback(func(this *activateHandler) uintptr { return uintptr(this.refs.Add(-1)) }),
	syscall.NewCallback(func(this *activateHandler, op uintptr) uintptr {
		select {
		case this.done <- struct{}{}:
		default:
		}
		return 0
	}),
}

// AudioCapture is the running capture of the computer's sound.
type AudioCapture struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// StartAudio captures the sound of a source (a Source ID): a window's program, or for a monitor
// what the computer plays without this process tree. onPCM receives interleaved 48 kHz stereo
// 16-bit frames every 10 ms or so, on a goroutine of its own, and must not keep the slice; while
// nothing plays it receives nothing.
func StartAudio(source string, onPCM func([]byte)) (*AudioCapture, error) {
	a := &AudioCapture{stop: make(chan struct{}), done: make(chan struct{})}
	ready := make(chan error, 1)
	go a.run(soundPID(source), onPCM, ready)
	if err := <-ready; err != nil {
		<-a.done
		return nil, err
	}
	return a, nil
}

// Stop ends the capture and waits for it.
func (a *AudioCapture) Stop() {
	a.once.Do(func() { close(a.stop) })
	<-a.done
}

// run is the capture's goroutine: it opens the loopback client (a window's process tree, or all
// but this one), reads packets whenever the buffer event fires and passes them to onPCM; a silent
// packet is passed as zeros so the sound keeps its timing. It reports to ready once it started.
func (a *AudioCapture) run(pid uint32, onPCM func([]byte), ready chan<- error) {
	defer close(a.done)
	started := false
	fail := func(err error) {
		if !started {
			ready <- err
		}
	}
	undo, err := mtaThread()
	if err != nil {
		fail(err)
		return
	}
	defer undo()
	client, err := activateLoopback(pid)
	if err != nil {
		fail(err)
		return
	}
	defer release(client)

	format := struct {
		Tag, Channels     uint16
		Rate, BytesPerSec uint32
		BlockAlign, Bits  uint16
		Extra             uint16
	}{1 /* PCM */, 2, AudioRate, AudioRate * audioBytesPerFrame, audioBytesPerFrame, 16, 0}
	const flags = 0x00020000 | 0x00040000 | 0x80000000 | 0x08000000 // LOOPBACK | EVENTCALLBACK | AUTOCONVERTPCM | SRC_DEFAULT_QUALITY
	if err := comCall(client, clientInitialize, 0 /* shared */, flags, uintptr(200*time.Millisecond/100), 0, uintptr(unsafe.Pointer(&format)), 0).err("IAudioClient.Initialize"); err != nil {
		fail(err)
		return
	}
	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		fail(err)
		return
	}
	defer windows.CloseHandle(event)
	if err := comCall(client, clientSetEventHandle, uintptr(event)).err("SetEventHandle"); err != nil {
		fail(err)
		return
	}
	var capture unsafe.Pointer
	if err := comCall(client, clientGetService, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capture))).err("GetService(IAudioCaptureClient)"); err != nil {
		fail(err)
		return
	}
	defer release(capture)
	if err := comCall(client, clientStart).err("IAudioClient.Start"); err != nil {
		fail(err)
		return
	}
	defer comCall(client, clientStop)
	started = true
	ready <- nil

	var buf []byte
	for {
		select {
		case <-a.stop:
			return
		default:
		}
		if ev, _ := windows.WaitForSingleObject(event, 100); ev != windows.WAIT_OBJECT_0 {
			continue
		}
		for {
			var frames uint32
			if comCall(capture, captureGetNextPacketSize, uintptr(unsafe.Pointer(&frames))) < 0 || frames == 0 {
				break
			}
			var data unsafe.Pointer
			var bufFlags uint32
			if comCall(capture, captureGetBuffer, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&bufFlags)), 0, 0) < 0 {
				break
			}
			n := int(frames) * audioBytesPerFrame
			if cap(buf) < n {
				buf = make([]byte, n)
			}
			buf = buf[:n]
			if bufFlags&0x2 == 0 && data != nil { // not AUDCLNT_BUFFERFLAGS_SILENT
				copy(buf, unsafe.Slice((*byte)(data), n))
			} else {
				clear(buf)
			}
			comCall(capture, captureReleaseBuffer, uintptr(frames))
			if n > 0 {
				onPCM(buf)
			}
		}
	}
}

// soundPID is the process whose tree a source's sound comes from: a window's program (for a Store
// app, the one inside the frame window, not the frame's host); 0 for a monitor, or a window of
// this app.
func soundPID(source string) uint32 {
	tg, err := target(source)
	if err != nil || tg.hwnd == 0 {
		return 0
	}
	var pid uint32
	_, _ = windows.GetWindowThreadProcessId(tg.hwnd, &pid)
	class, _ := windows.UTF16PtrFromString("Windows.UI.Core.CoreWindow")
	if core, _, _ := procFindWindowExW.Call(uintptr(tg.hwnd), 0, uintptr(unsafe.Pointer(class)), 0); core != 0 {
		var inner uint32
		if _, _ = windows.GetWindowThreadProcessId(windows.HWND(core), &inner); inner != 0 {
			pid = inner
		}
	}
	if pid == uint32(os.Getpid()) {
		return 0
	}
	return pid
}

// activateLoopback opens an IAudioClient on the process loopback of pid's process tree, or with
// pid 0 of everything but ours.
func activateLoopback(pid uint32) (unsafe.Pointer, error) {
	if err := procActivateAudioInterfaceAsync.Find(); err != nil {
		return nil, err
	}
	params := struct {
		Type, TargetPID, Mode uint32
	}{1 /* PROCESS_LOOPBACK */, uint32(os.Getpid()), 1 /* EXCLUDE_TARGET_PROCESS_TREE */}
	if pid != 0 {
		params.TargetPID, params.Mode = pid, 0 // INCLUDE_TARGET_PROCESS_TREE
	}
	blob := struct {
		vt   uint16
		_    [3]uint16
		size uint32
		_    uint32
		data unsafe.Pointer
	}{vt: 65 /* VT_BLOB */, size: uint32(unsafe.Sizeof(params)), data: unsafe.Pointer(&params)}
	path, _ := windows.UTF16PtrFromString(`VAD\Process_Loopback`)
	h := &activateHandler{vtbl: &activateHandlerVtbl, done: make(chan struct{}, 1)}
	handlers.Store(h, struct{}{})
	defer time.AfterFunc(5*time.Second, func() { handlers.Delete(h) })
	var op unsafe.Pointer
	if err := procCall(procActivateAudioInterfaceAsync, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&iidIAudioClient)),
		uintptr(unsafe.Pointer(&blob)), uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&op))).err("ActivateAudioInterfaceAsync"); err != nil {
		return nil, err
	}
	defer release(op)
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		return nil, errors.New("the system did not open the sound capture")
	}
	var hr hresult
	var client unsafe.Pointer
	if err := comCall(op, asyncOpGetActivateResult, uintptr(unsafe.Pointer(&hr)), uintptr(unsafe.Pointer(&client))).err("GetActivateResult"); err != nil {
		return nil, err
	}
	if err := hr.err("process loopback"); err != nil {
		release(client)
		return nil, err
	}
	return client, nil
}
