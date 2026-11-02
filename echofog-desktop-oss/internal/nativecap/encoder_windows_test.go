//go:build windows

package nativecap

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// TestEncoderQualityBench encodes the same clip with the hardware encoder at several speed and
// quality settings and reports how long a frame takes; the .h264 files it writes are compared with
// the clip by ffmpeg (PSNR, SSIM, VMAF), which says what each setting buys.
//
//	ECHOFOG_ENC_BENCH=clip.mkv ECHOFOG_ENC_BENCH_OUT=dir go test -run TestEncoderQualityBench -v ./internal/nativecap/
//
// _SIZE (1920x1080), _FPS (60), _KBPS (12000), _QUALITIES (0,33,66,100), _BUFFER (the rate buffer in
// frames, 6). ffmpeg must be on the path.
func TestEncoderQualityBench(t *testing.T) {
	src := os.Getenv("ECHOFOG_ENC_BENCH")
	if src == "" {
		t.Skip("set ECHOFOG_ENC_BENCH to a clip ffmpeg can read")
	}
	if err := Available(); err != nil {
		t.Skip(err)
	}
	w, h := 1920, 1080
	if s := os.Getenv("ECHOFOG_ENC_BENCH_SIZE"); s != "" {
		_, _ = fmt.Sscanf(s, "%dx%d", &w, &h)
	}
	fps := envInt("ECHOFOG_ENC_BENCH_FPS", 60)
	bufferFrames = envInt("ECHOFOG_ENC_BENCH_BUFFER", bufferFrames)
	kbps := envInt("ECHOFOG_ENC_BENCH_KBPS", 12000)
	outDir := os.Getenv("ECHOFOG_ENC_BENCH_OUT")
	qualities := []int{0, encoderQuality, 66, 100}
	if s := os.Getenv("ECHOFOG_ENC_BENCH_QUALITIES"); s != "" {
		qualities = nil
		for _, f := range strings.Split(s, ",") {
			q, _ := strconv.Atoi(f)
			qualities = append(qualities, q)
		}
	}
	for _, q := range qualities {
		var out io.Writer = io.Discard
		if outDir != "" {
			f, err := os.Create(filepath.Join(outDir, fmt.Sprintf("q%d.h264", q)))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			out = f
		}
		took, bytes, frames, wall, err := encodeClip(src, w, h, fps, kbps, q, out)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(took)
		avg := time.Duration(0)
		for _, d := range took {
			avg += d
		}
		avg /= time.Duration(max(len(took), 1))
		t.Logf("quality %3d: %d frames, %.1f Mbit/s, a frame takes %4.1f ms (p95 %4.1f ms), %4.0f frames/s in all",
			q, frames, float64(bytes)*8/(float64(frames)/float64(fps))/1e6, ms(avg), ms(took[len(took)*95/100]), float64(frames)/wall.Seconds())
	}
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// encodeClip feeds the clip's frames to a new encoder as fast as it takes them.
func encodeClip(src string, w, h, fps, kbps, quality int, out io.Writer) (took []time.Duration, bytes, frames int, wall time.Duration, err error) {
	undo, err := mtaThread()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer undo()
	d, err := newDevice()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer d.close()
	staging, err := d.texture(texture2DDesc{Width: uint32(w), Height: uint32(h), Format: dxgiFormatNV12, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessWrite})
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer release(staging)
	// a ring larger than the encoder ever holds
	var ring [8]unsafe.Pointer
	for i := range ring {
		if ring[i], err = d.texture(texture2DDesc{Width: uint32(w), Height: uint32(h), Format: dxgiFormatNV12, BindFlags: d3d11BindRenderTarget | d3d11BindVideoEncoder}); err != nil {
			return nil, 0, 0, 0, err
		}
		defer release(ring[i])
	}
	var mu sync.Mutex
	enc, err := newMFEncoder(d, w, h, fps, kbps, quality, func(f encodedFrame) {
		mu.Lock()
		took = append(took, f.took)
		bytes += len(f.data)
		_, _ = out.Write(f.data)
		mu.Unlock()
	})
	if err != nil {
		return nil, 0, 0, 0, err
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-i", src, "-vf", fmt.Sprintf("scale=%d:%d", w, h), "-pix_fmt", "nv12", "-f", "rawvideo", "-")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, 0, 0, err
	}
	defer cmd.Wait()
	frame := make([]byte, w*h*3/2)
	start := time.Now()
	for ; ; frames++ {
		if _, err := io.ReadFull(pipe, frame); err != nil {
			break
		}
		var mapped struct {
			Data       unsafe.Pointer
			RowPitch   uint32
			DepthPitch uint32
		}
		if err := comCall(d.ctx, ctxMap, uintptr(staging), 0, 2 /* D3D11_MAP_WRITE */, 0, uintptr(unsafe.Pointer(&mapped))).err("Map"); err != nil {
			return nil, 0, 0, 0, err
		}
		// NV12: h rows of luma, then h/2 rows of interleaved chroma, at the mapped pitch
		for y := 0; y < h*3/2; y++ {
			copy(unsafe.Slice((*byte)(unsafe.Add(mapped.Data, uintptr(y)*uintptr(mapped.RowPitch))), w), frame[y*w:(y+1)*w])
		}
		comCall(d.ctx, ctxUnmap, uintptr(staging), 0)
		tex := ring[frames%len(ring)]
		d.copyResource(tex, staging)
		d.flush()
		for !enc.ready() {
			select {
			case <-enc.asked:
			case <-time.After(2 * time.Second):
				return nil, 0, 0, 0, fmt.Errorf("the encoder stopped asking for frames: %v", enc.failed())
			}
		}
		if err := enc.encode(tex, time.Duration(frames)*time.Second/time.Duration(fps), frames == 0); err != nil {
			return nil, 0, 0, 0, err
		}
	}
	enc.drain(2 * time.Second)
	wall = time.Since(start)
	enc.close()
	mu.Lock()
	defer mu.Unlock()
	return took, bytes, frames, wall, nil
}
