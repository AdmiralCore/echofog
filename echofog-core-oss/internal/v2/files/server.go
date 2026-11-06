package files

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
)

// Peer is the authenticated session that opened a stream. It comes from the session, never from
// a frame (services.md §8: the sender serves only recipients named by the offer).
type Peer struct {
	Node   [32]byte
	Device [32]byte
}

// Limits are the holder's limits. Zero fields take the defaults. The size of a file is not limited
// by the protocol (up to MaxOfferSize); MaxFileSize and TreeBytes bound what one node holds in
// memory for it: the Merkle tree takes 64 bytes per 64 KiB chunk.
type Limits struct {
	// PerPeer and Total bound the transfers (streams that asked for chunks) running at once.
	PerPeer int
	Total   int
	// Files and TreeBytes bound what the holder keeps shared; MaxFileSize bounds one file.
	Files       int
	TreeBytes   int64
	MaxFileSize int64
	// WantTimeout is how long a receiver may take to accept one chunk before its stream is
	// reset; IdleTimeout is how long a stream may sit without a frame.
	WantTimeout time.Duration
	IdleTimeout time.Duration
}

// Defaults of Limits.
const (
	DefaultPerPeer     = 4
	DefaultTotal       = 32
	DefaultFiles       = 256
	DefaultTreeBytes   = 256 << 20
	DefaultMaxFileSize = int64(64) << 30
	DefaultWantTimeout = time.Minute
	DefaultIdleTimeout = 2 * time.Minute

	treeBytesPerChunk = 64
	maxRecipients     = 4096
)

func (l Limits) fill() Limits {
	if l.PerPeer <= 0 {
		l.PerPeer = DefaultPerPeer
	}
	if l.Total <= 0 {
		l.Total = DefaultTotal
	}
	if l.Files <= 0 {
		l.Files = DefaultFiles
	}
	if l.TreeBytes <= 0 {
		l.TreeBytes = DefaultTreeBytes
	}
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultMaxFileSize
	}
	if l.WantTimeout <= 0 {
		l.WantTimeout = DefaultWantTimeout
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = DefaultIdleTimeout
	}
	return l
}

// ServerConfig configures a Server.
type ServerConfig struct {
	// Clock is the injected clock (AD13). Nil means the system clock.
	Clock  carrier.Clock
	Limits Limits
	// OnDone is called when a recipient confirmed it got the whole file.
	OnDone func(fid [16]byte, node [32]byte)
}

// Share is a file the holder offers: where the bytes are, the tree over them, and who may fetch it.
type Share struct {
	Offer      Offer
	Source     io.ReaderAt
	Tree       *filexfer.Tree
	Recipients [][32]byte
}

// Stats counts what the server did.
type Stats struct {
	ChunksSent atomic.Uint64
	Refused    atomic.Uint64
	Violations atomic.Uint64
	Done       atomic.Uint64
	Active     atomic.Int64
}

type sysClock struct{}

func (sysClock) Now() time.Time                                    { return time.Now() }
func (sysClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return time.AfterFunc(d, f) }

type shared struct {
	Share
	recipients map[[32]byte]struct{}
	cost       int64
	gone       atomic.Bool
}

// Server is the sender side of files/2 on one node. It is safe for concurrent use.
type Server struct {
	cfg ServerConfig
	clk carrier.Clock
	lim Limits

	mu        sync.Mutex
	shares    map[[16]byte]*shared
	treeBytes int64
	perPeer   map[[32]byte]int
	active    int

	filter atomic.Pointer[func(fid [16]byte, index uint64) bool]
	stats  Stats
}

// NewServer returns a server.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Clock == nil {
		cfg.Clock = sysClock{}
	}
	return &Server{cfg: cfg, clk: cfg.Clock, lim: cfg.Limits.fill(),
		shares: map[[16]byte]*shared{}, perPeer: map[[32]byte]int{}}
}

// PrepareShare hashes size bytes of src and builds the offer and the share for the recipients.
func PrepareShare(src io.ReaderAt, size int64, name, mime string, sender [32]byte, recipients [][32]byte) (Share, error) {
	if size < 0 || src == nil {
		return Share{}, offerErr("size")
	}
	var fid [16]byte
	if _, err := rand.Read(fid[:]); err != nil {
		return Share{}, err
	}
	tree, err := filexfer.HashFile(src, size)
	if err != nil {
		return Share{}, err
	}
	o := Offer{FID: fid, Size: uint64(size), Root: [32]byte(tree.Root()), ChunkSize: ChunkSize, Name: name, Mime: mime, Sender: sender}
	if err := o.Validate(); err != nil {
		return Share{}, err
	}
	return Share{Offer: o, Source: src, Tree: tree, Recipients: append([][32]byte(nil), recipients...)}, nil
}

// Add starts sharing a file. It refuses a share that breaks the holder's quotas (ErrQuota) or whose
// offer does not match its tree (ErrOffer).
func (s *Server) Add(sh Share) error {
	if err := sh.Offer.Validate(); err != nil {
		return err
	}
	if sh.Source == nil || sh.Tree == nil || sh.Tree.Leaves() != sh.Offer.Chunks() || [32]byte(sh.Tree.Root()) != sh.Offer.Root {
		return offerErr("tree does not match the offer")
	}
	if len(sh.Recipients) == 0 || len(sh.Recipients) > maxRecipients {
		return offerErr("recipients")
	}
	if int64(sh.Offer.Size) > s.lim.MaxFileSize {
		return fmt.Errorf("%w: file of %d bytes", ErrQuota, sh.Offer.Size)
	}
	rs := make(map[[32]byte]struct{}, len(sh.Recipients))
	for _, r := range sh.Recipients {
		rs[r] = struct{}{}
	}
	cost := int64(sh.Tree.Leaves()) * treeBytesPerChunk
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.shares[sh.Offer.FID]; dup {
		return offerErr("already shared")
	}
	if len(s.shares) >= s.lim.Files {
		return fmt.Errorf("%w: %d files", ErrQuota, len(s.shares))
	}
	if s.treeBytes+cost > s.lim.TreeBytes {
		return fmt.Errorf("%w: hash trees", ErrQuota)
	}
	s.shares[sh.Offer.FID] = &shared{Share: sh, recipients: rs, cost: cost}
	s.treeBytes += cost
	return nil
}

// Remove stops sharing a file; streams serving it end.
func (s *Server) Remove(fid [16]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sh, ok := s.shares[fid]; ok {
		sh.gone.Store(true)
		s.treeBytes -= sh.cost
		delete(s.shares, fid)
	}
}

// Stats returns the counters.
func (s *Server) Stats() *Stats { return &s.stats }

// SetChunkFilter installs a test hook that drops chunks the server was about to send (loss).
func (s *Server) SetChunkFilter(f func(fid [16]byte, index uint64) bool) {
	if f == nil {
		s.filter.Store(nil)
		return
	}
	s.filter.Store(&f)
}

// lookup returns the share a peer may fetch. A file nobody has and a file the peer may not fetch
// give the same nil: the answer is no oracle for the existence of a file (AD20).
func (s *Server) lookup(fid [16]byte, node [32]byte) *shared {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shares[fid]
	if sh == nil {
		return nil
	}
	if _, member := sh.recipients[node]; !member {
		return nil
	}
	return sh
}

// acquire takes a transfer slot for the peer.
func (s *Server) acquire(node [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active >= s.lim.Total || s.perPeer[node] >= s.lim.PerPeer {
		return false
	}
	s.active++
	s.perPeer[node]++
	s.stats.Active.Add(1)
	return true
}

func (s *Server) release(node [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	if s.perPeer[node]--; s.perPeer[node] <= 0 {
		delete(s.perPeer, node)
	}
	s.stats.Active.Add(-1)
}

func filesLimiter() frame.Limiter {
	sp, ok := service.Builtin(service.Files)
	if !ok {
		return nil
	}
	return sp.Versions[2].Limiter()
}

// readWithin reads the next frame, giving up after d of the injected clock.
func (s *Server) readWithin(st *mux.Stream, d time.Duration) (frame.Frame, error) {
	var mu sync.Mutex
	finished := false
	tm := s.clk.AfterFunc(d, func() {
		mu.Lock()
		defer mu.Unlock()
		if !finished {
			_ = st.SetReadDeadline(time.Now())
		}
	})
	f, err := st.ReadFrame()
	tm.Stop()
	mu.Lock()
	finished = true
	if err == nil {
		_ = st.SetReadDeadline(time.Time{})
	}
	mu.Unlock()
	return f, err
}

func (s *Server) violation(st *mux.Stream, why string) {
	s.stats.Violations.Add(1)
	_ = st.CloseWithCode(frame.CodeProtocolViolation, "files: "+why)
}

// refuse answers a WANT that cannot be served: CANCEL unavailable, whatever the cause.
func (s *Server) refuse(st *mux.Stream, fid [16]byte) {
	s.stats.Refused.Add(1)
	_ = st.WriteFrame(TypeCancel, 0, Cancel{FID: fid, Reason: ReasonUnavailable}.Append(nil))
	_ = st.Close()
}

// Handle serves one files/2 stream opened by an authenticated peer. It owns the stream.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	st.SetLimiter(filesLimiter())
	held := false
	defer func() {
		if held {
			s.release(p.Node)
		}
	}()
	var scratch, buf []byte
	for {
		f, err := s.readWithin(st, s.lim.IdleTimeout)
		if err != nil {
			if frame.IsTooLarge(err) || frame.IsMalformed(err) {
				s.stats.Violations.Add(1)
			}
			st.Reset(frame.CodeTimeout)
			return
		}
		switch f.Type {
		case TypeWant:
			w, err := ParseWant(f.Payload)
			if err != nil {
				s.violation(st, "want")
				return
			}
			sh := s.lookup(w.FID, p.Node)
			if sh != nil && !held {
				held = s.acquire(p.Node)
				if !held {
					sh = nil
				}
			}
			if sh == nil {
				s.refuse(st, w.FID)
				return
			}
			if buf == nil {
				buf = make([]byte, ChunkSize)
			}
			var ok bool
			if scratch, ok = s.serve(ctx, st, sh, w, buf, scratch); !ok {
				return
			}
		case TypeDone:
			d, err := ParseDone(f.Payload)
			if err != nil {
				s.violation(st, "done")
				return
			}
			if sh := s.lookup(d.FID, p.Node); sh != nil {
				s.stats.Done.Add(1)
				if s.cfg.OnDone != nil {
					s.cfg.OnDone(d.FID, p.Node)
				}
			}
			_ = st.Close()
			return
		case TypeCancel:
			if _, err := ParseCancel(f.Payload); err != nil {
				s.violation(st, "cancel")
				return
			}
			_ = st.Close()
			return
		default:
			s.violation(st, "unexpected frame")
			return
		}
	}
}

// serve sends the chunks of one WANT. A receiver that does not take a chunk within WantTimeout of
// the injected clock loses its stream. It reports false when the stream is over.
func (s *Server) serve(ctx context.Context, st *mux.Stream, sh *shared, w Want, buf, scratch []byte) ([]byte, bool) {
	// the receiver is given WantTimeout per chunk, not per WANT: a slow link is fine, a stalled one is not
	var tm carrier.Timer
	arm := func() {
		if tm != nil {
			tm.Stop()
		}
		tm = s.clk.AfterFunc(s.lim.WantTimeout, func() { st.Reset(frame.CodeTimeout) })
	}
	arm()
	defer func() { tm.Stop() }()
	n := uint64(sh.Offer.Chunks())
	size := int64(sh.Offer.Size)
	for _, r := range w.Ranges {
		for k := uint64(0); k < uint64(r.Count); k++ {
			i := r.Start + k
			if i >= n {
				s.violation(st, "chunk index")
				return scratch, false
			}
			if ctx.Err() != nil || sh.gone.Load() {
				s.refuse(st, w.FID)
				return scratch, false
			}
			l := chunkLen(size, int(i))
			if err := readAt(sh.Source, buf[:l], int64(i)*ChunkSize); err != nil {
				s.refuse(st, w.FID) // the file is gone or changed under us
				return scratch, false
			}
			if fp := s.filter.Load(); fp != nil && (*fp)(w.FID, i) {
				continue
			}
			proof := sh.Tree.Proof(int(i))
			c := Chunk{FID: w.FID, Index: i, Data: buf[:l], Proof: make([][32]byte, len(proof))}
			for j := range proof {
				c.Proof[j] = proof[j]
			}
			scratch = c.Append(scratch[:0])
			if err := st.WriteFrame(TypeChunk, 0, scratch); err != nil {
				return scratch, false
			}
			s.stats.ChunksSent.Add(1)
			arm()
		}
	}
	return scratch, true
}

func readAt(r io.ReaderAt, buf []byte, off int64) error {
	n, err := r.ReadAt(buf, off)
	if n == len(buf) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}
