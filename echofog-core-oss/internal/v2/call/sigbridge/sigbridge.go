// Package sigbridge connects the call layer's signals (v1 envelopes, package v1sig) to the call/2
// signalling service (T9.3b): it sends the call layer's signals as signed CallSignal, forwards
// flooded signals byte for byte, admits joins through the JoinGuard, and hands verified inbound
// signals to the call layer as v1 envelopes.
package sigbridge

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/call/v1sig"
)

// ErrNoWrapper: a flooded signal of another originator can only be forwarded as received, and the
// received wrapper is no longer known (or never was): the bridge never signs in another's name.
var ErrNoWrapper = errors.New("sigbridge: no received wrapper to forward")

// ErrSpoof: a signal that is not forwarded names a sender other than this node.
var ErrSpoof = errors.New("sigbridge: signal in another sender's name")

const (
	wrapperTTL = 30 * time.Second
	maxCache   = 4096
	maxJoined  = 4096
)

// Service is the part of *call.Service the bridge uses.
type Service interface {
	Send(ctx context.Context, p call.Peer, m *pb.CallSignal) error
	Forward(ctx context.Context, p call.Peer, raw []byte) error
}

// Config wires a Bridge.
type Config struct {
	// Self is the local device ID: the call layer's own peer ID.
	Self    [32]byte
	Service Service
	// Guard admits joins. Nil admits every well-formed join.
	Guard *call.JoinGuard
	// Deliver hands a verified inbound signal, as a v1 envelope, to the call layer; from is the
	// neighbour it came from.
	Deliver func(from [32]byte, v1 []byte)
	Context context.Context
	Now     func() time.Time
}

// Bridge is the signal path between the call layer and call/2.
type Bridge struct {
	cfg Config

	mu      sync.Mutex
	wrapper map[wkey]wentry
	order   []wkey
	joined  map[pair]struct{}
	jorder  []pair
}

type wkey struct {
	sender [32]byte
	msgID  uint64
}

type wentry struct {
	raw []byte
	at  time.Time
}

type pair struct {
	device [32]byte
	salt   [call.JoinSaltLen]byte
}

// New returns a bridge.
func New(cfg Config) *Bridge {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	return &Bridge{cfg: cfg, wrapper: map[wkey]wentry{}, joined: map[pair]struct{}{}}
}

// SendSignal sends a v1 envelope the call layer produced to a neighbour. lossy copies are skipped:
// call/2 duplicates lossy signals on CALL_SIGNAL itself when it sends the reliable one.
func (b *Bridge) SendSignal(to [32]byte, lossy bool, payload []byte) error {
	if lossy {
		return nil
	}
	m, err := v1sig.ToPB(payload)
	if err != nil {
		return err
	}
	peer := call.Peer{Device: to}
	if !bytes.Equal(m.Sender, b.cfg.Self[:]) {
		if !call.IsForwarded(m.Action) {
			return ErrSpoof
		}
		var k wkey
		copy(k.sender[:], m.Sender)
		k.msgID = m.MsgId
		raw, ok := b.wrapperOf(k)
		if !ok {
			return ErrNoWrapper
		}
		return b.cfg.Service.Forward(b.cfg.Context, peer, raw)
	}
	return b.cfg.Service.Send(b.cfg.Context, peer, m)
}

func (b *Bridge) wrapperOf(k wkey) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.wrapper[k]
	if !ok || b.cfg.Now().Sub(e.at) > wrapperTTL {
		return nil, false
	}
	return e.raw, true
}

func (b *Bridge) remember(k wkey, raw []byte) {
	now := b.cfg.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.wrapper[k]; ok {
		return
	}
	for len(b.order) > 0 && (len(b.order) >= maxCache || now.Sub(b.wrapper[b.order[0]].at) > wrapperTTL) {
		delete(b.wrapper, b.order[0])
		b.order = b.order[1:]
	}
	b.wrapper[k] = wentry{raw: append([]byte(nil), raw...), at: now}
	b.order = append(b.order, k)
}

func (b *Bridge) markJoined(p pair) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.joined[p]; ok {
		return
	}
	for len(b.jorder) >= maxJoined {
		delete(b.joined, b.jorder[0])
		b.jorder = b.jorder[1:]
	}
	b.joined[p] = struct{}{}
	b.jorder = append(b.jorder, p)
}

func (b *Bridge) isJoined(p pair) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.joined[p]
	return ok
}

// OnSignal receives a verified call/2 signal (call.Config.OnSignal). It does not block: a join
// waits in the guard's uniform refusal delay on its own goroutine.
func (b *Bridge) OnSignal(ctx context.Context, p call.Peer, m *pb.CallSignal, raw []byte) {
	v1, err := v1sig.ToV1(m)
	if err != nil {
		return // an unmappable signal is dropped; the service already counted it as verified
	}
	if call.IsForwarded(m.Action) {
		var k wkey
		copy(k.sender[:], m.Sender)
		k.msgID = m.MsgId
		b.remember(k, raw)
	}
	deliver := func() { b.cfg.Deliver(p.Device, v1) }
	switch m.Action {
	case pb.Action_ACTION_JOIN:
		j := m.GetJoin()
		var pr pair
		copy(pr.device[:], m.Sender)
		copy(pr.salt[:], j.GetJoinSalt())
		if b.cfg.Guard == nil {
			b.markJoined(pr)
			deliver()
			return
		}
		go func() {
			if err := b.cfg.Guard.Admit(ctx, m.CallId, pr.device, j, m.Timestamp); err != nil {
				return
			}
			b.markJoined(pr)
			deliver()
		}()
	case pb.Action_ACTION_MEMBER_HEARTBEAT:
		if b.cfg.Guard != nil {
			var pr pair
			copy(pr.device[:], m.Sender)
			copy(pr.salt[:], m.GetJoin().GetJoinSalt())
			if !b.isJoined(pr) {
				return // a heartbeat never admits a member: only a fresh join does
			}
		}
		deliver()
	default:
		deliver()
	}
}
