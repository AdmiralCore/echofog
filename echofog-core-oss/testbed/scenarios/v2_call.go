package scenarios

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/transport"
	"github.com/echofog/echofog-core/internal/v2/call/route"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/testbed/behaviour"
	"github.com/echofog/echofog-core/testbed/gate"
)

func callScenario() Scenario {
	return Scenario{Name: "v2-call", Title: "Call: media across the network, with loss", Live: liveCall, Run: runCall}
}

const (
	callDelay   = 75 * time.Millisecond // each way: a 150 ms round trip
	callMinGood = 0.90                  // the share of frames that must arrive with 5 % loss
	callMaxLate = callDelay + 60*time.Millisecond
)

// mediaLeg is one stream of media frames from a to b.
type mediaLeg struct {
	kind    transport.Kind
	every   time.Duration
	size    int
	sent    int
	got     atomic.Int64
	dups    atomic.Int64
	mu      sync.Mutex
	seen    map[uint32]bool
	latency []time.Duration
}

// callLeg sends voice (20 ms, 160 bytes) and video (33 ms, 1000 bytes) from a to b for d and
// returns what b received.
func callLeg(ctx context.Context, dir string, loss float64, seed int64, d time.Duration) (voice, video *mediaLeg, direct, other int64, err error) {
	w := NewWorld(dir)
	defer w.Close()
	b, err := w.Add(NodeSpec{Name: "callee", Server: true, Listen: true, Netem: &behaviour.NetemConfig{Loss: loss, Delay: callDelay, Seed: seed}})
	if err != nil {
		return nil, nil, 0, 0, err
	}
	a, err := w.Add(NodeSpec{Name: "caller"})
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if err := w.StartAll(ctx); err != nil {
		return nil, nil, 0, 0, err
	}
	if _, _, err := a.Dial(ctx, b); err != nil {
		return nil, nil, 0, 0, err
	}
	ta, tb := a.RT.CallTransport(), b.RT.CallTransport()
	if ta == nil || tb == nil {
		return nil, nil, 0, 0, fmt.Errorf("call/2 has no transport on the runtime")
	}
	adm := a.ID.DeviceID
	b.RT.SetCallAdmit(func(dev [32]byte) bool { return dev == [32]byte(adm) })
	a.RT.SetCallRoute(route.Direct)
	b.RT.SetCallRoute(route.Direct)

	voice = &mediaLeg{kind: transport.MediaAudio, every: 20 * time.Millisecond, size: 160, seen: map[uint32]bool{}}
	video = &mediaLeg{kind: transport.MediaVideo, every: 33 * time.Millisecond, size: 1000, seen: map[uint32]bool{}}
	for _, l := range []*mediaLeg{voice, video} {
		l := l
		tb.RegisterFastFrameHandler(l.kind, func(p []byte, _ types.PeerID) error {
			if len(p) < 12 {
				return nil
			}
			seq := binary.BigEndian.Uint32(p[:4])
			sentAt := time.Unix(0, int64(binary.BigEndian.Uint64(p[4:12])))
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.seen[seq] {
				l.dups.Add(1)
				return nil
			}
			l.seen[seq] = true
			l.got.Add(1)
			l.latency = append(l.latency, time.Since(sentAt))
			return nil
		})
	}
	// which path the video datagrams rode, seen next to the call transport
	port := b.RT.Datagrams(frame.DgramCallVideo)
	var onDirect, onOther atomic.Int64
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			dg, err := port.Receive(pctx)
			if err != nil {
				return
			}
			if dg.PathKind == mux.PathDirect {
				onDirect.Add(1)
			} else {
				onOther.Add(1)
			}
		}
	}()

	var wg sync.WaitGroup
	var sent [2]atomic.Int64
	for i, l := range []*mediaLeg{voice, video} {
		i, l := i, l
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(l.every)
			defer tick.Stop()
			deadline := time.Now().Add(d)
			buf := make([]byte, l.size)
			for time.Now().Before(deadline) {
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
				binary.BigEndian.PutUint32(buf[:4], uint32(sent[i].Load()))
				binary.BigEndian.PutUint64(buf[4:12], uint64(time.Now().UnixNano()))
				if err := ta.SendUnreliableFrame(types.PeerID(b.ID.DeviceID), l.kind, buf); err == nil {
					sent[i].Add(1)
				}
			}
		}()
	}
	wg.Wait()
	voice.sent, video.sent = int(sent[0].Load()), int(sent[1].Load())
	time.Sleep(time.Second + callDelay*2) // the last frames are still on the way
	return voice, video, onDirect.Load(), onOther.Load(), nil
}

func (l *mediaLeg) median() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.latency) == 0 {
		return 0
	}
	s := slices.Clone(l.latency)
	slices.Sort(s)
	return s[len(s)/2]
}

func (l *mediaLeg) share() float64 {
	if l.sent == 0 {
		return 0
	}
	return float64(l.got.Load()) / float64(l.sent)
}

func runCall(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-call", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "call-")
	if err != nil {
		dir = os.TempDir()
	}
	defer os.RemoveAll(dir)
	d := 8 * time.Second
	if env.Short {
		d = 4 * time.Second
	}
	fail := func(id string, err error) {
		res.Rows = append(res.Rows, newRow(id, "call", err.Error(), "media arrives", gate.Red, ""))
	}

	// no loss: every frame, none twice
	voice, video, _, _, err := callLeg(ctx, dir, 0, env.Seed, d)
	if err != nil {
		fail("v2-call/clean-path", err)
	} else {
		st := gate.Green
		if voice.share() < 1 || video.share() < 1 || voice.dups.Load()+video.dups.Load() != 0 {
			st = gate.Red
		}
		res.Rows = append(res.Rows, newRow("v2-call/clean-path", "frames arrived",
			fmt.Sprintf("voice %d of %d, video %d of %d, duplicates %d", voice.got.Load(), voice.sent, video.got.Load(), video.sent, voice.dups.Load()+video.dups.Load()),
			"all of them, once each", st, ""))
	}

	// 5 % loss, a 150 ms round trip
	voice, video, direct, other, err := callLeg(ctx, dir, 0.05, env.Seed+1, d)
	if err != nil {
		fail("v2-call/voice-loss", err)
		fail("v2-call/video-loss", err)
		fail("v2-call/direct-path", err)
	} else {
		for _, c := range []struct {
			id string
			l  *mediaLeg
			n  string
		}{{"v2-call/voice-loss", voice, "voice"}, {"v2-call/video-loss", video, "video"}} {
			st := gate.Green
			if c.l.share() < callMinGood || c.l.median() > callMaxLate {
				st = gate.Red
			}
			res.Rows = append(res.Rows, newRow(c.id, c.n+" frames",
				fmt.Sprintf("%d of %d (%.0f %%), median %s", c.l.got.Load(), c.l.sent, 100*c.l.share(), c.l.median().Round(time.Millisecond)),
				fmt.Sprintf("at least %.0f %% arrive, median at most %s", 100*callMinGood, callMaxLate), st, ""))
		}
		st := gate.Green
		if direct == 0 || other != 0 {
			st = gate.Red
		}
		res.Rows = append(res.Rows, newRow("v2-call/direct-path", "video datagrams by path",
			fmt.Sprintf("direct %d, other %d", direct, other), "all direct", st, ""))
	}
	res.NotChecked = []string{
		"call setup and signalling (offer, answer, ring, hang-up): the transport carries the media, the signalling service is not driven here",
		"the relayed route and the call across three NAT types: need the NAT matrix of the stand",
		"voice and video are synthetic frames of the size and rate of a call, not encoded media; quality is not judged",
	}
	return res
}
