package behaviour

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// NetemConfig is a lossy, slow, narrow path between a client and a server on loopback: the part
// of tc netem the launchpad's network matrix needs, in process, so it runs without Docker.
type NetemConfig struct {
	Loss  float64       // probability that a datagram is dropped, each way
	Delay time.Duration // one-way delay added to every datagram
	MTU   int           // largest UDP payload that gets through; 0 means no limit
	Seed  int64         // seed of the loss decisions
}

// NetemStats counts what the path did.
type NetemStats struct{ Forwarded, Dropped, Oversize int64 }

// Netem forwards UDP between clients that talk to Addr and the target, applying the config. Each
// client gets its own upstream socket, so the target sees one source per client; Rebind gives them
// new ones (a change of NAT mapping or of network).
type Netem struct {
	front  net.PacketConn
	target *net.UDPAddr
	cfg    NetemConfig

	mu       sync.Mutex
	rng      *rand.Rand
	clients  map[string]*upstream
	cutUntil time.Time
	closed   bool

	forwarded, dropped, oversize atomic.Int64
	wg                           sync.WaitGroup
}

type upstream struct {
	conn   net.Conn
	client net.Addr
}

// StartNetem listens on a loopback port and forwards to target.
func StartNetem(target string, cfg NetemConfig) (*Netem, error) {
	ta, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	front, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	n := &Netem{front: front, target: ta, cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)), clients: map[string]*upstream{}}
	n.wg.Add(1)
	go n.readClients()
	return n, nil
}

// Addr is where clients send.
func (n *Netem) Addr() string { return n.front.LocalAddr().String() }

// Stats returns the counters.
func (n *Netem) Stats() NetemStats {
	return NetemStats{Forwarded: n.forwarded.Load(), Dropped: n.dropped.Load(), Oversize: n.oversize.Load()}
}

// Cut makes the path a black hole, both ways, for d.
func (n *Netem) Cut(d time.Duration) {
	n.mu.Lock()
	n.cutUntil = time.Now().Add(d)
	n.mu.Unlock()
}

// Rebind gives every client a new upstream socket: the server sees a new source address.
func (n *Netem) Rebind() {
	n.mu.Lock()
	old := n.clients
	n.clients = map[string]*upstream{}
	n.mu.Unlock()
	for _, u := range old {
		_ = u.conn.Close()
	}
}

// Close stops the path.
func (n *Netem) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	old := n.clients
	n.clients = map[string]*upstream{}
	n.mu.Unlock()
	_ = n.front.Close()
	for _, u := range old {
		_ = u.conn.Close()
	}
	n.wg.Wait()
}

// lose decides whether a datagram of this size is lost and counts why.
func (n *Netem) lose(size int) bool {
	n.mu.Lock()
	cut := time.Now().Before(n.cutUntil)
	drop := n.cfg.Loss > 0 && n.rng.Float64() < n.cfg.Loss
	n.mu.Unlock()
	switch {
	case n.cfg.MTU > 0 && size > n.cfg.MTU:
		n.oversize.Add(1)
		n.dropped.Add(1)
		return true
	case cut || drop:
		n.dropped.Add(1)
		return true
	}
	n.forwarded.Add(1)
	return false
}

func (n *Netem) later(f func()) {
	if n.cfg.Delay <= 0 {
		f()
		return
	}
	time.AfterFunc(n.cfg.Delay, f)
}

func (n *Netem) readClients() {
	defer n.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		k, from, err := n.front.ReadFrom(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:k]...)
		if n.lose(len(pkt)) {
			continue
		}
		u, err := n.upstreamFor(from)
		if err != nil {
			continue
		}
		n.later(func() { _, _ = u.conn.Write(pkt) })
	}
}

func (n *Netem) upstreamFor(client net.Addr) (*upstream, error) {
	key := client.String()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, errors.New("netem closed")
	}
	if u, ok := n.clients[key]; ok {
		return u, nil
	}
	c, err := net.DialUDP("udp", nil, n.target)
	if err != nil {
		return nil, err
	}
	u := &upstream{conn: c, client: client}
	n.clients[key] = u
	n.wg.Add(1)
	go n.readUpstream(u)
	return u, nil
}

func (n *Netem) readUpstream(u *upstream) {
	defer n.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		k, err := u.conn.Read(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:k]...)
		if n.lose(len(pkt)) {
			continue
		}
		n.later(func() { _, _ = n.front.WriteTo(pkt, u.client) })
	}
}
