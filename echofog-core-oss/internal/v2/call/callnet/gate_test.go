package callnet_test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
)

// T9.5: the quality gate of streams and voice on v2, on an in-memory stand.
//
// Every scenario runs twice on the same stand with the same synthetic encoder: once over callnet
// (v2: route choice, admit, rate bucket) and once over a plain transport that stands for v1's (no
// route choice, no admit, no bucket). The call layer is the real CallManager in both. The gate
// compares the two (AD21: fps not below v1, "screen to screen" latency not above v1 + 10 %) and
// checks the absolute limits of the task. The stand has no crypto, no kernel and no real network,
// so it measures what the call layer and the v2 transport add; real 4K capture, GPU and CPU are
// the owner's manual checks (reports/T9.5.md).
//
// Run the full gate (one run per machine, under a heavy lock, never next to other tests):
//
//	ECHOFOG_GATE=1 go test ./internal/v2/call/callnet/ -run TestGate -count=1 -v -timeout 20m
//
// Without ECHOFOG_GATE (and always under -race) each scenario is a short smoke run that only
// checks that media flows.

func fullGate() bool { return os.Getenv("ECHOFOG_GATE") == "1" && !raceOn }

func gateWindow() time.Duration {
	if fullGate() {
		return 6 * time.Second
	}
	return 1500 * time.Millisecond
}

// ---- the stand ----

// gnode is one participant: the real call manager and what its viewer side saw.
type gnode struct {
	id types.PeerID
	cm *call.CallManager

	mu      sync.Mutex
	frameAt map[int]time.Time // video frame index → arrival of its last packet
	audio   []audioRx
}

type audioRx struct {
	from      types.PeerID
	seq       uint32
	sent, got time.Time
}

func (n *gnode) onMedia(p *media.MediaPacket) {
	switch p.Kind {
	case media.FrameVideo:
		var pk rtp.Packet
		if pk.Unmarshal(p.Payload) != nil || len(pk.Payload) < 5 || !pk.Marker {
			return
		}
		f := int(binary.BigEndian.Uint32(pk.Payload[1:5]))
		now := time.Now()
		n.mu.Lock()
		if _, ok := n.frameAt[f]; !ok {
			n.frameAt[f] = now
		}
		n.mu.Unlock()
	case media.FrameAudio:
		if len(p.Payload) < 12 {
			return
		}
		rx := audioRx{from: p.Broadcaster, seq: binary.BigEndian.Uint32(p.Payload[0:4]),
			sent: time.Unix(0, int64(binary.BigEndian.Uint64(p.Payload[4:12]))), got: time.Now()}
		n.mu.Lock()
		n.audio = append(n.audio, rx)
		n.mu.Unlock()
	}
}

// plainNet is the baseline transport, standing for v1's: it sends on the best live path kind and
// delivers whatever arrives. Same delays as the v2 fabric.
type plainNet struct {
	mu     sync.Mutex
	nodes  map[types.PeerID]*plainTr
	kinds  map[[2]types.PeerID]map[mux.PathKind]int
	extra  time.Duration
	sentBy map[types.PeerID]int
	sentTo map[types.PeerID]int
}

type plainTr struct {
	pn   *plainNet
	self types.PeerID
	mu   sync.RWMutex
	h    map[transport.Kind]transport.Handler
}

func (t *plainTr) RegisterFastFrameHandler(k transport.Kind, h transport.Handler) {
	t.mu.Lock()
	t.h[k] = h
	t.mu.Unlock()
}

func (t *plainTr) SendRawFrame(peer types.PeerID, k transport.Kind, payload []byte) error {
	return t.send(peer, k, payload)
}

func (t *plainTr) SendUnreliableFrame(peer types.PeerID, k transport.Kind, payload []byte) error {
	return t.send(peer, k, payload)
}

func (t *plainTr) send(peer types.PeerID, k transport.Kind, payload []byte) error {
	pn := t.pn
	pn.mu.Lock()
	kinds := pn.kinds[[2]types.PeerID{t.self, peer}]
	target := pn.nodes[peer]
	if len(kinds) == 0 || target == nil {
		pn.mu.Unlock()
		return callnet.ErrNoLink
	}
	delay := 2 * time.Millisecond
	isMedia := k == transport.MediaAudio || k == transport.MediaVideo || k == transport.MediaAuth
	if isMedia {
		delay += pn.extra
		if kinds[mux.PathDirect] == 0 {
			delay += 8 * time.Millisecond
		}
		pn.sentBy[t.self]++
		pn.sentTo[peer]++
	}
	pn.mu.Unlock()
	data := append([]byte(nil), payload...)
	from := t.self
	time.AfterFunc(delay, func() {
		target.mu.RLock()
		h := target.h[k]
		if h == nil && k == transport.Signal {
			h = target.h[transport.SignalLossy]
		}
		target.mu.RUnlock()
		if h != nil {
			_ = h(data, from)
		}
	})
	return nil
}

func (t *plainTr) Relayed(peer types.PeerID) bool {
	t.pn.mu.Lock()
	defer t.pn.mu.Unlock()
	k := t.pn.kinds[[2]types.PeerID{t.self, peer}]
	return len(k) > 0 && k[mux.PathDirect] == 0
}

func (t *plainTr) DirectLink(peer types.PeerID) bool {
	t.pn.mu.Lock()
	defer t.pn.mu.Unlock()
	return t.pn.kinds[[2]types.PeerID{t.self, peer}][mux.PathDirect] > 0
}

// gcall is a room of n participants on one of the two stands.
type gcall struct {
	nodes  []*gnode
	sentBy func(types.PeerID) int
	sentTo func(types.PeerID) int
	// cut takes the member's sessions away and returns the function that gives them back.
	cut func(types.PeerID) func()
	// crash cuts a member off without a word: nothing reaches it, nothing it sends goes out.
	crash func(types.PeerID)
}

var (
	bothKinds  = map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}
	relayKinds = map[mux.PathKind]int{mux.PathRelay: 1}
)

func buildGate(t *testing.T, v2 bool, n int, kinds func(i, j int) map[mux.PathKind]int, extra time.Duration, prof func(i int) tree.PeerNodeProfile) *gcall {
	t.Helper()
	gc := &gcall{nodes: make([]*gnode, n)}
	var f *fabric
	var pn *plainNet
	if v2 {
		f = newFabric()
		f.extra = extra
	} else {
		pn = &plainNet{nodes: map[types.PeerID]*plainTr{}, kinds: map[[2]types.PeerID]map[mux.PathKind]int{}, extra: extra,
			sentBy: map[types.PeerID]int{}, sentTo: map[types.PeerID]int{}}
	}
	for i := range gc.nodes {
		s := newSigner()
		g := &gnode{id: s.id(), frameAt: map[int]time.Time{}}
		var tr transport.Transport
		if v2 {
			ct := callnet.New(callnet.Config{Links: fabricLinks{f, g.id}, Signals: signalBus{f, g.id}, Route: func() route.Route { return route.Auto }})
			f.mu.Lock()
			f.nodes[g.id] = ct
			f.mu.Unlock()
			tr = ct
		} else {
			pt := &plainTr{pn: pn, self: g.id, h: map[transport.Kind]transport.Handler{}}
			pn.mu.Lock()
			pn.nodes[g.id] = pt
			pn.mu.Unlock()
			tr = pt
		}
		g.cm = call.NewCallManager(tr, s, g.id)
		g.cm.OnMedia = g.onMedia
		g.cm.SetPeerConnector(func(context.Context, types.PeerID) error { return nil })
		gc.nodes[i] = g
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			k := kinds(i, j)
			a, b := gc.nodes[i].id, gc.nodes[j].id
			if v2 {
				f.setKinds(a, b, k)
			} else {
				pn.mu.Lock()
				pn.kinds[[2]types.PeerID{a, b}], pn.kinds[[2]types.PeerID{b, a}] = k, k
				pn.mu.Unlock()
			}
		}
	}
	if v2 {
		gc.sentBy = func(id types.PeerID) int { f.mu.Lock(); defer f.mu.Unlock(); return f.sentBy[id] }
		gc.sentTo = func(id types.PeerID) int { f.mu.Lock(); defer f.mu.Unlock(); return f.sentTo[id] }
	} else {
		gc.sentBy = func(id types.PeerID) int { pn.mu.Lock(); defer pn.mu.Unlock(); return pn.sentBy[id] }
		gc.sentTo = func(id types.PeerID) int { pn.mu.Lock(); defer pn.mu.Unlock(); return pn.sentTo[id] }
	}
	gc.cut = func(id types.PeerID) func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		saved := map[[2]types.PeerID]map[mux.PathKind]int{}
		for _, o := range gc.nodes {
			for _, k := range [][2]types.PeerID{{id, o.id}, {o.id, id}} {
				if v, ok := f.kinds[k]; ok {
					saved[k] = v
					delete(f.kinds, k)
				}
			}
		}
		return func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			for k, v := range saved {
				f.kinds[k] = v
			}
		}
	}
	gc.crash = func(id types.PeerID) {
		if v2 {
			f.mu.Lock()
			delete(f.nodes, id)
			for _, o := range gc.nodes {
				// no live session any more: Link answers (nil, false), as the runtime's does
				delete(f.kinds, [2]types.PeerID{id, o.id})
				delete(f.kinds, [2]types.PeerID{o.id, id})
			}
			f.mu.Unlock()
			return
		}
		pn.mu.Lock()
		delete(pn.nodes, id)
		for _, o := range gc.nodes {
			delete(pn.kinds, [2]types.PeerID{id, o.id})
			delete(pn.kinds, [2]types.PeerID{o.id, id})
		}
		pn.mu.Unlock()
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	coord := gc.nodes[0].id
	for i, g := range gc.nodes {
		require.NoError(t, g.cm.JoinRoom("room-95", coord, key, prof(i)))
	}
	t.Cleanup(func() {
		for _, g := range gc.nodes {
			_ = g.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool {
		for _, g := range gc.nodes {
			s, ok := g.cm.Snapshot()
			if !ok || !s.Joined || len(s.Members) != n {
				return false
			}
		}
		return true
	}, testutil.Slow(15*time.Second), 20*time.Millisecond, "the room forms")
	return gc
}

// ---- the synthetic encoder (L1T3, as the native one) ----

type gatePump struct {
	fps  int
	bps  atomic.Uint64
	key  atomic.Bool
	stop chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	sent map[int]time.Time
}

func (p *gatePump) SetBitrate(bps uint64) { p.bps.Store(bps) }
func (p *gatePump) RequestKeyframe()      { p.key.Store(true) }
func (p *gatePump) halt()                 { close(p.stop); p.wg.Wait() }

func (p *gatePump) run(cm *call.CallManager) {
	p.stop = make(chan struct{})
	p.sent = map[int]time.Time{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		period := time.Second / time.Duration(p.fps)
		start := time.Now()
		seq := uint16(1)
		for f := 0; ; f++ {
			if d := time.Until(start.Add(time.Duration(f) * period)); d > 0 {
				select {
				case <-p.stop:
					return
				case <-time.After(d):
				}
			} else {
				select {
				case <-p.stop:
					return
				default:
				}
			}
			p.mu.Lock()
			p.sent[f] = time.Now()
			p.mu.Unlock()
			tid := [4]uint8{0, 2, 1, 2}[f%4]
			key := p.key.Swap(false) || f == 0
			if key {
				tid = 0
			}
			size := int(p.bps.Load()/8/uint64(p.fps)) * 4 / 9
			if tid == 0 {
				size = size * 3 / 2
			}
			if key {
				size *= 4
			}
			n := max(1, (size+999)/1000)
			for i := 0; i < n; i++ {
				pl := make([]byte, 1000-12-videolayer.ExtensionLen)
				pl[0] = 0x41
				if key {
					pl[0] = 0x65
				}
				binary.BigEndian.PutUint32(pl[1:5], uint32(f))
				pk := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: uint32(f * 90000 / p.fps), SSRC: 9, Marker: i == n-1}, Payload: pl}
				pk.Header.Extension, pk.Header.ExtensionProfile = true, 0xBEDE
				m := videolayer.Mark{TID: tid, Start: i == 0, End: i == n-1, Key: key, Discardable: tid == 2, Index: uint16(i), Count: uint16(n)}
				_ = pk.Header.SetExtension(videolayer.ExtensionID, m.Data())
				b, _ := pk.Marshal()
				seq++
				cm.PublishLocalRTP(bridge.RoleScreen, b)
			}
		}
	}()
}

// ---- measurement ----

type viewerRes struct {
	fps, loss float64
	lat       []time.Duration
}

type streamRes struct {
	fpsMedian, fpsMin float64
	loss              float64 // worst viewer
	lat50, lat95      time.Duration
	fanout            float64 // most datagrams a relaying viewer sends, over what a viewer receives
	viewers           int
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

// gateFrames reports what every viewer has received, so a failure of the stream gate names the
// viewer that fell behind instead of only the deadline.
func gateFrames(gc *gcall) string {
	var b strings.Builder
	for i, g := range gc.nodes[1:] {
		g.mu.Lock()
		n := len(g.frameAt)
		g.mu.Unlock()
		fmt.Fprintf(&b, "viewer %d: %d frames; ", i+1, n)
	}
	return b.String()
}

// runStream: node 0 broadcasts a screen stream at the preset to the viewers, 1..viewers.
func runStream(t *testing.T, v2 bool, qid string, viewers int, relayed bool, win time.Duration) streamRes {
	t.Helper()
	k := bothKinds
	if relayed {
		k = relayKinds
	}
	gc := buildGate(t, v2, viewers+1, func(i, j int) map[mux.PathKind]int { return k }, 0, func(i int) tree.PeerNodeProfile {
		switch {
		case i == 0:
			return tree.PeerNodeProfile{UploadKbps: 100000, MaxRelayChildren: 4}
		case i <= 4:
			return tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}
		}
		return tree.PeerNodeProfile{UploadKbps: 5000}
	})
	q, err := tree.QualityByID(qid)
	require.NoError(t, err)
	b := gc.nodes[0]
	pump := &gatePump{fps: q.FPS}
	b.cm.SetLocalEncoder(bridge.RoleScreen, pump)
	require.NoError(t, b.cm.StartStream("s", "screen", q))
	for _, g := range gc.nodes[1:] {
		require.NoError(t, g.cm.WatchStream("s"))
	}
	require.Eventually(t, func() bool { return pump.bps.Load() > 0 }, testutil.Slow(5*time.Second), 20*time.Millisecond)
	pump.run(b.cm)
	defer pump.halt()
	require.Eventually(t, func() bool {
		for _, g := range gc.nodes[1:] {
			g.mu.Lock()
			n := len(g.frameAt)
			g.mu.Unlock()
			if n < q.FPS/2 {
				return false
			}
		}
		return true
	}, testutil.Slow(30*time.Second), 50*time.Millisecond, "every viewer receives the stream: %s", gateFrames(gc))

	// The gate measures a steady window. A viewer re-parented inside one gets nothing for as long
	// as the move takes, and a window with a topology change is not what the gate promises, so a
	// window in which some viewer received nothing at all is measured again. A viewer that never
	// receives still fails: three windows are the bound.
	var res streamRes
	for attempt := 1; ; attempt++ {
		var dead int
		res, dead = measureStream(gc, pump, win)
		if dead == 0 || attempt == 3 {
			break
		}
		t.Logf("gate: %d of %d viewers received nothing in the window, measuring again (%d/3)", dead, viewers, attempt)
	}
	return res
}

// measureStream measures one window of the running stream: what every viewer received of what the
// pump sent inside it, the latency of those frames and the fanout of the tree. dead is the number
// of viewers that received nothing at all in the window.
func measureStream(gc *gcall, pump *gatePump, win time.Duration) (res streamRes, dead int) {
	t0 := time.Now()
	time.Sleep(win)
	t1 := time.Now()
	pump.mu.Lock()
	sentFrames := map[int]time.Time{}
	for f, ts := range pump.sent {
		if !ts.Before(t0) && ts.Before(t1.Add(-300*time.Millisecond)) {
			sentFrames[f] = ts
		}
	}
	pump.mu.Unlock()

	var fps []float64
	var all []time.Duration
	res.viewers = len(gc.nodes) - 1
	for _, g := range gc.nodes[1:] {
		g.mu.Lock()
		got := 0
		for f, ts := range sentFrames {
			if at, ok := g.frameAt[f]; ok {
				got++
				all = append(all, at.Sub(ts))
			}
		}
		g.mu.Unlock()
		if got == 0 {
			dead++
		}
		fps = append(fps, float64(got)/(win-300*time.Millisecond).Seconds())
		res.loss = max(res.loss, 1-float64(got)/float64(max(1, len(sentFrames))))
	}
	sort.Float64s(fps)
	res.fpsMin, res.fpsMedian = fps[0], fps[len(fps)/2]
	res.lat50, res.lat95 = pct(all, 0.5), pct(all, 0.95)
	var recv float64
	for _, g := range gc.nodes[1:] {
		recv += float64(gc.sentTo(g.id))
	}
	recv /= float64(res.viewers)
	for _, g := range gc.nodes[1:] {
		res.fanout = max(res.fanout, float64(gc.sentBy(g.id))/recv)
	}
	return res, dead
}

func (r streamRes) String() string {
	return fmt.Sprintf("fps=%.1f (min %.1f) lat50=%v lat95=%v loss=%.2f%% fanout=%.2f", r.fpsMedian, r.fpsMin,
		r.lat50.Round(100*time.Microsecond), r.lat95.Round(100*time.Microsecond), r.loss*100, r.fanout)
}

// ---- the gate ----

// gateReps is how many times a scenario pair (v1, v2) is repeated; the gate looks at the median of
// the per-pair ratios, so a loaded machine that slows one run does not decide it.
func gateReps() int {
	if fullGate() {
		return 3
	}
	return 1
}

func medianF(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// ratios of one scenario over its repeats. The pairs run back to back (v1 then v2), so a change of
// load between pairs moves both halves of a ratio together.
type pairStats struct {
	fps, lat50, lat95    []float64 // v2 over v1
	loss, v1loss, fanout []float64 // loss of v2 and of v1; fanout of v2
	v2fps, v1fps         []float64
	v2lat50, v1lat50     []float64
	v2lat95, v1lat95     []float64
}

func (p *pairStats) add(v1, v2 streamRes, minFPS bool) {
	f1, f2 := v1.fpsMedian, v2.fpsMedian
	if minFPS {
		f1, f2 = v1.fpsMin, v2.fpsMin
	}
	p.v1fps, p.v2fps = append(p.v1fps, f1), append(p.v2fps, f2)
	p.fps = append(p.fps, f2/max(f1, 1e-9))
	p.lat50 = append(p.lat50, float64(v2.lat50)/max(float64(v1.lat50), 1))
	p.lat95 = append(p.lat95, float64(v2.lat95)/max(float64(v1.lat95), 1))
	p.v1lat50, p.v2lat50 = append(p.v1lat50, float64(v1.lat50)), append(p.v2lat50, float64(v2.lat50))
	p.v1lat95, p.v2lat95 = append(p.v1lat95, float64(v1.lat95)), append(p.v2lat95, float64(v2.lat95))
	p.loss, p.v1loss = append(p.loss, v2.loss), append(p.v1loss, v1.loss)
	p.fanout = append(p.fanout, v2.fanout)
}

func minF(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = min(m, x)
	}
	return m
}

func spread(v []float64) string {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return fmt.Sprintf("%.2f..%.2f", s[0], s[len(s)-1])
}

func (p *pairStats) check(t *testing.T, nominalFPS float64, direct bool) {
	t.Helper()
	t.Logf("GATE ratios v2/v1 (median of %d, spread): fps %.3f [%s]  lat50 %.2f [%s]  lat95 %.2f [%s]  worst loss %.2f%%  worst fanout %.2f",
		len(p.fps), medianF(p.fps), spread(p.fps), medianF(p.lat50), spread(p.lat50), medianF(p.lat95), spread(p.lat95),
		max(0, medianF(p.loss))*100, medianF(p.fanout))
	require.GreaterOrEqual(t, medianF(p.fps), gate.FPSRatio, "AD21: fps not below v1")
	// the latency ratio plus the absolute slack for timer noise of the stand
	slack := float64(gate.LatencySlack)
	require.LessOrEqual(t, medianF(p.v2lat50), gate.LatencyRatio*medianF(p.v1lat50)+slack, "AD21: median latency not above v1 + 10 %")
	require.LessOrEqual(t, minF(p.v2lat95), gate.LatencyRatio*minF(p.v1lat95)+2*slack, "AD21: best-run p95 latency not above v1 + 10 % (the tail gets twice the slack)")
	require.LessOrEqual(t, medianF(p.loss), medianF(p.v1loss)+gate.MaxLoss, "frames lost: no more than v1 loses on the same stand, plus a little")
	if direct {
		require.GreaterOrEqual(t, medianF(p.v2fps), gate.DirectFPS*nominalFPS, "1440p60 direct without degradation")
	}
}

func TestGateStreams(t *testing.T) {
	type tc struct {
		q       string
		relayed bool
	}
	cases := []tc{{"1440p60", false}, {"1440p144", false}, {"2160p60", false}, {"2160p60", true}}
	if !fullGate() {
		cases = cases[:1]
	}
	win := gateWindow()
	for _, c := range cases {
		path := "direct"
		if c.relayed {
			path = "relay"
		}
		t.Run(c.q+"/"+path, func(t *testing.T) {
			var ps pairStats
			for r := 0; r < gateReps(); r++ {
				v1 := runStream(t, false, c.q, 1, c.relayed, win)
				v2 := runStream(t, true, c.q, 1, c.relayed, win)
				t.Logf("GATE stream %s %s #%d v1: %v", c.q, path, r+1, v1)
				t.Logf("GATE stream %s %s #%d v2: %v", c.q, path, r+1, v2)
				require.Greater(t, v2.fpsMedian, 0.0, "media flows")
				ps.add(v1, v2, false)
			}
			if !fullGate() {
				return
			}
			q, _ := tree.QualityByID(c.q)
			ps.check(t, float64(q.FPS), c.q == "1440p60" && !c.relayed)
		})
	}
}

func TestGateTenViewers(t *testing.T) {
	win := gateWindow()
	n := 10
	if !fullGate() {
		n = 6
	}
	var ps pairStats
	for r := 0; r < gateReps(); r++ {
		v1 := runStream(t, false, "1440p60", n, false, win)
		v2 := runStream(t, true, "1440p60", n, false, win)
		t.Logf("GATE viewers%d 1440p60 #%d v1: %v", n, r+1, v1)
		t.Logf("GATE viewers%d 1440p60 #%d v2: %v", n, r+1, v2)
		require.Greater(t, v2.fpsMin, 0.0, "every viewer gets media")
		ps.add(v1, v2, true)
	}
	if !fullGate() {
		return
	}
	ps.check(t, 60, false)
	require.LessOrEqual(t, medianF(ps.fanout), gate.RelayFanout, "a relaying viewer feeds no more children than it declared")
}

// ---- voice ----

type voiceRes struct {
	p50, p95   time.Duration
	leaverSent int           // datagrams the leaving member had sent: 0 means it relayed for nobody
	rehang     time.Duration // longest silence a listener heard after the relaying member left
	dropped    time.Duration // how long the coordinator kept listing the member that went (-1: never)
	loss       float64
}

// runVoice: five participants, two of them reachable only through relays; the second speaks, and a
// third (a relaying member) leaves midway.
func runVoice(t *testing.T, v2 bool, extra, dur time.Duration, crash bool) voiceRes {
	t.Helper()
	// The tree this makes: 0 → {2, 1}, 2 → {3, 4}. Members 3 and 4 reach 1 and 2 only through relay
	// paths (and each other), so the speaker's voice crosses a relay path on its last hop, and
	// member 2, which feeds them, is the one that leaves.
	gc := buildGate(t, v2, 5, func(i, j int) map[mux.PathKind]int {
		if min(i, j) >= 1 && max(i, j) >= 3 {
			return relayKinds
		}
		return bothKinds
	}, extra, func(i int) tree.PeerNodeProfile {
		switch i {
		case 0: // the coordinator feeds two members itself
			return tree.PeerNodeProfile{UploadKbps: 50000, MaxRelayChildren: 2}
		case 2:
			return tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}
		}
		return tree.PeerNodeProfile{UploadKbps: 3000 - 100*(i-1), IsMobile: true} // leaves: 1 (the speaker), 3, 4
	})
	speaker, leaver := gc.nodes[1], gc.nodes[2]
	cands := []*gnode{gc.nodes[2], gc.nodes[3], gc.nodes[4]}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var sentN int
	go func() {
		defer wg.Done()
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for seq := uint32(0); ; seq++ {
			select {
			case <-stop:
				return
			case <-tk.C:
			}
			pl := make([]byte, 80)
			binary.BigEndian.PutUint32(pl[0:4], seq)
			binary.BigEndian.PutUint64(pl[4:12], uint64(time.Now().UnixNano()))
			if speaker.cm.PublishAudio(pl) == nil {
				sentN++
			}
		}
	}()
	time.Sleep(dur / 3)
	// the member that relays the most (the speaker and the coordinator stay) is the one that leaves
	best := -1
	for _, c := range cands {
		if e := gc.sentBy(c.id); e > best {
			best, leaver = e, c
		}
	}
	leftAt := time.Now()
	if crash {
		gc.crash(leaver.id)
	} else {
		_ = leaver.cm.LeaveRoom()
	}
	// when does the coordinator stop listing the member that went? (reference, goes to the log)
	dropped := time.Duration(-1)
	for end := time.Now().Add(dur - dur/3); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if dropped < 0 {
			if s, ok := gc.nodes[0].cm.Snapshot(); ok && !slices.Contains(s.Members, leaver.id) {
				dropped = time.Since(leftAt)
			}
		}
	}
	res0drop := dropped
	close(stop)
	wg.Wait()
	endAt := time.Now()

	res0 := best
	var listeners []*gnode
	for _, g := range gc.nodes {
		if g != speaker && g != leaver {
			listeners = append(listeners, g)
		}
	}
	res := voiceRes{leaverSent: res0, dropped: res0drop}
	var lat []time.Duration
	var got int
	for _, l := range listeners {
		l.mu.Lock()
		rx := append([]audioRx(nil), l.audio...)
		l.mu.Unlock()
		last := leftAt
		for _, a := range rx {
			if a.from != speaker.id {
				continue
			}
			got++
			if a.sent.Before(leftAt) {
				lat = append(lat, a.got.Sub(a.sent))
			}
			if a.got.After(leftAt) {
				res.rehang = max(res.rehang, a.got.Sub(last))
				last = a.got
			}
		}
		res.rehang = max(res.rehang, endAt.Sub(last)) // a listener that went silent to the end
	}
	res.p50, res.p95 = pct(lat, 0.5), pct(lat, 0.95)
	if sentN > 0 {
		res.loss = max(0, 1-float64(got)/float64(sentN*len(listeners)))
	}
	return res
}

func TestGateVoiceRoomOfFive(t *testing.T) {
	extra, dur, reps := time.Duration(0), 3*time.Second, 1
	if fullGate() {
		extra, dur, reps = 40*time.Millisecond, 14*time.Second, 2 // an artificial 40 ms on every hop
	}
	for _, crash := range []bool{false, true} {
		how := "leaves"
		if crash {
			how = "crashes"
		}
		var p95v1, p95v2, rh1, rh2 []float64
		for r := 0; r < reps; r++ {
			v1 := runVoice(t, false, extra, dur, crash)
			v2 := runVoice(t, true, extra, dur, crash)
			for _, x := range []struct {
				name string
				v    voiceRes
			}{{"v1", v1}, {"v2", v2}} {
				t.Logf("GATE voice5 (relaying member %s) #%d %s: p50=%v p95=%v rehang=%v loss=%.1f%% leaver-sent=%d coord-dropped-after=%v", how, r+1, x.name,
					x.v.p50.Round(time.Millisecond), x.v.p95.Round(time.Millisecond), x.v.rehang.Round(time.Millisecond), x.v.loss*100, x.v.leaverSent, x.v.dropped.Round(10*time.Millisecond))
			}
			require.NotZero(t, v2.p50, "voice arrives")
			p95v1, p95v2 = append(p95v1, float64(v1.p95)), append(p95v2, float64(v2.p95))
			rh1, rh2 = append(rh1, float64(v1.rehang)), append(rh2, float64(v2.rehang))
		}
		if !fullGate() {
			continue
		}
		t.Logf("GATE voice5 (%s) medians: p95 v1 %v v2 %v; rehang v1 %v v2 %v [spread v2 %s ms]", how,
			time.Duration(medianF(p95v1)).Round(time.Millisecond), time.Duration(medianF(p95v2)).Round(time.Millisecond),
			time.Duration(medianF(rh1)).Round(time.Millisecond), time.Duration(medianF(rh2)).Round(time.Millisecond), spread(scaleMs(rh2)))
		require.Less(t, time.Duration(medianF(p95v2)), gate.VoiceP95, "voice below 250 ms one way")
		require.LessOrEqual(t, time.Duration(medianF(rh2)), time.Duration(gate.RehangRatio*medianF(rh1))+gate.Rehang,
			"the tree re-hangs after the relaying member "+how+" as fast as v1's")
	}
}

// TestJitterBlipKeepsTheVoiceTree: a member's sessions go quiet for 300 ms, well inside the
// watchdogs (parent 800 ms, member 2.5 s). The tree must not change: the coordinator still lists
// the member and its epoch stays.
func TestJitterBlipKeepsTheVoiceTree(t *testing.T) {
	gc := buildGate(t, true, 5, func(i, j int) map[mux.PathKind]int { return bothKinds }, 0, func(i int) tree.PeerNodeProfile {
		if i == 0 || i == 2 {
			return tree.PeerNodeProfile{UploadKbps: 40000 - 100*i, MaxRelayChildren: 2}
		}
		return tree.PeerNodeProfile{UploadKbps: 3000 - 100*i, IsMobile: true}
	})
	coord, blip := gc.nodes[0], gc.nodes[2]
	time.Sleep(1500 * time.Millisecond)
	before, ok := coord.cm.Snapshot()
	require.True(t, ok)
	require.Contains(t, before.Members, blip.id)
	saved := gc.cut(blip.id)
	time.Sleep(300 * time.Millisecond)
	saved()
	time.Sleep(2500 * time.Millisecond)
	after, ok := coord.cm.Snapshot()
	require.True(t, ok)
	require.Contains(t, after.Members, blip.id, "a 300 ms blip does not drop the member")
	require.Equal(t, before.Epoch, after.Epoch, "and does not rebuild the tree")
}

func scaleMs(v []float64) []float64 {
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / float64(time.Millisecond)
	}
	return out
}
