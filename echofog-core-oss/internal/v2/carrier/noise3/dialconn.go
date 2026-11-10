package noise3

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/elligator"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3/packet"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// dialConn is the client-side virtual net.PacketConn of one noise3 flow. Until the first
// authenticated responder packet it wraps every inner datagram in a fresh Init (new ephemeral,
// new representative) and keeps up to four outstanding key sets; the first authenticated response
// selects the set, and the others are discarded (wire.md §2.4, §2.5 step 6).
type dialConn struct {
	pc     *queueConn
	remote net.Addr

	dial func(context.Context) (net.PacketConn, net.Addr, error)

	maskPub     [32]byte
	sender      *shape.Sender
	clock       Clock
	maxDatagram int

	restartAfter time.Duration

	mu          sync.Mutex
	writeMu     sync.Mutex
	conn        net.PacketConn
	sets        []packet.Keys
	adopted     *packet.Keys
	sendN       uint64
	recvWin     packet.Window
	lastAuth    time.Time
	lastSend    time.Time
	closed      bool
	sentPrelude bool

	closeOnce sync.Once
}

// clientVAddr is the single virtual remote address of the dialer's inner QUIC stack.
var clientVAddr = flowAddr{id: 1}

// clientLocalAddr is the dialer's virtual local address; it must differ from clientVAddr so the
// QUIC stack does not confuse its two ends.
var clientLocalAddr = flowAddr{id: 2}

func newDialConn(cfg DialerConfig, clock Clock, profile *shape.Profile, maskPub [32]byte,
	dial func(context.Context) (net.PacketConn, net.Addr, error), remote net.Addr) *dialConn {
	c := &dialConn{
		remote:       remote,
		dial:         dial,
		maskPub:      maskPub,
		sender:       shape.NewSender(profile, nil, cfg.LowPower),
		clock:        clock,
		maxDatagram:  l0MaxDatagram(cfg.MaxDatagramLen),
		restartAfter: restartDelay(nil),
	}
	c.pc = newQueueConn(clientLocalAddr, func(p []byte, _ net.Addr) (int, error) { return c.write(p) })
	return c
}

func (c *dialConn) start(ctx context.Context) error {
	conn, remote, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.remote = remote
	c.mu.Unlock()
	go c.readFrom(conn)
	return nil
}

func (c *dialConn) readFrom(conn net.PacketConn) {
	buf := make([]byte, 4096)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		c.handle(append([]byte(nil), buf[:n]...), c.clock.Now())
	}
}

// WriteTo is the inner QUIC stack writing one datagram into the flow.
//
// writeMu serialises writers (and keeps packet order); the state mutex is held only around state
// changes, never across dialing, sleeping or socket writes.
func (c *dialConn) write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	now := c.clock.Now()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errClosed
	}
	restart := c.adopted != nil && now.Sub(c.lastAuth) >= c.restartAfter && c.lastSend.After(c.lastAuth)
	c.mu.Unlock()
	if restart {
		if err := c.restart(now); err != nil {
			// Keep the old flow and let QUIC retransmit; a failed dial must not kill the
			// inner connection.
			return len(p), nil
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errClosed
	}
	prelude := !c.sentPrelude
	if prelude {
		c.sentPrelude = true
	}
	conn, remote := c.conn, c.remote
	adopted := c.adopted == nil
	c.lastSend = now
	c.mu.Unlock()

	if prelude {
		c.sendPrelude(conn, remote)
	}
	if adopted {
		return c.sendInit(p, now)
	}
	return c.sendData(p)
}

// restart starts a new flow after uniform(12, 18) s without an authenticated responder packet
// (wire.md §2.6). The inner QUIC connection keeps running: to the responder the new flow is a new
// path of the same connection. The dial and the old socket close happen without the state mutex.
func (c *dialConn) restart(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, remote, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return errClosed
	}
	if now.Sub(c.lastAuth) < c.restartAfter {
		// An authenticated packet arrived while the dial was in flight; keep the old flow.
		c.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	old := c.conn
	c.conn = conn
	c.remote = remote
	c.sets = nil
	c.adopted = nil
	c.sendN = 0
	c.recvWin = packet.Window{}
	c.lastAuth = now
	c.lastSend = now
	c.sentPrelude = false
	c.restartAfter = restartDelay(nil)
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go c.readFrom(conn)
	return nil
}

// sendPrelude sends the junk datagrams and decoys of the shape profile before the first Init
// (wire.md §2.7). It may sleep for the profile's inter-junk gap; writeMu is held, the state mutex
// is not.
func (c *dialConn) sendPrelude(conn net.PacketConn, remote net.Addr) {
	if c.sender == nil || conn == nil {
		return
	}
	for _, d := range c.sender.Decoys() {
		_, _ = conn.WriteTo(d.Data, remote)
	}
	for _, d := range c.sender.Junk() {
		if d.Gap > 0 {
			time.Sleep(d.Gap)
		}
		_, _ = conn.WriteTo(d.Data, remote)
	}
}

func (c *dialConn) sendInit(inner []byte, now time.Time) (int, error) {
	need := packet.InitOverhead + len(inner)
	if need > c.maxDatagram {
		return 0, packet.ErrMalformed
	}
	for attempt := 0; attempt < 8; attempt++ {
		sk, repr, err := elligator.Generate(rand.Reader)
		if err != nil {
			return 0, err
		}
		z, err := curve25519.X25519(sk, c.maskPub[:])
		if err != nil {
			continue
		}
		if allZero(z) {
			continue
		}
		dirty := elligator.Forward(repr)
		keys, err := packet.Derive(z, c.maskPub[:], dirty, repr)
		if err != nil {
			continue
		}
		total := c.sender.NextInitLen()
		if total < need {
			total = need
		}
		if total > c.maxDatagram {
			total = c.maxDatagram
		}
		if total > packet.MaxFirstLen && need <= packet.MaxFirstLen {
			total = packet.MaxFirstLen // no datagram before the first answer is longer (wire.md §2.4)
		}
		dg, err := packet.SealInit(keys, repr, uint32(now.Unix()), inner, total)
		if err != nil {
			return 0, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, errClosed
		}
		c.sets = append(c.sets, keys)
		if len(c.sets) > 4 {
			c.sets = append(c.sets[:0], c.sets[len(c.sets)-4:]...)
		}
		conn, remote := c.conn, c.remote
		c.mu.Unlock()
		if _, err := conn.WriteTo(dg, remote); err != nil {
			return 0, err
		}
		return len(inner), nil
	}
	return 0, errors.New("noise3: could not derive a fresh Init key")
}

func (c *dialConn) sendData(inner []byte) (int, error) {
	if packet.DataOverhead+len(inner) > c.maxDatagram {
		return 0, packet.ErrMalformed
	}
	pad := c.sender.PadFor(packet.DataOverhead + len(inner))
	if room := c.maxDatagram - packet.DataOverhead - len(inner); pad > room {
		pad = room
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, errClosed
	}
	if c.adopted == nil {
		c.mu.Unlock()
		return c.sendInit(inner, c.clock.Now())
	}
	dg, err := packet.SealData(c.adopted.KI2R, c.adopted.HPI2R, c.sendN, inner, pad)
	if err != nil {
		c.mu.Unlock()
		return 0, err
	}
	c.sendN++
	conn, remote := c.conn, c.remote
	c.mu.Unlock()
	if _, err := conn.WriteTo(dg, remote); err != nil {
		return 0, err
	}
	return len(inner), nil
}

func (c *dialConn) handle(d []byte, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	highest := c.recvWin.Highest()
	if c.adopted != nil {
		if n, inner, err := packet.OpenData(c.adopted.KR2I, c.adopted.HPR2I, d, highest); err == nil {
			if c.recvWin.Check(n) {
				c.recvWin.Commit(n)
				c.lastAuth = now
				c.pc.deliver(inner, clientVAddr)
			}
			return
		}
	}
	// The responder sends everything with the keys of the first Init of the flow; adopt the
	// first set that authenticates the packet and drop the rest.
	for i := len(c.sets) - 1; i >= 0; i-- {
		k := c.sets[i]
		n, inner, err := packet.OpenData(k.KR2I, k.HPR2I, d, highest)
		if err != nil {
			continue
		}
		c.recvWin = packet.Window{}
		if !c.recvWin.Check(n) {
			return
		}
		c.recvWin.Commit(n)
		adopted := k
		c.adopted = &adopted
		c.sets = nil
		c.lastAuth = now
		c.pc.deliver(inner, clientVAddr)
		return
	}
}

func (c *dialConn) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		conn := c.conn
		c.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		_ = c.pc.Close()
	})
	return nil
}

func (c *dialConn) ReadFrom(p []byte) (int, net.Addr, error) { return c.pc.ReadFrom(p) }
func (c *dialConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.pc.WriteTo(p, addr)
}
func (c *dialConn) LocalAddr() net.Addr                { return c.pc.LocalAddr() }
func (c *dialConn) SetDeadline(t time.Time) error      { return c.pc.SetDeadline(t) }
func (c *dialConn) SetReadDeadline(t time.Time) error  { return c.pc.SetReadDeadline(t) }
func (c *dialConn) SetWriteDeadline(t time.Time) error { return c.pc.SetWriteDeadline(t) }
