package reality2

import (
	"errors"
	"net"
	"sort"
	"sync"
	"time"
)

// Cover forwarding of wire.md §3.3: every packet the gate does not admit goes to the real
// cover site, which answers it. Each stranger source gets its own outbound UDP socket (up to
// 1024) so the cover's replies come back to the right source; beyond that a shared socket is
// used and replies are mapped back by QUIC connection ID.
const (
	// DefaultCoverSessions is the number of per-source cover sockets of wire.md §3.3.
	DefaultCoverSessions = 1024
	// DefaultCoverIdle is how long a per-source cover socket may stay idle before it is
	// evicted. A live forwarded connection is never cut by the node.
	DefaultCoverIdle = 30 * time.Second
	// maxSharedCIDs bounds the shared-socket connection-ID table.
	maxSharedCIDs = 8192
)

// Cover forwards packets that are not admitted to the cover site.
type Cover interface {
	// Forward sends one datagram from src to the cover. It never blocks and never returns
	// an error: a stranger's packet is best effort, exactly like a real site's traffic.
	Forward(src net.Addr, p []byte)
	Close() error
}

// CoverConfig configures UDPCover.
type CoverConfig struct {
	// Addr is the cover site's UDP address (host:port, port 443 recommended).
	Addr *net.UDPAddr
	// Reply delivers a cover reply to the stranger source. The gate installs it.
	Reply func(p []byte, src net.Addr)
	// MaxSessions is DefaultCoverSessions when zero.
	MaxSessions int
	// IdleTimeout is DefaultCoverIdle when zero.
	IdleTimeout time.Duration
	// Now is the injectable clock; nil means time.Now.
	Now func() time.Time
	// OnError receives cover socket errors (a failed dial, a failed write, a read that ends
	// abnormally). Nil ignores them. A stranger's traffic is best effort either way.
	OnError func(error)
}

// UDPCover is the cover forwarder of wire.md §3.3.
type UDPCover struct {
	cfg CoverConfig

	// dial opens an outbound UDP socket; net.DialUDP unless a test replaces it.
	dial func(network string, laddr, raddr *net.UDPAddr) (*net.UDPConn, error)

	mu       sync.Mutex
	sessions map[string]*coverSession
	closed   bool
	shared   *sharedCover
}

type coverSession struct {
	conn     *net.UDPConn
	src      net.Addr
	lastUsed time.Time
}

// NewUDPCover opens the cover forwarder. It does not connect to the cover until the first
// stranger packet: a node without strangers never talks to its cover.
func NewUDPCover(cfg CoverConfig) (*UDPCover, error) {
	if cfg.Addr == nil {
		return nil, errors.New("reality2: cover address is required")
	}
	if cfg.Reply == nil {
		return nil, errors.New("reality2: cover reply callback is required")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultCoverSessions
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultCoverIdle
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &UDPCover{
		cfg:      cfg,
		dial:     net.DialUDP,
		sessions: make(map[string]*coverSession),
	}, nil
}

// report passes a cover socket error to the configured handler, if any.
func (c *UDPCover) report(err error) {
	if c.cfg.OnError != nil && !errors.Is(err, net.ErrClosed) {
		c.cfg.OnError(err)
	}
}

// Forward sends p from src to the cover.
func (c *UDPCover) Forward(src net.Addr, p []byte) {
	if src == nil {
		return
	}
	key := src.String()
	now := c.cfg.Now()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if s := c.sessions[key]; s != nil {
		s.lastUsed = now
		c.mu.Unlock()
		if _, err := s.conn.Write(p); err != nil {
			c.report(err)
		}
		return
	}
	// A full table is where eviction matters: sessions idle for longer than IdleTimeout are
	// closed to make room for the new stranger (wire.md §3.3). Below the limit no scan runs,
	// so a stream of new sources does not pay for one per packet.
	if len(c.sessions) >= c.cfg.MaxSessions {
		c.evictIdleLocked(now)
	}
	if len(c.sessions) < c.cfg.MaxSessions {
		dial := c.dial
		if dial == nil {
			dial = net.DialUDP
		}
		conn, err := dial("udp", nil, c.cfg.Addr)
		if err == nil {
			s := &coverSession{conn: conn, src: src, lastUsed: now}
			c.sessions[key] = s
			c.mu.Unlock()
			go c.readSession(s)
			if _, err := conn.Write(p); err != nil {
				c.report(err)
			}
			return
		}
		c.report(err)
	}
	if c.shared == nil {
		dial := c.dial
		if dial == nil {
			dial = net.DialUDP
		}
		shared, err := newSharedCover(c.cfg, dial)
		if err != nil {
			c.mu.Unlock()
			c.report(err)
			return
		}
		c.shared = shared
		go c.readShared(shared)
	}
	shared := c.shared
	c.mu.Unlock()
	shared.forward(src, p)
}

// evictIdleLocked closes sessions idle for longer than IdleTimeout. A session with traffic is
// never touched, as wire.md §3.3 requires.
func (c *UDPCover) evictIdleLocked(now time.Time) {
	for k, s := range c.sessions {
		if now.Sub(s.lastUsed) > c.cfg.IdleTimeout {
			_ = s.conn.Close()
			delete(c.sessions, k)
		}
	}
}

func (c *UDPCover) readSession(s *coverSession) {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			c.report(err)
			return
		}
		c.mu.Lock()
		s.lastUsed = c.cfg.Now()
		c.mu.Unlock()
		c.cfg.Reply(buf[:n], s.src)
	}
}

func (c *UDPCover) readShared(s *sharedCover) {
	buf := make([]byte, 64<<10)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			c.report(err)
			return
		}
		if src, ok := s.lookup(buf[:n]); ok {
			c.cfg.Reply(buf[:n], src)
		}
	}
}

// Close closes every cover socket. It is idempotent.
func (c *UDPCover) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	for k, s := range c.sessions {
		_ = s.conn.Close()
		delete(c.sessions, k)
	}
	if c.shared != nil {
		_ = c.shared.conn.Close()
	}
	return nil
}

// sharedCover carries strangers beyond the per-source limit over one socket. Replies are
// mapped back by the client's source connection ID, which the cover echoes as the
// destination connection ID (RFC 9000 §7.2).
type sharedCover struct {
	conn net.Conn
	addr *net.UDPAddr

	mu    sync.Mutex
	byCID map[string]net.Addr
	order []string
	lens  map[int]struct{}
	onErr func(error)
}

func newSharedCover(cfg CoverConfig, dial func(string, *net.UDPAddr, *net.UDPAddr) (*net.UDPConn, error)) (*sharedCover, error) {
	if dial == nil {
		dial = net.DialUDP
	}
	conn, err := dial("udp", nil, cfg.Addr)
	if err != nil {
		return nil, err
	}
	return &sharedCover{
		conn:  conn,
		addr:  cfg.Addr,
		byCID: make(map[string]net.Addr),
		lens:  make(map[int]struct{}),
		onErr: cfg.OnError,
	}, nil
}

// forward sends one packet and remembers which stranger owns the source connection ID. The
// first owner wins: a stranger that reuses another's CID must not take over its replies, and a
// real client never reuses a CID across sources.
func (s *sharedCover) forward(src net.Addr, p []byte) {
	if cid, ok := clientSCID(p); ok && len(cid) > 0 {
		key := string(cid)
		s.mu.Lock()
		owner, seen := s.byCID[key]
		if !seen {
			s.order = append(s.order, key)
			if len(s.order) > maxSharedCIDs {
				delete(s.byCID, s.order[0])
				s.order = s.order[1:]
			}
			s.byCID[key] = src
		} else if owner.String() == src.String() {
			// The same source sent another Initial/Coalesced packet: nothing to update.
		}
		s.lens[len(cid)] = struct{}{}
		s.mu.Unlock()
	}
	if _, err := s.conn.Write(p); err != nil && s.onErr != nil && !errors.Is(err, net.ErrClosed) {
		s.onErr(err)
	}
}

// lookup finds the stranger a cover reply belongs to. A long header carries the DCID length; a
// short header does not, so every client CID length seen on this socket is tried, longest
// first: the most specific match wins, so a short CID that is a prefix of another cannot
// capture the longer one's replies.
func (s *sharedCover) lookup(p []byte) (net.Addr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) < 2 {
		return nil, false
	}
	if p[0]&0x80 != 0 {
		if len(p) < 6 {
			return nil, false
		}
		l := int(p[5])
		if len(p) < 6+l {
			return nil, false
		}
		src, ok := s.byCID[string(p[6:6+l])]
		return src, ok
	}
	lens := make([]int, 0, len(s.lens))
	for l := range s.lens {
		lens = append(lens, l)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(lens)))
	for _, l := range lens {
		if len(p) < 1+l {
			continue
		}
		if src, ok := s.byCID[string(p[1:1+l])]; ok {
			return src, true
		}
	}
	return nil, false
}

// clientSCID returns the source connection ID of a long-header packet.
func clientSCID(p []byte) ([]byte, bool) {
	if len(p) < 6 || p[0]&0x80 == 0 {
		return nil, false
	}
	off := 5
	dcidLen := int(p[off])
	off++
	if dcidLen > 20 || len(p) < off+dcidLen+1 {
		return nil, false
	}
	off += dcidLen
	scidLen := int(p[off])
	off++
	if scidLen > 20 || len(p) < off+scidLen {
		return nil, false
	}
	return p[off : off+scidLen], true
}
