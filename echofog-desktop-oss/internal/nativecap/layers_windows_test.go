//go:build windows

package nativecap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// encodeSynthetic encodes frames of a moving test picture with a new encoder asked for layers
// temporal layers, a keyframe at each frame keyAt reports, and returns what came out and how many
// layers the encoder took.
func encodeSynthetic(w, h, fps, kbps, layers, frames int, keyAt func(int) bool) ([]encodedFrame, int, string, error) {
	undo, err := mtaThread()
	if err != nil {
		return nil, 0, "", err
	}
	defer undo()
	d, err := newDevice()
	if err != nil {
		return nil, 0, "", err
	}
	defer d.close()
	staging, err := d.texture(texture2DDesc{Width: uint32(w), Height: uint32(h), Format: dxgiFormatNV12, Usage: d3d11UsageStaging, CPUAccessFlags: d3d11CPUAccessWrite})
	if err != nil {
		return nil, 0, "", err
	}
	defer release(staging)
	var ring [8]unsafe.Pointer
	for i := range ring {
		if ring[i], err = d.texture(texture2DDesc{Width: uint32(w), Height: uint32(h), Format: dxgiFormatNV12, BindFlags: d3d11BindRenderTarget | d3d11BindVideoEncoder}); err != nil {
			return nil, 0, "", err
		}
		defer release(ring[i])
	}
	var mu sync.Mutex
	var out []encodedFrame
	enc, err := newMFEncoder(d, w, h, fps, kbps, encoderQuality, layers, func(f encodedFrame) {
		mu.Lock()
		out = append(out, f)
		mu.Unlock()
	})
	if err != nil {
		return nil, 0, "", err
	}
	took, name := enc.layers, enc.name
	for i := 0; i < frames; i++ {
		var mapped struct {
			Data       unsafe.Pointer
			RowPitch   uint32
			DepthPitch uint32
		}
		if err := comCall(d.ctx, ctxMap, uintptr(staging), 0, 2 /* D3D11_MAP_WRITE */, 0, uintptr(unsafe.Pointer(&mapped))).err("Map"); err != nil {
			enc.close()
			return nil, 0, "", err
		}
		// a diagonal pattern that moves every frame, so every frame has something to code
		for y := 0; y < h*3/2; y++ {
			row := unsafe.Slice((*byte)(unsafe.Add(mapped.Data, uintptr(y)*uintptr(mapped.RowPitch))), w)
			for x := range row {
				row[x] = byte((x + y + 3*i) ^ (x * i >> 4))
			}
		}
		comCall(d.ctx, ctxUnmap, uintptr(staging), 0)
		tex := ring[i%len(ring)]
		d.copyResource(tex, staging)
		d.flush()
		for !enc.ready() {
			select {
			case <-enc.asked:
			case <-time.After(2 * time.Second):
				enc.close()
				return nil, 0, "", fmt.Errorf("the encoder stopped asking for frames: %v", enc.failed())
			}
		}
		if err := enc.encode(tex, time.Duration(i)*time.Second/time.Duration(fps), keyAt(i)); err != nil {
			enc.close()
			return nil, 0, "", err
		}
	}
	enc.drain(2 * time.Second)
	enc.close()
	mu.Lock()
	defer mu.Unlock()
	return out, took, name, nil
}

// TestEncoderTemporalLayers asks this machine's hardware encoder for three temporal layers and
// says whether it makes them. If it does, every frame's layer is read, the base and the middle
// layer must decode on their own (ffmpeg, when it is on the path), and frames of the top layer must
// be ones nothing refers to. If it does not, the stream is one layer, unmarked, as before.
func TestEncoderTemporalLayers(t *testing.T) {
	if err := Available(); err != nil {
		t.Skip(err)
	}
	const n, fps = 96, 60
	frames, layers, name, err := encodeSynthetic(1280, 720, fps, 6000, 3, n, func(i int) bool { return i == 0 || i == 50 })
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < n-4 {
		t.Fatalf("%d of %d frames came out", len(frames), n)
	}
	if layers <= 1 {
		t.Logf("%s: temporal layers OFF (the encoder took none); the stream goes out as one layer", name)
		for _, f := range frames {
			if f.layered {
				t.Fatal("a frame of a one-layer encoder is marked layered")
			}
		}
		return
	}
	t.Logf("%s: temporal layers ON, %d layers", name, layers)
	perLayer := map[uint8]int{}
	byLayer := map[uint8][]byte{}
	prefixSeen := false
	for i, f := range frames {
		if !f.layered {
			t.Fatalf("frame %d is not marked layered", i)
		}
		for _, nal := range annexBNALs(f.data) {
			if nal[0]&0x1f == 14 {
				t.Fatalf("frame %d still has a prefix NAL unit", i)
			}
		}
		l := f.layer
		perLayer[l.tid]++
		if f.keyframe && l.tid != 0 {
			t.Fatalf("keyframe %d in layer %d", i, l.tid)
		}
		if int(l.tid) == layers-1 && !l.discardable {
			t.Fatalf("frame %d of the top layer is a reference", i)
		}
		if l.tid > 0 {
			prefixSeen = true
		}
		for c := l.tid; c < uint8(layers); c++ {
			byLayer[c] = append(byLayer[c], f.data...)
		}
	}
	if !prefixSeen {
		t.Fatal("the encoder took the layer count but no frame went above the base")
	}
	t.Logf("frames per layer: %v", perLayer)
	if perLayer[0] > len(frames)/2 || perLayer[uint8(layers-1)] < len(frames)/3 {
		t.Fatalf("layers do not look dyadic: %v", perLayer)
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Logf("no ffprobe on the path: decoding of the lower layers alone not checked")
		return
	}
	dir := t.TempDir()
	want := 0
	for c := uint8(0); c < uint8(layers); c++ {
		want += perLayer[c]
		path := filepath.Join(dir, fmt.Sprintf("upto%d.h264", c))
		if err := os.WriteFile(path, byLayer[c], 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0",
			"-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", path).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe: %v: %s", err, out)
		}
		lines := strings.Fields(strings.TrimSpace(string(out)))
		if len(lines) != 1 {
			t.Fatalf("layers up to %d: the decoder complained: %s", c, out)
		}
		got, _ := strconv.Atoi(lines[0])
		if got != want {
			t.Fatalf("layers up to %d: %d frames decoded of %d", c, got, want)
		}
		t.Logf("layers up to %d decode on their own: %d frames (%d fps of %d)", c, got, fps*got/len(frames), fps)
	}
}
