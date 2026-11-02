//go:build windows

package nativecap

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// TestStreamBench runs the whole native path (capture, NV12, hardware H.264, RTP, pacer) on a
// source and reports once a second what it sends. Halfway it asks for a keyframe and halves the
// bitrate, as the call's rate control and a joining viewer do.
//
//	ECHOFOG_STREAM_BENCH=monitor:0 go test -run TestStreamBench -v ./internal/nativecap/
//
// _FPS (120), _KBPS (32000), _SECONDS (10), _SIZE (2560x1440), _OUT (an .h264 file to write the
// stream to, depacketized again, for TestViewerBench of the core), _LEVELS (any: the call's presets
// as the ladder, so the halved bitrate moves the stream down), _VIEWERS (the viewers' screens, e.g.
// "60,144").
func TestStreamBench(t *testing.T) {
	id := os.Getenv("ECHOFOG_STREAM_BENCH")
	if id == "" {
		t.Skip("set ECHOFOG_STREAM_BENCH to a source ID")
	}
	if err := Available(); err != nil {
		t.Skip(err)
	}
	fps := envInt("ECHOFOG_STREAM_BENCH_FPS", 120)
	kbps := envInt("ECHOFOG_STREAM_BENCH_KBPS", 32000)
	seconds := envInt("ECHOFOG_STREAM_BENCH_SECONDS", 10)
	w, h := 2560, 1440
	if s := os.Getenv("ECHOFOG_STREAM_BENCH_SIZE"); s != "" {
		_, _ = fmt.Sscanf(s, "%dx%d", &w, &h)
	}

	var mu sync.Mutex
	var out *os.File
	if path := os.Getenv("ECHOFOG_STREAM_BENCH_OUT"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	var depack codecs.H264Packet
	var lastSeq uint16
	var lost, maxPacket int
	first := true
	// per frame (RTP timestamp): whether it carried parameter sets, an IDR, or both; parameter
	// sets in a frame that is not an IDR make some hardware decoders give up until the next one
	var frameTS uint32
	var frameSets, frameIDR bool
	var setsOutsideIDR, idrFrames int
	deltas := map[int]int{} // RTP timestamp steps between frames, in tenths of a millisecond
	endFrame := func() {
		if frameSets && !frameIDR {
			setsOutsideIDR++
		}
		if frameIDR {
			idrFrames++
		}
		frameSets, frameIDR = false, false
	}
	publish := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		maxPacket = max(maxPacket, len(b))
		var p rtp.Packet
		if err := p.Unmarshal(b); err != nil {
			t.Errorf("bad RTP: %v", err)
			return
		}
		if !first && p.SequenceNumber != lastSeq+1 {
			lost++
		}
		if !first && p.Timestamp != frameTS {
			endFrame()
			deltas[int(int64(p.Timestamp-frameTS)*10000/90000)]++
		}
		first, lastSeq, frameTS = false, p.SequenceNumber, p.Timestamp
		if nal, err := depack.Unmarshal(p.Payload); err == nil && len(nal) > 0 {
			for _, n := range annexBNALs(nal) {
				switch n[0] & 0x1f {
				case 7, 8:
					frameSets = true
				case 5:
					frameIDR = true
				}
			}
			if out != nil {
				_, _ = out.Write(nal)
			}
		}
	}
	ended := make(chan error, 1)
	o := Options{Source: id, Width: w, Height: h, FPS: fps, BitrateKbps: kbps, Cursor: true, Publish: publish, MTU: 1100,
		OnEnd: func(err error) { ended <- err }}
	if os.Getenv("ECHOFOG_STREAM_BENCH_LEVELS") != "" {
		o.Levels = testPresets
		o.OnLevel = func(l Level, why string) { t.Logf("level %dx%d %d fps (%s)", l.Width, l.Height, l.FPS, why) }
	}
	s, err := Start(o)
	if err != nil {
		t.Fatal(err)
	}
	if v := os.Getenv("ECHOFOG_STREAM_BENCH_VIEWERS"); v != "" {
		var hz []int
		for _, f := range strings.Split(v, ",") {
			n, _ := strconv.Atoi(f)
			hz = append(hz, n)
		}
		s.SetViewerRates(hz)
	}
	st := s.Stats()
	t.Logf("%s on %s: %dx%d from %dx%d, %d fps, %d kbps", st.Encoder, st.Adapter, st.Width, st.Height, st.SourceWidth, st.SourceHeight, fps, kbps)
	prev := st
	var keyAt uint64
	if os.Getenv("ECHOFOG_STREAM_BENCH_FLAP") != "" {
		// the rate control moves the bitrate every half second on a busy link
		go func() {
			for i := 0; ; i++ {
				select {
				case <-ended:
					return
				case <-time.After(500 * time.Millisecond):
				}
				f := uint64(1000)
				if i%2 == 0 {
					f = 800
				}
				s.SetBitrate(uint64(kbps) * f)
			}
		}()
	}
	for sec := 1; sec <= seconds; sec++ {
		select {
		case err := <-ended:
			t.Fatalf("the stream ended by itself: %v", err)
		case <-time.After(time.Second):
		}
		cur := s.Stats()
		enc := cur.Encoded - prev.Encoded
		ms := 0.0
		if enc > 0 {
			ms = (cur.EncodeSeconds - prev.EncodeSeconds) * 1000 / float64(enc)
		}
		t.Logf("captured %3d encoded %3d sent %3d skipped %2d | %5.1f Mbit/s | encode %4.1f ms | keyframes %d | pacer %4.1f ms | rate %d kbps | %dx%d %d fps %s",
			cur.Captured-prev.Captured, enc, cur.Sent-prev.Sent, cur.Skipped-prev.Skipped, float64(cur.Bytes-prev.Bytes)*8/1e6, ms,
			cur.Keyframes, cur.PacerDelayMs, cur.BitrateKbps, cur.Width, cur.Height, cur.FPS, cur.Lowered)
		prev = cur
		if sec == seconds/2 {
			keyAt = cur.Keyframes
			s.RequestKeyframe()
			s.SetBitrate(uint64(kbps) * 500)
			t.Log("asked for a keyframe, bitrate halved")
		}
	}
	s.Stop()
	final := s.Stats()
	mu.Lock()
	defer mu.Unlock()
	endFrame()
	steps := make([]int, 0, len(deltas))
	for d := range deltas {
		steps = append(steps, d)
	}
	sort.Ints(steps)
	line := ""
	for _, d := range steps {
		if deltas[d] >= 5 {
			line += fmt.Sprintf(" %.1fms×%d", float64(d)/10, deltas[d])
		}
	}
	t.Logf("frame time steps (5 or more):%s", line)
	t.Logf("largest packet %d bytes, %d sequence gaps, %d keyframes (%d IDR frames on the wire), %d frames with SPS/PPS but no IDR",
		maxPacket, lost, final.Keyframes, idrFrames, setsOutsideIDR)
	if setsOutsideIDR > 0 {
		t.Errorf("parameter sets outside a keyframe")
	}
	if maxPacket > 1100 || lost > 0 {
		t.Fatalf("packets over the MTU or out of sequence")
	}
	if final.Keyframes <= keyAt {
		t.Fatalf("the keyframe asked for did not come")
	}
}
