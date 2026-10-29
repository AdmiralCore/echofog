package network

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"net"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network/mimicry"
)

// TCP link rotation (rotconn.go) in the TCP transport: when to use it, how a client opens the
// spare carriers, how a server joins them to a session.

const (
	frozenFor     = 6 * time.Hour // an address that froze a link is dialed with rotation for this long
	freezeMinRx   = 6 << 10       // a link that had carried this much ...
	freezeMaxRx   = 256 << 10     // ... but not past this ...
	freezeQuietAt = 15 * time.Second
	maxRotSession = 8192
)

// rotState is the transport side of rotation: the policy, what froze, and the sessions a server
// holds.
type rotState struct {
	mu       sync.Mutex
	policy   func(addr string) bool
	off      bool // never rotate, not even where a link froze
	budget   int
	frozen   map[string]time.Time
	sessions map[[16]byte]*rotConn
}

// SetRotatePolicy says for which addresses a link is dialed with rotation (nil: none, except the
// addresses that froze a link here). Servers accept rotation from whoever asks.
func (t *TCPTransport) SetRotatePolicy(f func(addr string) bool) {
	t.rot.mu.Lock()
	t.rot.policy = f
	t.rot.mu.Unlock()
}

// SetRotateBudget sets how many bytes a carrier may carry in each direction (default RotBudget).
func (t *TCPTransport) SetRotateBudget(n int) {
	t.rot.mu.Lock()
	t.rot.budget = n
	t.rot.mu.Unlock()
}

// SetRotateOff turns rotation off for good, what is learned from a frozen link included.
func (t *TCPTransport) SetRotateOff(off bool) {
	t.rot.mu.Lock()
	t.rot.off = off
	t.rot.mu.Unlock()
}

func (t *TCPTransport) shouldRotate(addr string) bool {
	t.rot.mu.Lock()
	if t.rot.off {
		t.rot.mu.Unlock()
		return false
	}
	if until, ok := t.rot.frozen[addr]; ok {
		if time.Now().Before(until) {
			t.rot.mu.Unlock()
			return true
		}
		delete(t.rot.frozen, addr)
	}
	policy := t.rot.policy
	t.rot.mu.Unlock()
	return policy != nil && policy(addr) // outside the lock: it may look a country up
}

// markFrozen: a link to addr went quiet after it had carried a little. The next dial rotates.
func (t *TCPTransport) markFrozen(addr string) {
	t.rot.mu.Lock()
	if t.rot.frozen == nil || len(t.rot.frozen) > 1024 {
		t.rot.frozen = map[string]time.Time{}
	}
	_, known := t.rot.frozen[addr]
	t.rot.frozen[addr] = time.Now().Add(frozenFor)
	t.rot.mu.Unlock()
	if !known {
		t.logger.Info("A TCP link froze after a few KB (the TSPU freeze?): the next links there rotate", "addr", addr)
	}
}

// watchFreeze learns from a plain link: when it ends after it had carried a few KB and then heard
// nothing for most of the keepalive time, the censor froze it.
func (t *TCPTransport) watchFreeze(mc MuxConn, cc *countConn, addr string) {
	go func() {
		select {
		case <-mc.Context().Done():
		case <-t.ctx.Done():
			return
		}
		rx := cc.rx.Load()
		quiet := time.Since(time.Unix(0, cc.lastRx.Load()))
		if rx >= freezeMinRx && rx < freezeMaxRx && quiet >= freezeQuietAt {
			t.markFrozen(addr)
		}
	}()
}

func (t *TCPTransport) rotOpts() rotOptions {
	t.rot.mu.Lock()
	defer t.rot.mu.Unlock()
	return rotOptions{Budget: t.rot.budget}
}

// rotateClient wraps the first carrier of a new link: it sends the ATTACH that opens the session
// and returns the stream the rest of the dial uses. dial opens the spares.
func (t *TCPTransport) rotateClient(first net.Conn, cc *countConn, dial func(ctx context.Context, id [16]byte) (*rotCarrier, error)) (net.Conn, error) {
	var id [16]byte
	if _, err := crand.Read(id[:]); err != nil {
		return nil, err
	}
	if _, err := first.Write(rotAttachFrame(id, rotFlagNew)); err != nil {
		return nil, err
	}
	o := t.rotOpts()
	o.Dial = func(ctx context.Context) (*rotCarrier, error) { return dial(ctx, id) }
	rc := newRotConn(true, id, o)
	rc.addCarrier(newRotCarrier(first, cc, nil))
	return rc, nil
}

// dialCarrier opens one more carrier of the session id to addr and attaches it.
func (t *TCPTransport) dialCarrier(ctx context.Context, addr string, id [16]byte, cfg *mimicry.MimicryConfig) (*rotCarrier, error) {
	raw, err := dialTCPOut(ctx, addr)
	if err != nil {
		return nil, err
	}
	tuneTCP(raw)
	_ = raw.SetDeadline(time.Now().Add(tcpHandshakeTimeout))
	cc := newCountConn(raw)
	conn, _, err := mimicry.ClientHandshake(cc, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if _, err := conn.Write(rotAttachFrame(id, 0)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return newRotCarrier(conn, cc, nil), nil
}

type rotKind int

const (
	rotPlain rotKind = iota // an ordinary link
	rotSpare                // a carrier for a session this server holds
	rotFirst                // the carrier that opens a session
)

// sniffRotating reads the first byte of an authenticated inbound connection. An ordinary link comes
// back as it was (conn replaces it); an ATTACH is read, and says whether the carrier opens a
// session or joins one.
func (t *TCPTransport) sniffRotating(conn net.Conn) (out net.Conn, br *bufio.Reader, kind rotKind, id [16]byte, err error) {
	br = bufio.NewReaderSize(conn, 8<<10)
	b, err := br.Peek(1)
	if err != nil {
		return conn, nil, rotPlain, id, err
	}
	if b[0] != rotAttach {
		return &peekedConn{Conn: conn, br: br}, br, rotPlain, id, nil
	}
	id, flags, err := readRotAttach(br)
	if err != nil {
		return conn, nil, rotPlain, id, err
	}
	if flags&rotFlagNew != 0 {
		return conn, br, rotFirst, id, nil
	}
	return conn, br, rotSpare, id, nil
}

// attachSpare joins a carrier to the session it names. The session id is a secret only its client
// holds, so a spare needs no turn at the per-address limits the first carrier went through: a
// rotating link opens a new connection every few KB, and a limit made for strangers would be its
// ceiling. It reports false when this server has no such session.
func (t *TCPTransport) attachSpare(conn net.Conn, cc *countConn, br *bufio.Reader, id [16]byte) bool {
	t.rot.mu.Lock()
	rc := t.rot.sessions[id]
	t.rot.mu.Unlock()
	if rc == nil {
		return false
	}
	return rc.addCarrier(newRotCarrier(conn, cc, br))
}

// openRotating starts the session whose first carrier this is; the stream it presents takes the
// place of the connection.
func (t *TCPTransport) openRotating(conn net.Conn, cc *countConn, br *bufio.Reader, id [16]byte) net.Conn {
	t.rot.mu.Lock()
	if t.rot.sessions == nil {
		t.rot.sessions = map[[16]byte]*rotConn{}
	}
	if t.rot.sessions[id] != nil || len(t.rot.sessions) >= maxRotSession {
		t.rot.mu.Unlock()
		return nil
	}
	o := rotOptions{Budget: t.rot.budget}
	o.OnClose = func() {
		t.rot.mu.Lock()
		delete(t.rot.sessions, id)
		t.rot.mu.Unlock()
	}
	rc := newRotConn(false, id, o)
	t.rot.sessions[id] = rc
	t.rot.mu.Unlock()
	rc.addCarrier(newRotCarrier(conn, cc, br))
	return rc
}
