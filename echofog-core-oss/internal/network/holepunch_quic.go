package network

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// The punching half of a direct connection through two NATs. The idea and the pacing come from
// go-libp2p's QUIC transport (p2p/transport/quic/transport.go, holePunch; MIT License, Copyright
// (c) 2014 Juan Batiz-Benet): one side sends small random UDP packets from the very socket QUIC
// listens on, so its NAT opens a mapping toward the other side, while that other side dials
// normally. Each side's packets (the random ones, and the dialer's QUIC Initial retransmissions)
// open its own NAT for the other, and the handshake goes through. See THIRD_PARTY_NOTICES.md.

var (
	// ErrHolePunchTimeout: no connection from the peer appeared in time.
	ErrHolePunchTimeout = errors.New("hole punch timed out")
	// ErrPunchActive: a punch toward this peer and address is already running.
	ErrPunchActive = errors.New("already punching a hole toward this peer")
)

const (
	punchPayloadLen = 64
	// punchTimeout bounds one attempt (libp2p uses 5 s for QUIC).
	punchTimeout = 5 * time.Second
)

type punchKey struct {
	addr string
	peer types.PeerID
}

type arrivalWaiters struct {
	mu      sync.Mutex
	waiters map[types.PeerID]map[chan struct{}]struct{}
	active  map[punchKey]struct{}
}

func (a *arrivalWaiters) watch(peer types.PeerID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	a.mu.Lock()
	if a.waiters == nil {
		a.waiters = make(map[types.PeerID]map[chan struct{}]struct{})
	}
	if a.waiters[peer] == nil {
		a.waiters[peer] = make(map[chan struct{}]struct{})
	}
	a.waiters[peer][ch] = struct{}{}
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		delete(a.waiters[peer], ch)
		if len(a.waiters[peer]) == 0 {
			delete(a.waiters, peer)
		}
		a.mu.Unlock()
	}
}

// notify wakes everybody waiting for a connection from peer.
func (a *arrivalWaiters) notify(peer types.PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.waiters[peer] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *arrivalWaiters) begin(k punchKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		a.active = make(map[punchKey]struct{})
	}
	if _, ok := a.active[k]; ok {
		return false
	}
	a.active[k] = struct{}{}
	return true
}

func (a *arrivalWaiters) end(k punchKey) {
	a.mu.Lock()
	delete(a.active, k)
	a.mu.Unlock()
}

// PunchHole sends random packets to addr from the QUIC socket until an authenticated connection
// from peer shows up, or ctx / the attempt timeout ends. The other side must dial this node at the
// same time (see DialDirect); on success the connection arrives through ConnChan like any other.
func (q *QuicTransport) PunchHole(ctx context.Context, addr string, peer types.PeerID) error {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	key := punchKey{addr: ua.String(), peer: peer}
	if !q.arrivals.begin(key) {
		return ErrPunchActive
	}
	defer q.arrivals.end(key)

	arrived, cancel := q.arrivals.watch(peer)
	defer cancel()

	ctx, stop := context.WithTimeout(ctx, punchTimeout)
	defer stop()

	payload := make([]byte, punchPayloadLen)
	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C
	for i := 0; ; i++ {
		if _, err := rand.Read(payload); err != nil {
			return err
		}
		if _, err := q.tr.WriteTo(payload, ua); err != nil {
			return fmt.Errorf("send punch packet: %w", err)
		}
		// quick at first, slowing to a few packets a second: the first packets race the other
		// side's dial, the later ones keep the mapping open while the handshake completes
		maxSleep := min(10*(i+1)*(i+1), 200)
		timer.Reset(10*time.Millisecond + time.Duration(mrand.IntN(maxSleep))*time.Millisecond) // #nosec G404 -- pacing jitter
		select {
		case <-arrived:
			return nil
		case <-timer.C:
		case <-ctx.Done():
			select { // it may have arrived in the same instant
			case <-arrived:
				return nil
			default:
			}
			return ErrHolePunchTimeout
		}
	}
}

// DialDirect dials addr over QUIC only (no TCP fallback). It is the dialing half of a hole punch:
// the QUIC handshake retransmits its first packet while the other side punches toward us.
func (q *QuicTransport) DialDirect(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, punchTimeout)
	defer cancel()
	return q.DialEarly(ctx, addr)
}

func (q *QuicTransport) arrivalNotify(peer types.PeerID) { q.arrivals.notify(peer) }
