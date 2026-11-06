package files

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

func TestTransferSizes(t *testing.T) {
	for _, n := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3 * ChunkSize, 5<<20 + 123} {
		l := newLab(t, ServerConfig{})
		var gotDone atomic.Int32
		l.srv = NewServer(ServerConfig{Clock: l.clk, OnDone: func(f [16]byte, node [32]byte) {
			if node == nodeB.node() {
				gotDone.Add(1)
			}
		}})
		data := content(n, int64(n))
		o := l.share(data, nodeB)
		k := l.dial(nodeB)
		st := newMemStore(o)
		if err := l.fetch(k, o, st, fast()); err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if !bytes.Equal(st.bytes(), data) || !st.done {
			t.Fatalf("%d bytes: content differs or not committed", n)
		}
		deadline := time.Now().Add(2 * time.Second)
		for gotDone.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if gotDone.Load() != 1 {
			t.Fatalf("%d bytes: OnDone called %d times", n, gotDone.Load())
		}
	}
}

// askRaw sends one WANT for fid and returns the first answer frame.
func askRaw(t *testing.T, l *lab, k *link, f [16]byte) frame.Frame {
	t.Helper()
	st := l.open(k)
	if err := st.WriteFrame(TypeWant, 0, Want{FID: f, Ranges: []Range{{0, 1}}}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	type res struct {
		f   frame.Frame
		err error
	}
	ch := make(chan res, 1)
	go func() { fr, err := st.ReadFrame(); ch <- res{fr, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadFrame: %v", r.err)
		}
		return r.f
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	return frame.Frame{}
}

func TestOnlyRecipientsAreServed(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(3*ChunkSize, 1), nodeB)
	kB, kC := l.dial(nodeB), l.dial(nodeC)

	// a node that is not a recipient is refused, and so is a file nobody has: the same answer
	refusedKnown := askRaw(t, l, kC, o.FID)
	unknown := fid(200)
	refusedUnknown := askRaw(t, l, kC, unknown)
	for name, f := range map[string]frame.Frame{"known file, not a recipient": refusedKnown, "unknown file": refusedUnknown} {
		if f.Type != TypeCancel {
			t.Fatalf("%s: frame type %#x", name, f.Type)
		}
		c, err := ParseCancel(f.Payload)
		if err != nil || c.Reason != ReasonUnavailable {
			t.Fatalf("%s: %+v %v", name, c, err)
		}
	}
	a, _ := ParseCancel(refusedKnown.Payload)
	b, _ := ParseCancel(refusedUnknown.Payload)
	if a.FID != o.FID || b.FID != unknown {
		t.Fatal("CANCEL must echo the asked fid and nothing else")
	}
	if len(refusedKnown.Payload) != len(refusedUnknown.Payload) {
		t.Fatal("the two refusals differ in size: an oracle for the file's existence")
	}
	if err := l.fetch(kC, o, newMemStore(o), fast()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fetch by a stranger: %v", err)
	}
	if l.srv.Stats().ChunksSent.Load() != 0 {
		t.Fatal("a stranger got chunks")
	}
	// the recipient is served, over its own session
	st := newMemStore(o)
	if err := l.fetch(kB, o, st, fast()); err != nil {
		t.Fatalf("recipient: %v", err)
	}
	// the group case: every member listed is served, nobody else
	o2 := l.share(content(ChunkSize, 2), nodeB, nodeC)
	for _, k := range []*link{kB, kC} {
		if err := l.fetch(k, o2, newMemStore(o2), fast()); err != nil {
			t.Fatalf("member: %v", err)
		}
	}
}

func TestRemovedFileIsUnavailable(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(ChunkSize, 3), nodeB)
	l.srv.Remove(o.FID)
	if err := l.fetch(l.dial(nodeB), o, newMemStore(o), fast()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestCorruptChunksAreRejected(t *testing.T) {
	l := newLab(t, ServerConfig{})
	data := content(8*ChunkSize, 4)
	sh, err := PrepareShare(bytes.NewReader(data), int64(len(data)), "x.bin", "", nodeA.node(), [][32]byte{nodeB.node()})
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), data...)
	bad[2*ChunkSize+5] ^= 0xff // chunk 2 differs from what the tree was built from
	sh.Source = bytes.NewReader(bad)
	if err := l.srv.Add(sh); err != nil {
		t.Fatal(err)
	}
	st := newMemStore(sh.Offer)
	var stats FetchStats
	opt := fast()
	opt.Stats = &stats
	err = l.fetch(l.dial(nodeB), sh.Offer, st, opt)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
	if st.Have(2) {
		t.Fatal("a chunk that does not verify was stored")
	}
	if stats.Rejected.Load() == 0 {
		t.Fatal("rejections not counted")
	}
	if st.done {
		t.Fatal("committed a corrupt file")
	}
}

// A sender that answers for the wrong file (a root that is not the offered one) gets nothing stored.
func TestWrongRootStoresNothing(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(4*ChunkSize, 5), nodeB)
	o.Root[0] ^= 1
	st := newMemStore(o)
	if err := l.fetch(l.dial(nodeB), o, st, fast()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
	if st.Count() != 0 {
		t.Fatalf("%d chunks stored", st.Count())
	}
}

func TestResumeFetchesOnlyWhatIsMissing(t *testing.T) {
	l := newLab(t, ServerConfig{})
	const chunks = 60
	data := content(chunks*ChunkSize, 6)
	o := l.share(data, nodeB)
	k := l.dial(nodeB)
	st := newMemStore(o)

	ctx, cancel := context.WithCancel(l.ctx)
	opt := fast()
	opt.OnProgress = func(done, total int) {
		if done >= 20 {
			cancel()
		}
	}
	err := Fetch(ctx, l.open(k), o, st, opt)
	if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrCancelled) {
		t.Fatalf("first run: %v", err)
	}
	have := st.Count()
	if have < 20 || have >= chunks {
		t.Fatalf("after the cancel the store holds %d of %d", have, chunks)
	}
	var stats FetchStats
	opt = fast()
	opt.Stats = &stats
	if err := Fetch(l.ctx, l.open(k), o, st, opt); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if want := uint64(chunks - have); stats.Requested.Load() != want {
		t.Fatalf("second run asked for %d chunks, %d were missing", stats.Requested.Load(), want)
	}
	if !bytes.Equal(st.bytes(), data) {
		t.Fatal("content differs after the resume")
	}
	// a chunk is stored once even when the sender sent it twice
	seen := map[int]int{}
	for _, i := range st.put {
		seen[i]++
	}
	for i, n := range seen {
		if n > 1 {
			t.Fatalf("chunk %d stored %d times", i, n)
		}
	}
}

func TestLossIsRecoveredWithoutRestartingTheFile(t *testing.T) {
	l := newLab(t, ServerConfig{})
	const chunks = 400
	data := content(chunks*ChunkSize, 7)
	o := l.share(data, nodeB)
	var mu sync.Mutex
	dropped := map[uint64]bool{}
	l.srv.SetChunkFilter(func(_ [16]byte, i uint64) bool {
		mu.Lock()
		defer mu.Unlock()
		if i%50 == 7 && !dropped[i] { // 2 % of the chunks, each lost once
			dropped[i] = true
			return true
		}
		return false
	})
	st := newMemStore(o)
	var stats FetchStats
	opt := fast()
	opt.Stats = &stats
	if err := l.fetch(l.dial(nodeB), o, st, opt); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(st.bytes(), data) {
		t.Fatal("content differs")
	}
	if stats.Resent.Load() < uint64(len(dropped)) || stats.Losses.Load() == 0 {
		t.Fatalf("resent %d, losses %d, dropped %d", stats.Resent.Load(), stats.Losses.Load(), len(dropped))
	}
	if sent := l.srv.Stats().ChunksSent.Load(); sent > chunks+uint64(len(dropped))*3 {
		t.Fatalf("server sent %d chunks for %d: the file was not resumed from the bitmap", sent, chunks)
	}
}

func TestWindowGrowsToTheMaximum(t *testing.T) {
	l := newLab(t, ServerConfig{})
	const chunks = 900
	o := l.share(content(chunks*ChunkSize, 8), nodeB)
	var stats FetchStats
	opt := fast()
	opt.Stats = &stats
	if err := l.fetch(l.dial(nodeB), o, newMemStore(o), opt); err != nil {
		t.Fatal(err)
	}
	if w := stats.MaxWindow.Load(); w < 128 || w > 256 {
		t.Fatalf("largest window %d, want 128…256 (AIMD 16–256)", w)
	}
}

func TestWindowBoundsAreHonoured(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(300*ChunkSize, 9), nodeB)
	var stats FetchStats
	opt := fast()
	opt.Stats = &stats
	opt.MaxWindow = 20
	opt.InitialWindow = 4
	if err := l.fetch(l.dial(nodeB), o, newMemStore(o), opt); err != nil {
		t.Fatal(err)
	}
	if w := stats.MaxWindow.Load(); w > 20 {
		t.Fatalf("window %d over the configured maximum", w)
	}
}

// The retransmission timer runs on the injected clock (AD13): no real time passes, and a chunk
// that never arrived is asked for again once the clock has moved by the RTO.
func TestRetransmissionUsesTheInjectedClock(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(10*ChunkSize, 10), nodeB)
	var once sync.Once
	l.srv.SetChunkFilter(func(_ [16]byte, i uint64) bool {
		drop := false
		if i == 3 {
			once.Do(func() { drop = true })
		}
		return drop
	})
	var stats FetchStats
	clk := newTestClock()
	opt := FetchOptions{Clock: clk, Stats: &stats} // default MinRTO: 1.5 s of the injected clock
	st := newMemStore(o)
	done := make(chan error, 1)
	k := l.dial(nodeB)
	go func() { done <- Fetch(l.ctx, l.open(k), o, st, opt) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if stats.Resent.Load() == 0 {
				t.Fatal("finished without a retransmission: the drop did not happen")
			}
			return
		case <-time.After(20 * time.Millisecond):
			clk.Advance(500 * time.Millisecond)
			if time.Now().After(deadline) {
				t.Fatal("stuck: the timer does not follow the injected clock")
			}
		}
	}
}

func TestTransfersAreLimitedPerPeerAndPerNode(t *testing.T) {
	l := newLab(t, ServerConfig{Limits: Limits{PerPeer: 2, Total: 3}})
	o := l.share(content(200*ChunkSize, 11), nodeB, nodeC)
	kB, kC := l.dial(nodeB), l.dial(nodeC)
	// a stream counts when it asks for chunks and ends when it is closed
	open := func(k *link) *streamHandle {
		st := l.open(k)
		if err := st.WriteFrame(TypeWant, 0, Want{FID: o.FID, Ranges: []Range{{0, 1}}}.Append(nil)); err != nil {
			t.Fatal(err)
		}
		return &streamHandle{st: st}
	}
	first := func(h *streamHandle) frame.Frame {
		f, err := h.st.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	b1, b2 := open(kB), open(kB)
	if first(b1).Type != TypeChunk || first(b2).Type != TypeChunk {
		t.Fatal("the first two transfers must be served")
	}
	b3 := open(kB)
	if f := first(b3); f.Type != TypeCancel {
		t.Fatalf("third transfer of one peer: frame %#x, want CANCEL", f.Type)
	}
	c1 := open(kC)
	if first(c1).Type != TypeChunk {
		t.Fatal("another peer must still get its transfer (3 in all)")
	}
	c2 := open(kC)
	if f := first(c2); f.Type != TypeCancel {
		t.Fatalf("fourth transfer of the node: frame %#x, want CANCEL", f.Type)
	}
	// closing one frees its slot
	_ = b1.st.Close()
	deadline := time.Now().Add(3 * time.Second)
	for l.srv.Stats().Active.Load() > 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	b4 := open(kB)
	if f := first(b4); f.Type != TypeChunk {
		t.Fatalf("after a slot was freed: frame %#x", f.Type)
	}
}

type streamHandle struct {
	st interface {
		ReadFrame() (frame.Frame, error)
		Close() error
	}
}

func TestIdleStreamFreesItsSlot(t *testing.T) {
	l := newLab(t, ServerConfig{Limits: Limits{PerPeer: 1, IdleTimeout: time.Minute}})
	o := l.share(content(10*ChunkSize, 12), nodeB)
	k := l.dial(nodeB)
	st := l.open(k)
	if err := st.WriteFrame(TypeWant, 0, Want{FID: o.FID, Ranges: []Range{{0, 1}}}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	if f, err := st.ReadFrame(); err != nil || f.Type != TypeChunk {
		t.Fatalf("%v %v", f.Type, err)
	}
	if l.srv.Stats().Active.Load() != 1 {
		t.Fatalf("active = %d", l.srv.Stats().Active.Load())
	}
	// the receiver goes silent: after the idle timeout (of the injected clock) the stream is dropped
	deadline := time.Now().Add(10 * time.Second)
	for l.srv.Stats().Active.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a silent receiver still holds its slot")
		}
		l.clk.Advance(30 * time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	// the slot is reusable
	st2 := l.open(k)
	_ = st2.WriteFrame(TypeWant, 0, Want{FID: o.FID, Ranges: []Range{{0, 1}}}.Append(nil))
	if f, err := st2.ReadFrame(); err != nil || f.Type != TypeChunk {
		t.Fatalf("after the idle drop: %v %v", f.Type, err)
	}
}

func TestMalformedFramesEndTheStream(t *testing.T) {
	l := newLab(t, ServerConfig{})
	l.share(content(ChunkSize, 13), nodeB)
	k := l.dial(nodeB)
	for name, w := range map[string][]byte{"garbage": {0xff, 0xff, 0xff}, "257 chunks": Want{FID: fid(1), Ranges: []Range{{0, 257}}}.Append(nil)} {
		st := l.open(k)
		if err := st.WriteFrame(TypeWant, 0, w); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReadFrame(); err == nil {
			t.Fatalf("%s: the server answered a malformed WANT", name)
		}
	}
	if l.srv.Stats().Violations.Load() < 2 {
		t.Fatalf("violations = %d", l.srv.Stats().Violations.Load())
	}
	// frames only the receiver sends are violations coming from the other side
	st := l.open(k)
	if err := st.WriteFrame(TypeChunk, 0, Chunk{FID: fid(1)}.Append(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadFrame(); err == nil {
		t.Fatal("the server accepted a CHUNK")
	}
}

func TestShareQuotas(t *testing.T) {
	l := newLab(t, ServerConfig{Limits: Limits{Files: 2, TreeBytes: 64 * 1024, MaxFileSize: 100 * ChunkSize}})
	mk := func(n int, seed int64) Share {
		sh, err := PrepareShare(bytes.NewReader(content(n, seed)), int64(n), "f.bin", "", nodeA.node(), [][32]byte{nodeB.node()})
		if err != nil {
			t.Fatal(err)
		}
		return sh
	}
	a, b := mk(ChunkSize, 1), mk(ChunkSize, 2)
	if err := l.srv.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := l.srv.Add(b); err != nil {
		t.Fatal(err)
	}
	if err := l.srv.Add(a); err == nil {
		t.Fatal("the same file added twice")
	}
	if err := l.srv.Add(mk(ChunkSize, 3)); !errors.Is(err, ErrQuota) {
		t.Fatalf("third file: %v", err)
	}
	l.srv.Remove(a.Offer.FID)
	if err := l.srv.Add(mk(ChunkSize, 3)); err != nil {
		t.Fatalf("after a removal: %v", err)
	}
	if err := l.srv.Add(mk(101*ChunkSize, 4)); !errors.Is(err, ErrQuota) {
		t.Fatalf("file over the size limit: %v", err)
	}
	// the memory the trees take is bounded: 64 bytes per chunk
	l2 := newLab(t, ServerConfig{Limits: Limits{TreeBytes: 64 * 100}})
	if err := l2.srv.Add(mk(60*ChunkSize, 5)); err != nil {
		t.Fatal(err)
	}
	if err := l2.srv.Add(mk(60*ChunkSize, 6)); !errors.Is(err, ErrQuota) {
		t.Fatalf("trees over the memory limit: %v", err)
	}
	// a share whose offer does not match its tree is refused
	bad := mk(ChunkSize, 7)
	bad.Offer.Root[0] ^= 1
	if err := l.srv.Add(bad); !errors.Is(err, ErrOffer) {
		t.Fatalf("root mismatch: %v", err)
	}
}

func TestGate(t *testing.T) {
	g := NewGate(2)
	r1, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquire(); !errors.Is(err, ErrBusy) {
		t.Fatalf("third: %v", err)
	}
	r1()
	r1() // a second release is harmless
	if _, err := g.Acquire(); err != nil {
		t.Fatalf("after a release: %v", err)
	}
	r2()
}

func TestFetchHonoursTheGate(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(ChunkSize, 14), nodeB)
	g := NewGate(1)
	hold, _ := g.Acquire()
	opt := fast()
	opt.Gate = g
	if err := l.fetch(l.dial(nodeB), o, newMemStore(o), opt); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	hold()
	if err := l.fetch(l.dial(nodeB), o, newMemStore(o), opt); err != nil {
		t.Fatalf("after the release: %v", err)
	}
}

// What a member sees when the author is offline and nobody else has the file (T8.3 question of
// ТЕО): not a protocol error but a state: the fetch ends with ErrUnavailable, the verified chunks
// stay in the store, and the same fetch resumes the moment a holder is back.
func TestOfflineAuthorIsAStateNotAFailure(t *testing.T) {
	l := newLab(t, ServerConfig{})
	const chunks = 200
	data := content(chunks*ChunkSize, 15)
	o := l.share(data, nodeB)
	k := l.dial(nodeB)
	st := newMemStore(o)
	opt := fast()
	opt.OnProgress = func(done, total int) {
		if done == 30 {
			_ = k.server.Close() // the author goes offline mid-transfer
		}
	}
	err := Fetch(l.ctx, l.open(k), o, st, opt)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("author offline: err = %v, want ErrUnavailable", err)
	}
	if !Waiting(err) {
		t.Fatal("Waiting(err) must say the file can be fetched later")
	}
	have := st.Count()
	if have < 30 || have >= chunks {
		t.Fatalf("store holds %d chunks", have)
	}
	// the author is back (a new session): the download goes on from the bitmap
	k2 := l.dial(nodeB)
	var stats FetchStats
	opt = fast()
	opt.Stats = &stats
	if err := Fetch(l.ctx, l.open(k2), o, st, opt); err != nil {
		t.Fatalf("after the author returned: %v", err)
	}
	if stats.Requested.Load() != uint64(chunks-have) {
		t.Fatalf("asked for %d, missing were %d", stats.Requested.Load(), chunks-have)
	}
	if !bytes.Equal(st.bytes(), data) {
		t.Fatal("content differs")
	}
	// a refusal that is final is not "waiting"
	if Waiting(ErrCorrupt) || Waiting(ErrDeclined) || Waiting(nil) {
		t.Fatal("Waiting is true for a final failure")
	}
}

// Fetches and servers that end leave no goroutine behind.
func TestNoGoroutinesLeak(t *testing.T) {
	l := newLab(t, ServerConfig{})
	o := l.share(content(40*ChunkSize, 20), nodeB)
	k, kC := l.dial(nodeB), l.dial(nodeC)
	run := func() {
		_ = l.fetch(k, o, newMemStore(o), fast())
		ctx, cancel := context.WithCancel(l.ctx)
		opt := fast()
		opt.OnProgress = func(done, _ int) {
			if done > 3 {
				cancel()
			}
		}
		_ = Fetch(ctx, l.open(k), o, newMemStore(o), opt)
		_ = l.fetch(kC, o, newMemStore(o), fast()) // refused
	}
	run() // warm up
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		run()
	}
	var after int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		after = runtime.NumGoroutine()
		if after <= before+6 {
			return
		}
	}
	t.Fatalf("goroutines: %d before, %d after 5 rounds", before, after)
}
