package node

import (
	"context"
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// Datagram is one application datagram delivered to a port, with the authenticated peer of the
// session it arrived on.
type Datagram struct {
	Peer    Peer
	Type    uint64
	Payload []byte
}

// DatagramPort is a module's view of datagram types it owns (registries.md §2: types belong to
// services, not to sessions). Send resolves or dials the peer; Receive fans the registered types
// in from every live session.
type DatagramPort interface {
	Send(ctx context.Context, peer DeviceID, typ uint64, payload []byte) error
	Receive(ctx context.Context) (Datagram, error)
	Close() error
}

// ErrDatagramType: the port did not register the type (types are claimed at Datagrams call).
var ErrDatagramType = errors.New("node: datagram type is not registered for this port")

// ErrDatagramClosed is returned by a port after Close.
var ErrDatagramClosed = errors.New("node: datagram port closed")

// Datagrams claims the datagram types for a module and returns its port. Passing no types claims
// every type, which is only useful for a router module.
func (r *Runtime) Datagrams(types ...uint64) DatagramPort {
	return r.dgrams.add(types)
}

// datagramPump delivers datagrams of one session to the registered ports. Unknown types are
// dropped (registries.md §2); the pump ends with the session.
func (r *Runtime) datagramPump(est *session.Established, peer Peer) {
	for {
		d, err := est.Session.ReceiveDatagram(r.ctx)
		if err != nil {
			return
		}
		r.dgrams.deliver(peer, d)
	}
}

type datagramBroker struct {
	rt    *Runtime
	mu    sync.Mutex
	ports map[*datagramPort]struct{}
	byTyp map[uint64]map[*datagramPort]struct{}
	all   map[*datagramPort]struct{}
	done  bool
}

func newDatagramBroker(rt *Runtime) *datagramBroker {
	return &datagramBroker{
		rt:    rt,
		ports: map[*datagramPort]struct{}{},
		byTyp: map[uint64]map[*datagramPort]struct{}{},
		all:   map[*datagramPort]struct{}{},
	}
}

func (b *datagramBroker) add(types []uint64) *datagramPort {
	p := &datagramPort{
		runtime: b.rt,
		broker:  b,
		ch:      make(chan Datagram, 64),
		types:   map[uint64]struct{}{},
		all:     len(types) == 0,
		done:    make(chan struct{}),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		close(p.done)
		return p
	}
	b.ports[p] = struct{}{}
	if len(types) == 0 {
		b.all[p] = struct{}{}
	}
	for _, t := range types {
		m := b.byTyp[t]
		if m == nil {
			m = map[*datagramPort]struct{}{}
			b.byTyp[t] = m
		}
		m[p] = struct{}{}
		p.types[t] = struct{}{}
	}
	return p
}

func (b *datagramBroker) remove(p *datagramPort) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.ports[p]; !ok {
		return
	}
	delete(b.ports, p)
	delete(b.all, p)
	for t := range p.types {
		if m := b.byTyp[t]; m != nil {
			delete(m, p)
			if len(m) == 0 {
				delete(b.byTyp, t)
			}
		}
	}
}

func (b *datagramBroker) deliver(peer Peer, d mux.Datagram) {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	targets := make([]*datagramPort, 0, len(b.all)+len(b.byTyp[d.Type]))
	seen := map[*datagramPort]struct{}{}
	for p := range b.all {
		seen[p] = struct{}{}
		targets = append(targets, p)
	}
	for p := range b.byTyp[d.Type] {
		if _, ok := seen[p]; ok {
			continue
		}
		targets = append(targets, p)
	}
	b.mu.Unlock()

	msg := Datagram{Peer: peer, Type: d.Type, Payload: append([]byte(nil), d.Payload...)}
	for _, p := range targets {
		select {
		case p.ch <- msg:
		default:
			// Datagrams are unreliable: a slow consumer loses, it never blocks the session.
		}
	}
}

func (b *datagramBroker) close() {
	b.mu.Lock()
	if b.done {
		b.mu.Unlock()
		return
	}
	b.done = true
	ports := make([]*datagramPort, 0, len(b.ports))
	for p := range b.ports {
		ports = append(ports, p)
	}
	b.mu.Unlock()
	for _, p := range ports {
		_ = p.Close()
	}
}

type datagramPort struct {
	runtime *Runtime
	broker  *datagramBroker
	ch      chan Datagram
	types   map[uint64]struct{}
	all     bool
	once    sync.Once
	done    chan struct{}
}

func (p *datagramPort) Send(ctx context.Context, peer DeviceID, typ uint64, payload []byte) error {
	if _, ok := p.types[typ]; !ok && !p.all {
		return ErrDatagramType
	}
	select {
	case <-p.done:
		return ErrDatagramClosed
	default:
	}
	est, _, err := p.brokerEstablished(ctx, peer)
	if err != nil {
		return err
	}
	return est.Session.SendDatagram(typ, payload)
}

func (p *datagramPort) Receive(ctx context.Context) (Datagram, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case d := <-p.ch:
		return d, nil
	case <-p.done:
		return Datagram{}, ErrDatagramClosed
	case <-ctx.Done():
		return Datagram{}, ctx.Err()
	}
}

func (p *datagramPort) Close() error {
	p.once.Do(func() {
		p.broker.remove(p)
		close(p.done)
	})
	return nil
}

// brokerEstablished finds the runtime that owns the port through the broker. The port keeps no
// back pointer to keep the public surface small; Send is a rare path and reuses Node.Host anyway.
func (p *datagramPort) brokerEstablished(ctx context.Context, peer DeviceID) (*session.Established, Peer, error) {
	if p.runtime == nil {
		return nil, Peer{}, ErrClosed
	}
	return p.runtime.established(ctx, peer, DialOpt{})
}
