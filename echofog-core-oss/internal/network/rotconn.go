package network

// rotConn: one byte stream carried by a series of short TCP connections, its carriers. It answers
// the TSPU freeze: a TCP connection from inside Russia to a foreign hosting address works for about
// 16 KB of the server's answer and then goes silent. A carrier here never lives that long: each side
// counts the bytes it put on the carrier (the TLS handshake included, counted under the TLS) and
// moves to the next carrier before the budget is spent. The streams above (smux, the peer
// handshake) see an ordinary net.Conn and do not know.
//
// The stream is cut into DATA frames that carry their offset, so the receiver puts them in order
// whichever carrier they came by and drops what it already has. The sender keeps the bytes the
// peer has not acknowledged and sends them again on another carrier when one dies in the middle;
// a carrier that ends in the agreed way (RETIRE both ways) loses nothing and needs no resend.
// Spare carriers are dialed ahead by the client and wait idle, attached to the session, so the
// switch costs no round trip; the server uses the ones the client attached.
//
// Wire, after the TLS of the carrier (type byte first, big endian):
//
//	ATTACH 0xE5 | session id 16 | flags 1       first on every carrier, client to server
//	DATA   1    | offset 8 | length 2 | bytes
//	ACK    2    | offset 8                       the receiver holds everything before offset
//	RETIRE 3    | offset 8                       no more data from me on this carrier (offset: as ACK)
//	CLOSE  4                                     the session is over
//
// 0xE5 never opens an smux stream (its first byte is the version, 1 or 2), which is how a server
// tells a rotating client from one that is not.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rotAttach byte = 0xE5
	rotData   byte = 1
	rotAck    byte = 2
	rotRetire byte = 3
	rotClose  byte = 4

	rotFlagNew byte = 1 // ATTACH: this carrier opens the session

	rotMaxPayload = 4096
	rotAckEvery   = 16 << 10
	rotMaxUnacked = 512 << 10              // Write waits when the peer is this far behind
	rotMaxReadBuf = 1 << 20                // a carrier waits to deliver when the reader is this far behind
	rotMaxOOO     = rotMaxUnacked + 64<<10 // out-of-order bytes held: no more than the sender can have in flight
	rotOverhead   = 64                     // TLS record and frame header, kept clear of the budget
	rotMinRoom    = 160                    // below this a carrier is retired, not filled

	// RotBudget is what each carrier may carry in each direction, TLS handshake included. The
	// freeze counts around 16 KB; this leaves the room a different count or a retransmission eats.
	RotBudget = 12 << 10

	rotRetireGrace = 3 * time.Second
	rotResend      = 4 * time.Second  // unacknowledged for this long: sent again
	rotGrace       = 45 * time.Second // a session with no carrier at all lives this long
	rotMaxDials    = 3                // dials at once while the link is quiet
	rotMaxCarriers = 20               // a session holds no more than this many at once
	rotSpares      = 2                // spare carriers kept ready while the link is quiet
	rotMaxSpares   = 12               // ... and at most this many while data waits for one
	rotShrinkAfter = 8 * time.Second
)

var errRotProtocol = errors.New("rotating connection: protocol error")

// countConn counts the bytes that go through a raw connection, below its TLS.
type countConn struct {
	net.Conn
	rx, tx atomic.Int64
	lastRx atomic.Int64 // unix nanos
}

func newCountConn(c net.Conn) *countConn {
	cc := &countConn{Conn: c}
	cc.lastRx.Store(time.Now().UnixNano())
	return cc
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.rx.Add(int64(n))
		c.lastRx.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.tx.Add(int64(n))
	return n, err
}

// rotOptions: Budget is per carrier and direction; Dial (client only) opens a carrier with its
// ATTACH sent; Spares is how many ready carriers the client keeps besides the one in use.
type rotOptions struct {
	Budget  int
	Spares  int
	Dial    func(ctx context.Context) (*rotCarrier, error)
	OnClose func()
	Grace   time.Duration
}

// rotCarrier is one TCP connection of a session: conn is its TLS, cc the raw connection under it.
type rotCarrier struct {
	conn net.Conn
	cc   *countConn
	br   *bufio.Reader

	wmu                   sync.Mutex
	retired               bool // under wmu: RETIRE went out, nothing may follow it
	sentRetire, gotRetire bool // under rotConn.mu
	dead                  bool // under rotConn.mu
}

func newRotCarrier(conn net.Conn, cc *countConn, br *bufio.Reader) *rotCarrier {
	if br == nil {
		br = bufio.NewReaderSize(conn, 8<<10)
	}
	return &rotCarrier{conn: conn, cc: cc, br: br}
}

var errRotRetired = errors.New("carrier retired")

// write sends a frame. Nothing may follow RETIRE on a carrier, since the peer closes it as soon as
// it has read the RETIRE it is waiting for: a later frame is refused with errRotRetired.
func (c *rotCarrier) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.retired {
		return errRotRetired
	}
	if b[0] == rotRetire {
		c.retired = true
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := c.conn.Write(b)
	return err
}

type rotDeadline struct {
	t     time.Time
	timer *time.Timer
}

func (d *rotDeadline) expired() bool { return !d.t.IsZero() && !time.Now().Before(d.t) }

type rotConn struct {
	mu       sync.Mutex
	cond     *sync.Cond
	opt      rotOptions
	isClient bool
	id       [16]byte
	local    net.Addr
	remote   net.Addr

	buf      []byte // bytes [ackedOff, nextOff)
	ackedOff uint64
	nextOff  uint64
	sendPos  uint64 // the next byte to put on the wire; behind nextOff while there is something to send

	recvOff  uint64
	rbuf     []byte
	ooo      map[uint64][]byte
	oooBytes int
	lastAck  uint64
	ackWait  bool      // a delayed ACK is scheduled
	progress time.Time // when the peer last acknowledged, or the unacknowledged bytes began

	remoteClosed bool
	closed       bool
	err          error

	carriers   []*rotCarrier
	active     *rotCarrier
	dialing    int
	want       int       // spare carriers wanted now: grows when data waits for a carrier, shrinks when none does
	stalledAt  time.Time // when data last waited for a carrier
	dialFails  int
	emptySince time.Time

	rdl, wdl rotDeadline
	stop     chan struct{}
}

func newRotConn(isClient bool, id [16]byte, opt rotOptions) *rotConn {
	if opt.Budget <= 0 {
		opt.Budget = RotBudget
	}
	if opt.Spares <= 0 {
		opt.Spares = rotSpares
	}
	if opt.Grace <= 0 {
		opt.Grace = rotGrace
	}
	r := &rotConn{opt: opt, want: opt.Spares, isClient: isClient, id: id, ooo: map[uint64][]byte{}, stop: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	go r.writeLoop()
	go r.watchdog()
	return r
}

// addCarrier puts a carrier into the session and starts reading it.
func (r *rotConn) addCarrier(c *rotCarrier) bool {
	r.mu.Lock()
	if r.closed || len(r.carriers) >= rotMaxCarriers {
		r.mu.Unlock()
		_ = c.conn.Close()
		return false
	}
	if r.local == nil {
		r.local, r.remote = c.conn.LocalAddr(), c.conn.RemoteAddr()
	}
	r.carriers = append(r.carriers, c)
	r.emptySince = time.Time{}
	r.dialFails = 0
	r.ensureSparesLocked()
	r.cond.Broadcast()
	r.mu.Unlock()
	go r.readLoop(c)
	return true
}

func (r *rotConn) liveLocked() (n int) {
	for _, c := range r.carriers {
		if !c.dead && !c.sentRetire && !c.gotRetire {
			n++
		}
	}
	return n
}

// ensureSparesLocked: the client dials until it holds the carrier in use and its spares.
func (r *rotConn) ensureSparesLocked() {
	if !r.isClient || r.opt.Dial == nil || r.closed {
		return
	}
	for r.liveLocked()+r.dialing < 1+r.want && r.dialing < max(rotMaxDials, r.want) {
		r.dialing++
		delay := time.Duration(0)
		if r.dialFails > 0 {
			delay = min(time.Duration(1<<min(r.dialFails, 5))*100*time.Millisecond, 5*time.Second)
		}
		go r.dialOne(delay)
	}
}

func (r *rotConn) dialOne(delay time.Duration) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.stop:
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	c, err := r.opt.Dial(ctx)
	cancel()
	r.mu.Lock()
	r.dialing--
	if err != nil || c == nil {
		r.dialFails++
		r.ensureSparesLocked()
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	r.addCarrier(c)
}

// watchdog ends a session that has had no carrier for the grace period.
func (r *rotConn) watchdog() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
		}
		r.mu.Lock()
		if !r.closed && len(r.carriers) == 0 && !r.emptySince.IsZero() && time.Since(r.emptySince) > r.opt.Grace {
			r.mu.Unlock()
			r.fail(io.ErrClosedPipe)
			return
		}
		if !r.closed && r.isClient {
			if r.want > r.opt.Spares && time.Since(r.stalledAt) > rotShrinkAfter {
				r.want-- // the speed dropped: fewer spares idle on the server
			}
			r.ensureSparesLocked()
		}
		// bytes the peer has not acknowledged for a while may have gone down a carrier that did not
		// carry them: send them again (a copy it already has is dropped)
		if !r.closed && r.nextOff > r.ackedOff && r.sendPos >= r.nextOff && time.Since(r.progress) > rotResend {
			r.sendPos = r.ackedOff
			r.progress = time.Now()
			r.cond.Broadcast()
		}
		r.mu.Unlock()
	}
}

// growLocked: the pool of spares is too small for the speed, so the client wants more of them. It
// is called when this side's data waits for a carrier, and, since a download starves the server
// (which cannot dial) and not the client, when the peer retires a carrier and fewer than half the
// spares are left.
func (r *rotConn) growLocked() {
	r.stalledAt = time.Now()
	if r.want < rotMaxSpares {
		r.want = min(rotMaxSpares, r.want*2)
		r.ensureSparesLocked()
	}
}

// ---- sending

func (r *rotConn) roomLocked(c *rotCarrier) int {
	return r.opt.Budget - int(c.cc.tx.Load()) - rotOverhead
}

// usableLocked: the carrier data goes on now, nil when there is none ready.
func (r *rotConn) usableLocked() *rotCarrier {
	if a := r.active; a != nil && !a.dead && !a.sentRetire && !a.gotRetire {
		return a
	}
	r.active = nil
	var best *rotCarrier
	for _, c := range r.carriers {
		if c.dead || c.sentRetire || c.gotRetire {
			continue
		}
		// the one with the most room left: a spare, or the one that has carried least
		if best == nil || r.roomLocked(c) > r.roomLocked(best) {
			best = c
		}
	}
	r.active = best
	r.ensureSparesLocked()
	return best
}

func (r *rotConn) writeLoop() {
	for {
		r.mu.Lock()
		for !r.closed && r.sendPos >= r.nextOff {
			r.cond.Wait()
		}
		if r.closed {
			r.mu.Unlock()
			return
		}
		c := r.usableLocked()
		if c == nil {
			// data waits for a carrier: the pool is too small for this speed, ask for more
			r.growLocked()
			r.cond.Wait()
			r.mu.Unlock()
			continue
		}
		room := r.roomLocked(c)
		if room < rotMinRoom {
			c.sentRetire = true
			r.active = nil
			off := r.recvOff
			r.ensureSparesLocked()
			r.mu.Unlock()
			r.sendRetire(c, off)
			continue
		}
		n := min(rotMaxPayload, room-11, int(r.nextOff-r.sendPos))
		off := r.sendPos
		fr := make([]byte, 11+n)
		fr[0] = rotData
		binary.BigEndian.PutUint64(fr[1:], off)
		binary.BigEndian.PutUint16(fr[9:], uint16(n))
		copy(fr[11:], r.buf[off-r.ackedOff:off-r.ackedOff+uint64(n)])
		r.mu.Unlock()

		err := c.write(fr)
		r.mu.Lock()
		if errors.Is(err, errRotRetired) {
			// the carrier was retired under us: the frame did not go out, the loop picks another
		} else if err != nil {
			r.carrierGoneLocked(c)
		} else if r.sendPos == off {
			r.sendPos += uint64(n)
		}
		r.mu.Unlock()
	}
}

// sendRetire tells the peer this carrier is spent on my side; once it has said the same the
// carrier is closed.
func (r *rotConn) sendRetire(c *rotCarrier, recvOff uint64) {
	var fr [9]byte
	fr[0] = rotRetire
	binary.BigEndian.PutUint64(fr[1:], recvOff)
	if err := c.write(fr[:]); err != nil && !errors.Is(err, errRotRetired) {
		r.mu.Lock()
		r.carrierGoneLocked(c)
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	both := c.gotRetire
	r.mu.Unlock()
	if both {
		r.closeCarrier(c)
		return
	}
	time.AfterFunc(rotRetireGrace, func() { r.closeCarrier(c) })
}

func (r *rotConn) closeCarrier(c *rotCarrier) { _ = c.conn.Close() }

// sendCtl sends a small frame on a live carrier, the one in use first.
func (r *rotConn) sendCtl(fr []byte) {
	r.mu.Lock()
	c := r.active
	if c == nil || c.dead {
		c = nil
		for _, x := range r.carriers {
			if !x.dead && !x.sentRetire {
				c = x
				break
			}
		}
	}
	r.mu.Unlock()
	if c != nil {
		_ = c.write(fr)
	}
}

// ---- receiving

func (r *rotConn) readLoop(c *rotCarrier) {
	defer func() {
		r.mu.Lock()
		r.carrierGoneLocked(c)
		r.mu.Unlock()
	}()
	var hdr [10]byte
	for {
		t, err := c.br.ReadByte()
		if err != nil {
			return
		}
		switch t {
		case rotData:
			if _, err := io.ReadFull(c.br, hdr[:10]); err != nil {
				return
			}
			off := binary.BigEndian.Uint64(hdr[:8])
			n := int(binary.BigEndian.Uint16(hdr[8:10]))
			if n > rotMaxPayload {
				r.fail(errRotProtocol)
				return
			}
			p := make([]byte, n)
			if _, err := io.ReadFull(c.br, p); err != nil {
				return
			}
			if !r.deliver(off, p) {
				return
			}
		case rotAck, rotRetire:
			if _, err := io.ReadFull(c.br, hdr[:8]); err != nil {
				return
			}
			off := binary.BigEndian.Uint64(hdr[:8])
			r.onAck(off)
			if t == rotRetire {
				r.onRetire(c)
			}
		case rotClose:
			r.mu.Lock()
			r.remoteClosed = true
			r.cond.Broadcast()
			r.mu.Unlock()
			return
		default:
			r.fail(errRotProtocol)
			return
		}
	}
}

func (r *rotConn) onAck(off uint64) {
	r.mu.Lock()
	if off > r.ackedOff && off <= r.nextOff {
		r.buf = append(r.buf[:0:0], r.buf[off-r.ackedOff:]...)
		r.ackedOff = off
		r.progress = time.Now()
		if r.sendPos < off {
			r.sendPos = off
		}
		r.cond.Broadcast()
	}
	r.mu.Unlock()
}

func (r *rotConn) onRetire(c *rotCarrier) {
	r.mu.Lock()
	c.gotRetire = true
	if r.active == c {
		r.active = nil
	}
	sent := c.sentRetire
	if !sent {
		c.sentRetire = true
	}
	off := r.recvOff
	if r.isClient && 2*r.liveLocked() < 1+r.want {
		r.growLocked()
	}
	r.ensureSparesLocked()
	r.cond.Broadcast()
	r.mu.Unlock()
	if !sent {
		r.sendRetire(c, off) // closes the carrier: the peer has retired it too
	} else {
		r.closeCarrier(c)
	}
}

// deliver puts received bytes in order. It reports false when the session is over.
func (r *rotConn) deliver(off uint64, p []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for !r.closed && len(r.rbuf) > rotMaxReadBuf && off <= r.recvOff {
		r.cond.Wait()
	}
	if r.closed {
		return false
	}
	end := off + uint64(len(p))
	switch {
	case end <= r.recvOff:
		return true // a resend of what is already here
	case off > r.recvOff:
		if _, dup := r.ooo[off]; !dup {
			if r.oooBytes+len(p) > rotMaxOOO {
				go r.fail(errRotProtocol)
				return false
			}
			r.ooo[off] = p
			r.oooBytes += len(p)
		}
		return true
	}
	r.rbuf = append(r.rbuf, p[r.recvOff-off:]...)
	r.recvOff = end
	for moved := true; moved; {
		moved = false
		for o, q := range r.ooo {
			if o <= r.recvOff {
				delete(r.ooo, o)
				r.oooBytes -= len(q)
				if e := o + uint64(len(q)); e > r.recvOff {
					r.rbuf = append(r.rbuf, q[r.recvOff-o:]...)
					r.recvOff = e
				}
				moved = true
			}
		}
	}
	r.cond.Broadcast()
	if r.recvOff-r.lastAck >= rotAckEvery {
		r.lastAck = r.recvOff
		var fr [9]byte
		fr[0] = rotAck
		binary.BigEndian.PutUint64(fr[1:], r.recvOff)
		go r.sendCtl(fr[:])
	} else if !r.ackWait {
		// what is left under the threshold is acknowledged a moment later, so the sender can let go
		// of it and a small last message does not stay unacknowledged
		r.ackWait = true
		time.AfterFunc(40*time.Millisecond, r.delayedAck)
	}
	return true
}

func (r *rotConn) delayedAck() {
	r.mu.Lock()
	r.ackWait = false
	if r.closed || r.recvOff == r.lastAck {
		r.mu.Unlock()
		return
	}
	r.lastAck = r.recvOff
	var fr [9]byte
	fr[0] = rotAck
	binary.BigEndian.PutUint64(fr[1:], r.recvOff)
	r.mu.Unlock()
	r.sendCtl(fr[:])
}

// carrierGoneLocked: a carrier ended. Unless both sides had retired it, what it carried may be
// lost, so everything the peer has not acknowledged goes out again.
func (r *rotConn) carrierGoneLocked(c *rotCarrier) {
	if c.dead {
		return
	}
	c.dead = true
	for i, x := range r.carriers {
		if x == c {
			r.carriers = append(r.carriers[:i:i], r.carriers[i+1:]...)
			break
		}
	}
	if r.active == c {
		r.active = nil
	}
	if !(c.sentRetire && c.gotRetire) {
		r.sendPos = r.ackedOff
	}
	if len(r.carriers) == 0 && r.emptySince.IsZero() {
		r.emptySince = time.Now()
	}
	_ = c.conn.Close()
	r.ensureSparesLocked()
	r.cond.Broadcast()
}

// ---- net.Conn

func (r *rotConn) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if len(r.rbuf) > 0 {
			n := copy(p, r.rbuf)
			r.rbuf = r.rbuf[n:]
			if len(r.rbuf) == 0 {
				r.rbuf = nil
			}
			r.cond.Broadcast()
			return n, nil
		}
		if len(p) == 0 {
			return 0, nil
		}
		if r.remoteClosed {
			return 0, io.EOF
		}
		if r.closed {
			return 0, r.errLocked()
		}
		if r.rdl.expired() {
			return 0, os.ErrDeadlineExceeded
		}
		r.cond.Wait()
	}
}

func (r *rotConn) errLocked() error {
	if r.err != nil {
		return r.err
	}
	return net.ErrClosed
}

func (r *rotConn) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if r.closed {
			return 0, r.errLocked()
		}
		if r.wdl.expired() {
			return 0, os.ErrDeadlineExceeded
		}
		if r.nextOff-r.ackedOff <= rotMaxUnacked {
			break
		}
		r.cond.Wait()
	}
	if r.nextOff == r.ackedOff {
		r.progress = time.Now()
	}
	r.buf = append(r.buf, p...)
	r.nextOff += uint64(len(p))
	r.cond.Broadcast()
	return len(p), nil
}

func (r *rotConn) Close() error {
	// what was written goes out first
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		r.mu.Lock()
		done := r.closed || r.sendPos >= r.nextOff || len(r.carriers) == 0
		r.mu.Unlock()
		if done {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	c := r.active
	r.mu.Unlock()
	if c != nil {
		_ = c.write([]byte{rotClose})
	}
	// the carriers close a moment later: a close at once can reset the connection and take the
	// last bytes with it
	r.failAfter(nil, 150*time.Millisecond)
	return nil
}

// fail ends the session; err is what later calls return (nil: closed by us).
func (r *rotConn) fail(err error) { r.failAfter(err, 0) }

func (r *rotConn) failAfter(err error, linger time.Duration) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed, r.err = true, err
	cs := r.carriers
	r.carriers = nil
	close(r.stop)
	r.cond.Broadcast()
	r.mu.Unlock()
	closeAll := func() {
		for _, c := range cs {
			_ = c.conn.Close()
		}
	}
	if linger > 0 {
		time.AfterFunc(linger, closeAll)
	} else {
		closeAll()
	}
	if r.opt.OnClose != nil {
		r.opt.OnClose()
	}
}

func (r *rotConn) LocalAddr() net.Addr  { r.mu.Lock(); defer r.mu.Unlock(); return r.local }
func (r *rotConn) RemoteAddr() net.Addr { r.mu.Lock(); defer r.mu.Unlock(); return r.remote }

func (r *rotConn) setDeadline(d *rotDeadline, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d.t = t
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if !t.IsZero() {
		if dur := time.Until(t); dur > 0 {
			d.timer = time.AfterFunc(dur, func() { r.mu.Lock(); r.cond.Broadcast(); r.mu.Unlock() })
		}
	}
	r.cond.Broadcast()
}

func (r *rotConn) SetDeadline(t time.Time) error {
	r.setDeadline(&r.rdl, t)
	r.setDeadline(&r.wdl, t)
	return nil
}
func (r *rotConn) SetReadDeadline(t time.Time) error  { r.setDeadline(&r.rdl, t); return nil }
func (r *rotConn) SetWriteDeadline(t time.Time) error { r.setDeadline(&r.wdl, t); return nil }

// ---- attach

func rotAttachFrame(id [16]byte, flags byte) []byte {
	b := make([]byte, 18)
	b[0] = rotAttach
	copy(b[1:], id[:])
	b[17] = flags
	return b
}

// readRotAttach reads the ATTACH frame the peeked first byte announced.
func readRotAttach(br *bufio.Reader) (id [16]byte, flags byte, err error) {
	var b [18]byte
	if _, err = io.ReadFull(br, b[:]); err != nil {
		return id, 0, err
	}
	copy(id[:], b[1:17])
	return id, b[17], nil
}

// peekedConn is conn with the bytes already read from it through br given back.
type peekedConn struct {
	net.Conn
	br *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.br.Read(b) }
