package callnet_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/bridge"
	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
)

// The T9.3 scenario: a call of five participants, two of them reachable only through a relay, a
// stream with three temporal layers (L1T3), and one weak receiver. The call layer is the real
// CallManager (trees, layers, media keys); the transport is callnet over a fabric that stands for
// the participants' sessions: each pair has a set of live path kinds, a datagram sent on a kind
// the pair lacks fails exactly like mux.Session.SendDatagramVia, and every datagram is recorded
// with the kind it travelled on. The real mux.Session paths are covered at the node level
// (node: TestCallMediaFollowsCallRouteOnARelayedAndPunchedSession).

type fabric struct {
	mu       sync.Mutex
	nodes    map[types.PeerID]*callnet.Transport
	kinds    map[[2]types.PeerID]map[mux.PathKind]int
	sentOn   map[mux.PathKind]int // CALL_AUDIO / CALL_VIDEO datagrams by path kind
	shapers  map[[2]types.PeerID]*shaper
	relayHop int
}

// shaper is a bottleneck: it sends kbps and drops what waits longer than queue.
type shaper struct {
	mu      sync.Mutex
	kbps    float64
	queue   time.Duration
	free    time.Time
	dropped atomic.Uint64
	passed  atomic.Uint64
}

func (s *shaper) admit(size int, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := now
	if s.free.After(now) {
		start = s.free
	}
	if start.Sub(now) > s.queue {
		s.dropped.Add(1)
		return 0, false
	}
	s.free = start.Add(time.Duration(float64(size*8) / s.kbps * float64(time.Millisecond)))
	s.passed.Add(1)
	return s.free.Sub(now), true
}

func newFabric() *fabric {
	return &fabric{
		nodes:   map[types.PeerID]*callnet.Transport{},
		kinds:   map[[2]types.PeerID]map[mux.PathKind]int{},
		sentOn:  map[mux.PathKind]int{},
		shapers: map[[2]types.PeerID]*shaper{},
	}
}

func (f *fabric) setKinds(a, b types.PeerID, k map[mux.PathKind]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kinds[[2]types.PeerID{a, b}] = k
	f.kinds[[2]types.PeerID{b, a}] = k
}

// link is one participant's view of its session with another.
type link struct {
	f        *fabric
	from, to types.PeerID
}

func (l link) PathKinds() map[mux.PathKind]int {
	l.f.mu.Lock()
	defer l.f.mu.Unlock()
	out := map[mux.PathKind]int{}
	for k, v := range l.f.kinds[[2]types.PeerID{l.from, l.to}] {
		out[k] = v
	}
	return out
}

func (l link) SendDatagramVia(kind mux.PathKind, typ uint64, payload []byte) error {
	f := l.f
	f.mu.Lock()
	if f.kinds[[2]types.PeerID{l.from, l.to}][kind] == 0 {
		f.mu.Unlock()
		return mux.ErrNoPathKind
	}
	if typ == frame.DgramCallAudio || typ == frame.DgramCallVideo {
		f.sentOn[kind]++
	}
	target := f.nodes[l.to]
	sh := f.shapers[[2]types.PeerID{l.from, l.to}]
	f.mu.Unlock()
	if target == nil {
		return nil
	}
	data := append([]byte(nil), payload...)
	delay := 2 * time.Millisecond
	if kind == mux.PathRelay {
		delay += 8 * time.Millisecond // the extra hop
	}
	if sh != nil {
		d, ok := sh.admit(len(data)+30, time.Now())
		if !ok {
			return nil
		}
		delay += d
	}
	in := callnet.Inbound{Device: l.from, Type: typ, Payload: data, Path: kind}
	time.AfterFunc(delay, func() { target.Deliver(in) })
	return nil
}

type fabricLinks struct {
	f    *fabric
	self types.PeerID
}

func (l fabricLinks) Link(device [32]byte) (route.Link, bool) {
	l.f.mu.Lock()
	_, ok := l.f.kinds[[2]types.PeerID{l.self, device}]
	l.f.mu.Unlock()
	if !ok {
		return nil, false
	}
	return link{l.f, l.self, device}, true
}

// signals stand for call/2 SIGNAL between participants: delivered whole, over the session.
type signalBus struct {
	f    *fabric
	self types.PeerID
}

func (b signalBus) SendSignal(to [32]byte, lossy bool, payload []byte) error {
	b.f.mu.Lock()
	target := b.f.nodes[to]
	b.f.mu.Unlock()
	if target == nil {
		return nil
	}
	data := append([]byte(nil), payload...)
	time.AfterFunc(2*time.Millisecond, func() {
		target.Deliver(callnet.Inbound{Device: b.self, Type: frame.DgramCallSignal, Payload: data, Path: mux.PathRelay})
	})
	return nil
}

type signer struct {
	priv ed25519.PrivateKey
	pub  types.PeerPublicKey
}

func newSigner() *signer {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	s := &signer{priv: priv}
	copy(s.pub[:], pub)
	return s
}

func (s *signer) CreateSignature(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }
func (s *signer) GetMyPubKey() types.PeerPublicKey         { return s.pub }
func (s *signer) id() types.PeerID                         { return types.PeerPubKeyToID(s.pub) }

type participant struct {
	id  types.PeerID
	cm  *call.CallManager
	tr  *callnet.Transport
	tap *layerTap
}

// layerPump stands for an encoder with three temporal layers (L1T3), 60 frames a second.
type layerPump struct {
	bps  atomic.Uint64
	key  atomic.Bool
	stop chan struct{}
	wg   sync.WaitGroup
}

func (p *layerPump) SetBitrate(bps uint64) { p.bps.Store(bps) }
func (p *layerPump) RequestKeyframe()      { p.key.Store(true) }
func (p *layerPump) halt()                 { close(p.stop); p.wg.Wait() }

func (p *layerPump) run(cm *call.CallManager) {
	p.stop = make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(time.Second / 60)
		defer t.Stop()
		seq := uint16(1)
		for f := 0; ; f++ {
			select {
			case <-p.stop:
				return
			case <-t.C:
			}
			tid := [4]uint8{0, 2, 1, 2}[f%4]
			key := p.key.Swap(false) || f == 0
			if key {
				tid = 0
			}
			size := int(p.bps.Load()/8/60) * 4 / 9
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
				pk := rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 112, SequenceNumber: seq, Timestamp: uint32(f * 1500), SSRC: 9, Marker: i == n-1}, Payload: pl}
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

// layerTap counts the frames a viewer plays per temporal layer.
type layerTap struct {
	mu     sync.Mutex
	frames map[int]uint8
}

func (l *layerTap) tap(p *media.MediaPacket) {
	if p.Kind != media.FrameVideo {
		return
	}
	var pk rtp.Packet
	if pk.Unmarshal(p.Payload) != nil || len(pk.Payload) < 5 {
		return
	}
	m, _, ok := videolayer.Find(p.Payload)
	if !ok {
		return
	}
	l.mu.Lock()
	l.frames[int(binary.BigEndian.Uint32(pk.Payload[1:5]))] = m.TID
	l.mu.Unlock()
}

func (l *layerTap) take() map[uint8]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[uint8]int{}
	for _, tid := range l.frames {
		out[tid]++
	}
	l.frames = map[int]uint8{}
	return out
}

func (l *layerTap) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.frames)
}

func newCall(t *testing.T, f *fabric, r route.Route, n int) []*participant {
	t.Helper()
	ps := make([]*participant, n)
	for i := range ps {
		s := newSigner()
		p := &participant{id: s.id(), tap: &layerTap{frames: map[int]uint8{}}}
		rr := r
		p.tr = callnet.New(callnet.Config{
			Links:   fabricLinks{f, p.id},
			Signals: signalBus{f, p.id},
			Route:   func() route.Route { return rr },
		})
		p.cm = call.NewCallManager(p.tr, s, p.id)
		p.cm.OnMedia = p.tap.tap
		p.cm.SetPeerConnector(func(_ context.Context, _ types.PeerID) error { return nil })
		f.mu.Lock()
		f.nodes[p.id] = p.tr
		f.mu.Unlock()
		ps[i] = p
	}
	return ps
}

func runCallScenario(t *testing.T, r route.Route) (*fabric, []*participant) {
	f := newFabric()
	ps := newCall(t, f, r, 5)
	// Participants 3 and 4 have no direct path to anyone: all their sessions are relayed. The
	// others have both kinds between them, so call_route decides.
	for i := range ps {
		for j := i + 1; j < len(ps); j++ {
			k := map[mux.PathKind]int{mux.PathDirect: 1, mux.PathRelay: 1}
			if i >= 3 || j >= 3 {
				k = map[mux.PathKind]int{mux.PathRelay: 1}
			}
			f.setKinds(ps[i].id, ps[j].id, k)
		}
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	coord := ps[0].id
	for i, p := range ps {
		prof := tree.PeerNodeProfile{UploadKbps: 40000, MaxRelayChildren: 2}
		if i >= 3 {
			prof = tree.PeerNodeProfile{UploadKbps: 3000, IsMobile: true, RelayOnly: true}
		}
		require.NoError(t, p.cm.JoinRoom("room-93", coord, key, prof))
	}
	t.Cleanup(func() {
		for _, p := range ps {
			_ = p.cm.LeaveRoom()
		}
	})
	require.Eventually(t, func() bool {
		for _, p := range ps {
			s, ok := p.cm.Snapshot()
			if !ok || !s.Joined || len(s.Members) != len(ps) {
				return false
			}
		}
		return true
	}, testutil.Slow(10*time.Second), 20*time.Millisecond, "the room of five forms over the fabric")
	return f, ps
}

func TestCallOfFiveTwoViaRelayLayersAndRouteRelay(t *testing.T) {
	f, ps := runCallScenario(t, route.Relay)
	streamer, strong, weak := ps[1], ps[2], ps[4] // ps[3] and ps[4] are the relayed members
	enc := &layerPump{}
	streamer.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("720p60")
	require.NoError(t, streamer.cm.StartStream("s", "screen", q))
	for _, p := range []*participant{ps[0], strong, ps[3], weak} {
		require.NoError(t, p.cm.WatchStream("s"))
	}
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, testutil.Slow(5*time.Second), 20*time.Millisecond)
	enc.run(streamer.cm)
	defer enc.halt()
	require.Eventually(t, func() bool {
		for _, p := range []*participant{ps[0], strong, ps[3], weak} {
			if p.tap.total() == 0 {
				return false
			}
		}
		return true
	}, testutil.Slow(15*time.Second), 20*time.Millisecond, "every viewer, relayed ones too, receives the stream")

	// The weak receiver's downlink: the base layer fits, the rest does not.
	sh := &shaper{kbps: 2800, queue: 200 * time.Millisecond}
	f.mu.Lock()
	for _, p := range ps {
		if p != weak {
			f.shapers[[2]types.PeerID{p.id, weak.id}] = sh
		}
	}
	f.mu.Unlock()

	var wl, sl map[uint8]int
	require.Eventually(t, func() bool {
		weak.tap.take()
		strong.tap.take()
		time.Sleep(1500 * time.Millisecond)
		wl, sl = weak.tap.take(), strong.tap.take()
		wTop := wl[1] + wl[2]
		return wl[0] > 10 && wTop*4 <= wl[0]+wTop && sl[2] > 10
	}, testutil.Slow(40*time.Second), 100*time.Millisecond,
		"the weak child settles on the base layer while the strong one keeps all layers")
	t.Logf("weak plays %v, strong plays %v; shaper passed %d dropped %d", wl, sl, sh.passed.Load(), sh.dropped.Load())

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Zero(t, f.sentOn[mux.PathDirect], "call_route=relay: not one media datagram on a direct path")
	assert.Greater(t, f.sentOn[mux.PathRelay], 100, "media travelled on relay paths")
	for _, p := range ps {
		assert.Zero(t, p.tr.Stats().DroppedRoute, "no media arrived on a path the route forbids")
	}
}

// The same call under call_route=auto: pairs that have a direct path use it, the relayed members
// get the stream through relay paths.
func TestCallOfFiveAutoUsesDirectWhereItExists(t *testing.T) {
	f, ps := runCallScenario(t, route.Auto)
	streamer := ps[1]
	enc := &layerPump{}
	streamer.cm.SetLocalEncoder(bridge.RoleScreen, enc)
	q, _ := tree.QualityByID("720p60")
	require.NoError(t, streamer.cm.StartStream("s", "screen", q))
	for _, p := range ps {
		if p != streamer {
			require.NoError(t, p.cm.WatchStream("s"))
		}
	}
	require.Eventually(t, func() bool { return enc.bps.Load() > 0 }, testutil.Slow(5*time.Second), 20*time.Millisecond)
	enc.run(streamer.cm)
	defer enc.halt()
	require.Eventually(t, func() bool {
		for _, p := range ps {
			if p != streamer && p.tap.total() == 0 {
				return false
			}
		}
		return true
	}, testutil.Slow(15*time.Second), 20*time.Millisecond, "every viewer receives the stream")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Greater(t, f.sentOn[mux.PathDirect], 20, "auto uses direct paths between the participants that have them")
	assert.Greater(t, f.sentOn[mux.PathRelay], 0, "and relay paths to the relayed ones")
}
