package call

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/require"
)

// TestViewerBench plays a recorded H.264 clip (Annex-B) through a broadcaster and a viewer node of
// a real room, as the screen of the broadcaster, and serves a page that watches it the way the app
// does: through the viewer's media bridge. The page reports once a second what the browser decodes
// and shows, so the frame rate a viewer really gets can be measured without two computers.
//
//	ECHOFOG_VIEWER_BENCH=clip.h264 go test -run TestViewerBench -v ./internal/services/call/
//
// then open http://127.0.0.1:8765 in Edge (the engine of WebView2). Also read:
// ECHOFOG_VIEWER_BENCH_FPS (120), _QUALITY (1440p120), _SECONDS (40), _ADDR (127.0.0.1:8765).
func TestViewerBench(t *testing.T) {
	clip := os.Getenv("ECHOFOG_VIEWER_BENCH")
	if clip == "" {
		t.Skip("set ECHOFOG_VIEWER_BENCH to an Annex-B H.264 clip")
	}
	data, err := os.ReadFile(clip)
	require.NoError(t, err)
	units := accessUnits(data)
	require.NotEmpty(t, units, "no access units in the clip")
	fps := benchEnvInt("ECHOFOG_VIEWER_BENCH_FPS", 120)
	seconds := benchEnvInt("ECHOFOG_VIEWER_BENCH_SECONDS", 40)
	qid := os.Getenv("ECHOFOG_VIEWER_BENCH_QUALITY")
	if qid == "" {
		qid = "1440p120"
	}
	addr := os.Getenv("ECHOFOG_VIEWER_BENCH_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8765"
	}
	t.Logf("clip: %d frames, %.1f MB, played at %d fps", len(units), float64(len(data))/1e6, fps)

	bn := newBenchNet()
	sender, viewer := bn.node(), bn.node()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	prof := tree.PeerNodeProfile{UploadKbps: 100000, MaxRelayChildren: 4}
	require.NoError(t, sender.JoinRoom("bench", sender.selfID, key, prof))
	require.NoError(t, viewer.JoinRoom("bench", sender.selfID, key, prof))
	t.Cleanup(func() {
		_ = viewer.LeaveRoom()
		_ = sender.LeaveRoom()
	})
	require.Eventually(t, func() bool {
		s, ok := viewer.Snapshot()
		return ok && s.Joined && len(s.Members) == 2
	}, 5*time.Second, 20*time.Millisecond, "room did not form")

	q, err := tree.QualityByID(qid)
	require.NoError(t, err)
	const streamID = "screen_bench"
	require.NoError(t, sender.StartStream(streamID, "screen", q))
	require.NoError(t, viewer.WatchStream(streamID))

	// the page: offer and answer through the viewer's bridge, then reports
	reports := make(chan map[string]any, 64)
	answered := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, benchPage)
	})
	mux.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		offer, err := viewer.GetBridgeOffer()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, offer)
	})
	mux.HandleFunc("/answer", func(w http.ResponseWriter, r *http.Request) {
		sdp, _ := io.ReadAll(r.Body)
		if err := viewer.SubmitBridgeAnswer(string(sdp)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		once.Do(func() { close(answered) })
	})
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		var rep map[string]any
		if json.NewDecoder(r.Body).Decode(&rep) == nil {
			select {
			case reports <- rep:
			default:
			}
		}
	})
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Logf("open http://%s in Edge", addr)

	select {
	case <-answered:
	case <-time.After(120 * time.Second):
		t.Fatal("no page connected")
	}

	// the broadcaster: one access unit per frame interval, its packets spread over the interval
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	var sentFrames, sentBytes atomic.Uint64
	var lastSeq atomic.Uint32
	go func() {
		mtu := uint16(1100)
		pz := rtp.NewPacketizer(mtu, 112, 0x5eed, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
		interval := time.Second / time.Duration(fps)
		start := time.Now()
		for i := 0; ctx.Err() == nil; i++ {
			due := start.Add(time.Duration(i) * interval)
			if d := time.Until(due); d > 0 {
				time.Sleep(d)
			}
			pkts := pz.Packetize(units[i%len(units)], uint32(90000/fps))
			const bursts = 4
			per := (len(pkts) + bursts - 1) / bursts
			for b := 0; b*per < len(pkts); b++ {
				if b > 0 {
					time.Sleep(interval / bursts)
				}
				for _, p := range pkts[b*per : min((b+1)*per, len(pkts))] {
					raw, err := p.Marshal()
					if err != nil {
						continue
					}
					sender.onBridgeRTP(bridge.RoleScreen, raw)
					lastSeq.Store(uint32(p.SequenceNumber))
					sentBytes.Add(uint64(len(raw)))
				}
			}
			sentFrames.Add(1)
		}
	}()

	var lastFrames, lastBytes uint64
	for {
		select {
		case <-ctx.Done():
			return
		case rep := <-reports:
			f, b := sentFrames.Load(), sentBytes.Load()
			line, _ := json.Marshal(rep)
			t.Logf("sent %3d fps %5.1f Mbit/s seq %5d frame %d | viewer %s", f-lastFrames, float64(b-lastBytes)*8/1e6, lastSeq.Load(), f%uint64(len(units)), line)
			lastFrames, lastBytes = f, b
		}
	}
}

func benchEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// accessUnits splits an Annex-B stream into access units (frames), each still in Annex-B. A unit
// starts at an access unit delimiter, parameter set or SEI after a slice, or at a slice whose
// first_mb_in_slice is 0.
func accessUnits(b []byte) [][]byte {
	var out [][]byte
	var cur []byte
	haveSlice := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
		}
		cur, haveSlice = nil, false
	}
	for _, nal := range annexBNALs(b) {
		typ := nal[0] & 0x1f
		switch {
		case typ == 9 || typ == 7 || typ == 8 || typ == 6:
			if haveSlice {
				flush()
			}
		case typ == 1 || typ == 5:
			if haveSlice && len(nal) > 1 && nal[1]&0x80 != 0 { // first_mb_in_slice == 0
				flush()
			}
			haveSlice = true
		}
		cur = append(cur, 0, 0, 0, 1)
		cur = append(cur, nal...)
	}
	flush()
	return out
}

// annexBNALs returns the NAL units of an Annex-B stream, without their start codes.
func annexBNALs(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = append(out, bytes.TrimRight(b[start:i], "\x00"))
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// benchNet delivers frames in order through one queue per node, like a link does.
type benchNet struct {
	mu    sync.Mutex
	nodes map[types.PeerID]chan benchFrame
}

type benchFrame struct {
	from types.PeerID
	typ  transport.Kind
	data []byte
}

func newBenchNet() *benchNet { return &benchNet{nodes: make(map[types.PeerID]chan benchFrame)} }

type benchPort struct {
	n    *benchNet
	self types.PeerID
}

func (p benchPort) SendRawFrame(to types.PeerID, t transport.Kind, payload []byte) error {
	return p.n.deliver(p.self, to, t, payload)
}

func (p benchPort) SendUnreliableFrame(to types.PeerID, t transport.Kind, payload []byte) error {
	return p.n.deliver(p.self, to, t, payload)
}

func (n *benchNet) deliver(from, to types.PeerID, t transport.Kind, payload []byte) error {
	n.mu.Lock()
	q := n.nodes[to]
	n.mu.Unlock()
	if q == nil {
		return fmt.Errorf("peer %s not connected", to)
	}
	select {
	case q <- benchFrame{from, t, append([]byte(nil), payload...)}:
	default: // a full link drops, like a datagram
	}
	return nil
}

func (n *benchNet) node() *CallManager {
	s := newTestSigner()
	cm := newCallManager(benchPort{n, s.id()}, s, s.id())
	q := make(chan benchFrame, 1<<15)
	n.mu.Lock()
	n.nodes[s.id()] = q
	n.mu.Unlock()
	go func() {
		for f := range q {
			switch f.typ {
			case transport.Signal, transport.SignalLossy:
				_ = cm.handleIncomingSignaling(f.data, f.from)
			case transport.MediaAudio:
				_ = cm.handleIncomingAudio(f.data, f.from)
			case transport.MediaVideo:
				_ = cm.handleIncomingVideo(f.data, f.from)
			}
		}
	}()
	return cm
}

// benchPage watches the stream on the viewer's video slot and reports what the browser does with
// it: frames decoded and dropped by WebRTC, and frames presented on screen (requestVideoFrameCallback).
const benchPage = `<!doctype html><meta charset="utf-8"><title>viewer bench</title>
<style>body{margin:0;background:#000;color:#0f0;font:14px monospace}video{width:100vw;height:calc(100vh - 24px)}</style>
<div id="info">connecting</div><video id="v" autoplay muted playsinline></video>
<script>
(async () => {
  const info = document.getElementById('info'), v = document.getElementById('v');
  const pc = new RTCPeerConnection({ iceServers: [] });
  let rx = null;
  pc.ontrack = (ev) => {
    if (ev.track.kind !== 'video' || (ev.streams[0] && ev.streams[0].id !== 'efslot-v0')) return;
    rx = ev.receiver;
    if ('jitterBufferTarget' in rx) rx.jitterBufferTarget = 100;
    v.srcObject = ev.streams[0] || new MediaStream([ev.track]);
  };
  const offer = await (await fetch('/offer')).text();
  await pc.setRemoteDescription({ type: 'offer', sdp: offer });
  await pc.setLocalDescription(await pc.createAnswer());
  await new Promise((ok) => { if (pc.iceGatheringState === 'complete') ok(); pc.onicegatheringstatechange = () => pc.iceGatheringState === 'complete' && ok(); setTimeout(ok, 1500); });
  await fetch('/answer', { method: 'POST', body: pc.localDescription.sdp });
  let presented = 0;
  const onFrame = (now, meta) => { presented = meta.presentedFrames; v.requestVideoFrameCallback(onFrame); };
  v.requestVideoFrameCallback(onFrame);
  let prev = null, prevPresented = 0;
  setInterval(async () => {
    if (!rx) return;
    let r = null, tr = null;
    (await rx.getStats()).forEach((s) => { if (s.type === 'inbound-rtp' && s.kind === 'video') r = s; if (s.type === 'transport') tr = s; });
    if (!r) return;
    const dt = prev ? (r.timestamp - prev.timestamp) / 1000 : 0;
    const per = (k) => (prev && dt > 0 ? Math.round(((r[k] || 0) - (prev[k] || 0)) / dt) : 0);
    const rep = {
      size: r.frameWidth + 'x' + r.frameHeight, fps: Math.round(r.framesPerSecond || 0),
      received: per('framesReceived'), decoded: per('framesDecoded'), dropped: per('framesDropped'),
      presented: dt > 0 ? Math.round((presented - prevPresented) / dt) : 0,
      mbps: prev && dt > 0 ? Math.round(((r.bytesReceived - prev.bytesReceived) * 8) / dt / 1e4) / 100 : 0,
      lost: r.packetsLost, freezes: r.freezeCount, pli: r.pliCount,
      decodeMs: r.framesDecoded > (prev ? prev.framesDecoded : 0) ? Math.round(((r.totalDecodeTime - (prev ? prev.totalDecodeTime : 0)) * 1e4) / (r.framesDecoded - (prev ? prev.framesDecoded : 0))) / 10 : 0,
      decoder: r.decoderImplementation, hw: r.powerEfficientDecoder,
      pc: pc.connectionState, transportMB: tr ? Math.round(tr.bytesReceived / 1e5) / 10 : -1, transportPkts: tr ? tr.packetsReceived : -1,
      seq: r.packetsReceived,
    };
    prev = r; prevPresented = presented;
    info.textContent = JSON.stringify(rep);
    fetch('/report', { method: 'POST', body: JSON.stringify(rep) });
  }, 1000);
})().catch((e) => { document.getElementById('info').textContent = 'error: ' + e; });
</script>`
