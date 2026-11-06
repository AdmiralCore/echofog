package files

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// Store is where the receiver keeps the chunks it has verified. Have is the resume bitmap.
type Store interface {
	Have(i int) bool
	Count() int
	Put(i int, data []byte) error
	// Commit is called once all chunks are in.
	Commit() error
}

// Gate bounds the transfers a node runs at once (both as sender and receiver).
type Gate struct {
	mu   sync.Mutex
	max  int
	used int
}

// NewGate returns a gate for at most n transfers.
func NewGate(n int) *Gate { return &Gate{max: n} }

// Acquire takes a slot; it returns ErrBusy when all are taken. Call the returned function once to
// give the slot back (more calls are harmless).
func (g *Gate) Acquire() (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.used >= g.max {
		return func() {}, ErrBusy
	}
	g.used++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.used--
			g.mu.Unlock()
		})
	}, nil
}

// FetchStats are the observations of one fetch (tests and the UI).
type FetchStats struct {
	Requested atomic.Uint64
	Resent    atomic.Uint64
	Rejected  atomic.Uint64
	MaxWindow atomic.Int64
	Losses    atomic.Uint64
}

// FetchOptions tune a fetch.
type FetchOptions struct {
	Clock         carrier.Clock
	MinRTO        time.Duration // default 1.5 s
	InitialWindow int           // default 16
	MaxWindow     int           // default 256
	Gate          *Gate
	Stats         *FetchStats
	OnProgress    func(done, total int)
}

// Flow control of services.md §8: AIMD window of 16…256 chunks, RTO max(1.5 s, 3·srtt).
const (
	defaultInitialWindow = 16
	defaultMaxWindow     = 256
	defaultMinRTO        = 1500 * time.Millisecond
	maxRTO               = 30 * time.Second
	tickEvery            = 50 * time.Millisecond
	flushEvery           = 2 * time.Second
	// A sender that keeps sending chunks that do not verify is given up on: the same chunk
	// rejected maxRejectsPerChunk times, or maxRejects in all.
	maxRejectsPerChunk = 3
	maxRejects         = 64
)

type inflightEntry struct {
	at   time.Time
	retx bool
}

type fetchEvent struct {
	chunk  *Chunk
	cancel *Cancel
	bad    bool
	err    error
}

// Fetch downloads the file of offer over st (a files/2 stream opened to the sender) into store.
// It returns nil when every chunk is in, verified and committed. The store keeps what was verified
// when the fetch ends early, so the same call resumes it; Waiting(err) says when trying again later
// is what to do (the sender is not there). Fetch owns the stream.
func Fetch(ctx context.Context, st *mux.Stream, offer Offer, store Store, opt FetchOptions) (err error) {
	if err := offer.Validate(); err != nil {
		return err
	}
	n := offer.Chunks()
	clk := opt.Clock
	if clk == nil {
		clk = sysClock{}
	}
	if opt.MinRTO <= 0 {
		opt.MinRTO = defaultMinRTO
	}
	if opt.InitialWindow <= 0 {
		opt.InitialWindow = defaultInitialWindow
	}
	if opt.MaxWindow <= 0 {
		opt.MaxWindow = defaultMaxWindow
	}
	if opt.MaxWindow < opt.InitialWindow {
		opt.MaxWindow = opt.InitialWindow
	}
	stats := opt.Stats
	if stats == nil {
		stats = &FetchStats{}
	}
	if opt.Gate != nil {
		release, gerr := opt.Gate.Acquire()
		if gerr != nil {
			_ = st.Close()
			return gerr
		}
		defer release()
	}
	flusher, _ := store.(interface{ Flush() error })
	defer func() {
		if flusher != nil {
			_ = flusher.Flush()
		}
	}()
	if store.Count() >= n {
		_ = st.Close()
		return store.Commit()
	}

	st.SetLimiter(filesLimiter())
	events := make(chan fetchEvent, 64)
	quit := make(chan struct{})
	go readFrames(st, events, quit)
	finished := false
	defer func() {
		close(quit)
		if !finished {
			st.Reset(frame.CodeGoingAway)
		}
	}()

	tick := make(chan struct{}, 1)
	var tm carrier.Timer
	arm := func() {
		tm = clk.AfterFunc(tickEvery, func() {
			select {
			case tick <- struct{}{}:
			default:
			}
		})
	}
	arm()
	defer func() { tm.Stop() }()

	f := &fetcher{
		offer: offer, n: n, store: store, opt: opt, stats: stats, clk: clk, st: st,
		asked:    make([]byte, (n+7)/8),
		inflight: map[int]inflightEntry{},
		rejects:  map[int]int{},
		cwnd:     float64(opt.InitialWindow),
		ssthresh: float64(opt.MaxWindow),
		minWin:   float64(min(defaultInitialWindow, opt.InitialWindow)),
		rto:      opt.MinRTO * 2,
	}
	f.lastFlush = clk.Now()

	for {
		if store.Count() >= n {
			_ = st.WriteFrame(TypeDone, 0, Done{FID: offer.FID}.Append(nil))
			if err := store.Commit(); err != nil {
				return err
			}
			finished = true
			_ = st.Close()
			return nil
		}
		if err := f.pump(); err != nil {
			return f.streamErr(ctx, err)
		}
		select {
		case <-ctx.Done():
			_ = st.WriteFrame(TypeCancel, 0, Cancel{FID: offer.FID, Reason: ReasonCancelled}.Append(nil))
			return ctx.Err()
		case ev := <-events:
			if err := f.handle(ev); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		case <-tick:
			f.onTick()
			if flusher != nil && clk.Now().Sub(f.lastFlush) >= flushEvery {
				f.lastFlush = clk.Now()
				_ = flusher.Flush()
			}
			arm()
		}
	}
}

// readFrames turns the frames of the stream into events until it ends.
func readFrames(st *mux.Stream, out chan<- fetchEvent, quit <-chan struct{}) {
	send := func(ev fetchEvent) bool {
		select {
		case out <- ev:
			return true
		case <-quit:
			return false
		}
	}
	for {
		fr, err := st.ReadFrame()
		if err != nil {
			send(fetchEvent{err: err})
			return
		}
		switch fr.Type {
		case TypeChunk:
			c, err := ParseChunk(fr.Payload)
			if err != nil {
				if !send(fetchEvent{bad: true}) {
					return
				}
				continue
			}
			c.Data = append([]byte(nil), c.Data...) // the frame buffer is reused
			if !send(fetchEvent{chunk: &c}) {
				return
			}
		case TypeCancel:
			c, err := ParseCancel(fr.Payload)
			if err != nil {
				send(fetchEvent{bad: true})
				continue
			}
			send(fetchEvent{cancel: &c})
			return
		}
	}
}

type fetcher struct {
	offer Offer
	n     int
	store Store
	opt   FetchOptions
	stats *FetchStats
	clk   carrier.Clock
	st    *mux.Stream

	asked    []byte
	inflight map[int]inflightEntry
	retry    []int
	cursor   int
	rejects  map[int]int
	badTotal int

	cwnd, ssthresh, minWin float64
	srtt, minRTT           time.Duration
	haveRTT                bool
	rto                    time.Duration
	lastLoss               time.Time
	lastFlush              time.Time
}

func (f *fetcher) wasAsked(i int) bool { return f.asked[i>>3]&(1<<(uint(i)&7)) != 0 }

func (f *fetcher) nextMissing() int {
	for len(f.retry) > 0 {
		i := f.retry[0]
		f.retry = f.retry[1:]
		if _, busy := f.inflight[i]; !busy && !f.store.Have(i) {
			return i
		}
	}
	for f.cursor < f.n {
		i := f.cursor
		f.cursor++
		if _, busy := f.inflight[i]; !busy && !f.store.Have(i) {
			return i
		}
	}
	return -1
}

// pump asks for chunks while the window has room.
func (f *fetcher) pump() error {
	for len(f.inflight) < int(f.cwnd) {
		room := min(int(f.cwnd)-len(f.inflight), MaxWantChunks)
		var idx []int
		for len(idx) < room {
			i := f.nextMissing()
			if i < 0 {
				break
			}
			idx = append(idx, i)
		}
		if len(idx) == 0 {
			if len(f.inflight) == 0 && len(f.retry) == 0 && f.cursor >= f.n && f.store.Count() < f.n {
				f.cursor = 0 // nothing is on its way and the file is not complete: look it over again
				continue
			}
			return nil
		}
		now := f.clk.Now()
		if err := f.st.WriteFrame(TypeWant, 0, Want{FID: f.offer.FID, Ranges: rangesOf(idx)}.Append(nil)); err != nil {
			return err
		}
		for _, i := range idx {
			e := inflightEntry{at: now}
			if f.wasAsked(i) {
				e.retx = true
				f.stats.Resent.Add(1)
			}
			f.asked[i>>3] |= 1 << (uint(i) & 7)
			f.inflight[i] = e
		}
		f.stats.Requested.Add(uint64(len(idx)))
		if w := int64(f.cwnd); w > f.stats.MaxWindow.Load() {
			f.stats.MaxWindow.Store(w)
		}
	}
	return nil
}

// rangesOf turns indices into ranges of consecutive ones.
func rangesOf(idx []int) []Range {
	var out []Range
	for _, i := range idx {
		if k := len(out) - 1; k >= 0 && out[k].Start+uint64(out[k].Count) == uint64(i) {
			out[k].Count++
			continue
		}
		out = append(out, Range{Start: uint64(i), Count: 1})
	}
	return out
}

func (f *fetcher) streamErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

func (f *fetcher) handle(ev fetchEvent) error {
	switch {
	case ev.err != nil:
		return fmt.Errorf("%w: %v", ErrUnavailable, ev.err)
	case ev.cancel != nil:
		switch ev.cancel.Reason {
		case ReasonUnavailable:
			return ErrUnavailable
		case ReasonDeclined:
			return ErrDeclined
		}
		return ErrCancelled
	case ev.bad:
		return f.reject(-1)
	case ev.chunk != nil:
		return f.chunk(ev.chunk)
	}
	return nil
}

// reject counts a chunk that did not verify; idx < 0 is a frame that did not parse or that no
// request asked for.
func (f *fetcher) reject(idx int) error {
	f.stats.Rejected.Add(1)
	f.badTotal++
	if idx >= 0 {
		f.rejects[idx]++
		delete(f.inflight, idx)
		f.retry = append(f.retry, idx)
		if f.rejects[idx] >= maxRejectsPerChunk {
			return ErrCorrupt
		}
	}
	if f.badTotal >= maxRejects {
		return ErrCorrupt
	}
	return nil
}

func (f *fetcher) chunk(c *Chunk) error {
	if c.FID != f.offer.FID || c.Index >= uint64(f.n) {
		return f.reject(-1)
	}
	i := int(c.Index)
	if f.store.Have(i) {
		delete(f.inflight, i) // a second copy of a chunk that was asked for again
		return nil
	}
	if !f.wasAsked(i) {
		return f.reject(-1)
	}
	if len(c.Data) != chunkLen(int64(f.offer.Size), i) {
		return f.reject(i)
	}
	proof := make([]filexfer.Hash, len(c.Proof))
	for j := range c.Proof {
		proof[j] = c.Proof[j]
	}
	if !filexfer.VerifyProof(filexfer.Hash(f.offer.Root), i, f.n, filexfer.LeafHash(c.Data), proof) {
		return f.reject(i)
	}
	if err := f.store.Put(i, c.Data); err != nil {
		return err
	}
	if e, ok := f.inflight[i]; ok {
		delete(f.inflight, i)
		f.acked(f.clk.Now().Sub(e.at), e.retx)
	}
	if f.opt.OnProgress != nil {
		f.opt.OnProgress(f.store.Count(), f.n)
	}
	return nil
}

// acked grows the window: while chunks come back promptly (they do not just queue behind each
// other) one chunk per ack in slow start, one per window afterwards.
func (f *fetcher) acked(rtt time.Duration, retx bool) {
	if !retx { // Karn: a retransmitted chunk says nothing about the round trip
		if f.minRTT == 0 || rtt < f.minRTT {
			f.minRTT = rtt
		}
		if !f.haveRTT {
			f.srtt, f.haveRTT = rtt, true
		} else {
			f.srtt = (7*f.srtt + rtt) / 8
		}
		f.rto = max(f.opt.MinRTO, 3*f.srtt)
	}
	if f.srtt < 2*f.minRTT+250*time.Millisecond {
		if f.cwnd < f.ssthresh {
			f.cwnd++
		} else {
			f.cwnd += 1 / f.cwnd
		}
		f.cwnd = min(f.cwnd, float64(f.opt.MaxWindow))
	}
}

// onTick finds the chunks that did not come back within the RTO and halves the window once per
// round of losses.
func (f *fetcher) onTick() {
	now := f.clk.Now()
	lost := false
	for i, e := range f.inflight {
		if now.Sub(e.at) > f.rto {
			delete(f.inflight, i)
			f.retry = append(f.retry, i)
			lost = true
		}
	}
	if lost && now.Sub(f.lastLoss) > max(f.srtt, 200*time.Millisecond) {
		f.ssthresh = max(f.cwnd/2, f.minWin)
		f.cwnd = f.ssthresh
		f.lastLoss = now
		f.rto = min(f.rto*2, maxRTO) // back off while chunks keep getting lost
		f.stats.Losses.Add(1)
	}
}

// Waiting reports whether a failed fetch can simply be tried again later: the sender is not
// there now, the store keeps what was verified.
func Waiting(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBusy)
}
