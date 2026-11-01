//go:build windows

package nativecap

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// pinterfaceIID computes the IID of a parameterized WinRT interface from its signature (a
// version 5 UUID in the WinRT namespace), the way the runtime does.
func pinterfaceIID(signature string) windows.GUID {
	ns := []byte{0x11, 0xf4, 0x7a, 0xd5, 0x7b, 0x73, 0x42, 0xc0, 0xab, 0xae, 0x87, 0x8b, 0x1e, 0x16, 0xad, 0xee}
	sum := sha1.Sum(append(ns, signature...))
	sum[6] = sum[6]&0x0f | 0x50
	sum[8] = sum[8]&0x3f | 0x80
	g := windows.GUID{Data1: binary.BigEndian.Uint32(sum[0:4]), Data2: binary.BigEndian.Uint16(sum[4:6]), Data3: binary.BigEndian.Uint16(sum[6:8])}
	copy(g.Data4[:], sum[8:16])
	return g
}

func TestFrameArrivedHandlerIID(t *testing.T) {
	sig := "pinterface({9de1c534-6ae1-11e0-84e1-18a905bcc53f};rc(Windows.Graphics.Capture.Direct3D11CaptureFramePool;{24eb6d22-1975-422e-82e7-780dbd8ddf24});cinterface(IInspectable))"
	if got := pinterfaceIID(sig); got != iidFrameArrivedHandler {
		t.Fatalf("TypedEventHandler<Direct3D11CaptureFramePool, Object> is %v, the table says %v", got, iidFrameArrivedHandler)
	}
}

func TestSourcesListsMonitors(t *testing.T) {
	src := Sources()
	mons := 0
	for _, s := range src {
		if s.Kind == "monitor" {
			mons++
		}
		t.Logf("%-16s %-8s %4dx%-4d %3d Hz min=%-5v %s %s", s.ID, s.Kind, s.Width, s.Height, s.RefreshHz, s.Minimized, s.App, s.Title)
	}
	if mons == 0 {
		t.Fatal("no monitor listed")
	}
}

// TestCaptureBench captures a source for a while, converts every frame to NV12 at the target
// rate and reports how many frames came: the capture's own limit, without an encoder.
//
//	ECHOFOG_CAPTURE_BENCH=monitor:0 go test -run TestCaptureBench -v ./internal/nativecap/
//
// _FPS (120), _SECONDS (10), _SIZE (2560x1440), _INTERVAL_MS (MinUpdateInterval, 1), _PNG (a
// file to save the last captured frame to).
func TestCaptureBench(t *testing.T) {
	id := os.Getenv("ECHOFOG_CAPTURE_BENCH")
	if id == "" {
		t.Skip("set ECHOFOG_CAPTURE_BENCH to a source ID")
	}
	fps := envInt("ECHOFOG_CAPTURE_BENCH_FPS", 120)
	seconds := envInt("ECHOFOG_CAPTURE_BENCH_SECONDS", 10)
	minIv := time.Duration(envInt("ECHOFOG_CAPTURE_BENCH_INTERVAL_MS", 1)) * time.Millisecond
	maxW, maxH := 2560, 1440
	if s := os.Getenv("ECHOFOG_CAPTURE_BENCH_SIZE"); s != "" {
		var w, h int
		if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err == nil {
			maxW, maxH = w, h
		}
	}

	undo, err := mtaThread()
	if err != nil {
		t.Fatal(err)
	}
	defer undo()
	if !captureSupported() {
		t.Skip("Windows.Graphics.Capture is not supported here")
	}
	d, err := newDevice()
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	t.Logf("adapter: %s (vendor 0x%04x)", d.adapterName, d.adapterVendor)
	tg, err := target(id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newWGCCapture(d, tg, true, minIv)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	outW, outH := outputSize(int(c.contentW), int(c.contentH), maxW, maxH)
	t.Logf("source %dx%d, output %dx%d, target %d fps, MinUpdateInterval %v", c.contentW, c.contentH, outW, outH, fps, minIv)
	conv, err := newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conv.close() }()
	nv12, err := d.texture(texture2DDesc{Width: uint32(outW), Height: uint32(outH), Format: dxgiFormatNV12, BindFlags: d3d11BindRenderTarget | d3d11BindVideoEncoder})
	if err != nil {
		t.Logf("NV12 with the encoder bind flag failed (%v), trying without", err)
		if nv12, err = d.texture(texture2DDesc{Width: uint32(outW), Height: uint32(outH), Format: dxgiFormatNV12, BindFlags: d3d11BindRenderTarget}); err != nil {
			t.Fatal(err)
		}
	}
	defer release(nv12)
	view, err := conv.outputView(nv12)
	if err != nil {
		t.Fatal(err)
	}
	defer release(view)

	start := time.Now()
	end := start.Add(time.Duration(seconds) * time.Second)
	clock := newFrameClock(fps, start)
	tick := time.NewTimer(time.Until(clock.next))
	defer tick.Stop()
	var arrivals []time.Time
	var captured, converted, repeats int
	var convTime time.Duration
	secStart, secCap, secConv := start, 0, 0
	convert := func(now time.Time, repeat bool) {
		if repeat {
			repeats++
		}
		t0 := time.Now()
		if err := conv.convert(c.latest, c.contentW, c.contentH, view); err != nil {
			t.Fatal(err)
		}
		d.flush()
		convTime += time.Since(t0)
		converted++
		secConv++
		clock.sent(now)
	}
	for time.Now().Before(end) {
		select {
		case <-c.arrived():
			got, resized, err := c.next()
			if err != nil {
				t.Fatal(err)
			}
			if resized {
				conv.close()
				if conv, err = newConverter(d, uint32(c.poolW), uint32(c.poolH), uint32(outW), uint32(outH)); err != nil {
					t.Fatal(err)
				}
				if view2, err := conv.outputView(nv12); err == nil {
					release(view)
					view = view2
				}
			}
			if got {
				captured++
				secCap++
				arrivals = append(arrivals, time.Now())
				if clock.captured() {
					convert(time.Now(), false)
				}
			}
		case now := <-tick.C:
			if fresh := clock.fresh; clock.beat(now) {
				convert(now, !fresh)
			}
			tick.Reset(time.Until(clock.next))
		}
		if now := time.Now(); now.Sub(secStart) >= time.Second {
			t.Logf("captured %3d fps, converted %3d fps", secCap, secConv)
			secStart, secCap, secConv = now, 0, 0
		}
	}
	el := time.Since(start).Seconds()
	gaps := make([]float64, 0, len(arrivals))
	for i := 1; i < len(arrivals); i++ {
		gaps = append(gaps, float64(arrivals[i].Sub(arrivals[i-1]).Microseconds())/1000)
	}
	sort.Float64s(gaps)
	pct := func(p float64) float64 {
		if len(gaps) == 0 {
			return 0
		}
		return gaps[min(len(gaps)-1, int(p/100*float64(len(gaps))))]
	}
	t.Logf("captured %.1f fps, converted %.1f fps (%d repeats), %.3f ms CPU per conversion; frame gaps ms: median %.2f p95 %.2f p99 %.2f max %.2f",
		float64(captured)/el, float64(converted)/el, repeats, float64(convTime.Microseconds())/1000/float64(max(converted, 1)), pct(50), pct(95), pct(99), pct(100))
	if err := d.removed(); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("ECHOFOG_CAPTURE_BENCH_PNG"); path != "" {
		px, w, h, err := d.readPixels(c.latest)
		if err != nil {
			t.Fatal(err)
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i < w*h; i++ { // BGRA -> RGBA
			img.Pix[i*4], img.Pix[i*4+1], img.Pix[i*4+2], img.Pix[i*4+3] = px[i*4+2], px[i*4+1], px[i*4], 255
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		t.Logf("last frame saved to %s", path)
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}
