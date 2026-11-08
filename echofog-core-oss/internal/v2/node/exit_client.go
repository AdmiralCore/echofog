package node

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/exit"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// exitIdentityNS is the store namespace of the per-exit client identities, keyed by the exit's
// device ID (services.md §7). The values are random seeds, never derived from the mnemonic.
const exitIdentityNS = "v2/exit/client"

// exitIdent is the client identity under which one exit knows this node.
type exitIdent struct {
	tw    *identity.Throwaway
	local session.LocalIdentity
}

// exitIdentity returns the client identity for an exit: loaded from the store, or drawn and
// stored on first use. The exit sees a random account and device and a one-device list with a
// minimal unlisted peer card; nothing links it to the messenger identity.
func (r *Runtime) exitIdentity(ctx context.Context, exitDev DeviceID) (*exitIdent, error) {
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	if id := r.exitIdents[exitDev]; id != nil {
		return id, nil
	}
	kv := r.cfg.Store.Store(exitIdentityNS)
	var seeds identity.ThrowawaySeeds
	defer seeds.Wipe()
	raw, err := kv.Get(exitDev[:])
	switch {
	case err == nil:
		if len(raw) != 4*32 {
			return nil, fmt.Errorf("node: exit identity record is %d bytes", len(raw))
		}
		copy(seeds.Account[:], raw[0:32])
		copy(seeds.Device[:], raw[32:64])
		copy(seeds.Handshake[:], raw[64:96])
		copy(seeds.Mask[:], raw[96:128])
	case errors.Is(err, store.ErrNotFound):
		if seeds, err = identity.NewThrowawaySeeds(); err != nil {
			return nil, fmt.Errorf("node: exit identity: %w", err)
		}
		raw = nil
		raw = append(raw, seeds.Account[:]...)
		raw = append(raw, seeds.Device[:]...)
		raw = append(raw, seeds.Handshake[:]...)
		raw = append(raw, seeds.Mask[:]...)
		if err := kv.Put(exitDev[:], raw); err != nil {
			return nil, fmt.Errorf("node: store the exit identity: %w", err)
		}
	default:
		return nil, fmt.Errorf("node: load the exit identity: %w", err)
	}
	tw, err := identity.ThrowawayFromSeeds(seeds)
	if err != nil {
		return nil, fmt.Errorf("node: restore the exit identity: %w", err)
	}
	local, err := r.exitLocal(ctx, tw)
	if err != nil {
		tw.Destroy()
		return nil, err
	}
	id := &exitIdent{tw: tw, local: local}
	if r.exitIdents == nil {
		r.exitIdents = map[DeviceID]*exitIdent{}
	}
	r.exitIdents[exitDev] = id
	return id, nil
}

// exitLocal builds the local identity of a session to an exit: the throwaway keys, a minimal
// unlisted peer card with a one-device list, and an offer of the exit service only, so the exit
// learns nothing of what else this node speaks.
func (r *Runtime) exitLocal(ctx context.Context, tw *identity.Throwaway) (session.LocalIdentity, error) {
	devices, err := tw.DeviceList(1, r.clock.Now())
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: exit identity device list: %w", err)
	}
	b, err := cards.New(cards.Config{
		Account:    tw.Account.Public(),
		Device:     tw.Device,
		HsKey:      tw.Device.Handshake().Public(),
		Role:       objects.RoleUser,
		Devices:    devices,
		Visibility: cards.Hidden,
		Clock:      r.clock,
	})
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: exit identity card: %w", err)
	}
	peer, err := b.Current(ctx, objects.ScopePeer)
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: exit identity card: %w", err)
	}
	features := append([]uint32(nil), r.cfg.Features...)
	if r.cfg.Profile.LowPower {
		features = withoutFeature(features, idlePaddingFeature)
	}
	var offers []service.Offer
	for _, o := range r.services.Offer(false) {
		if o.ID == service.Exit {
			offers = append(offers, o)
		}
	}
	return session.LocalIdentity{
		Device:   tw.Device,
		Card:     peer.Envelope,
		Features: features,
		Services: handshakeServices(offers),
	}, nil
}

// destroyExitIdentities wipes the keys of the loaded exit identities.
func (r *Runtime) destroyExitIdentities() {
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	for k, id := range r.exitIdents {
		id.tw.Destroy()
		delete(r.exitIdents, k)
	}
}

// ExitIdentityIDs returns the node and device IDs the given exit sees for this client. They are
// stable across runs and differ from the messenger identity and from every other exit's.
func (r *Runtime) ExitIdentityIDs(ctx context.Context, exitDevice DeviceID) (NodeID, DeviceID, error) {
	if !r.running() {
		return NodeID{}, DeviceID{}, ErrClosed
	}
	id, err := r.exitIdentity(ctx, exitDevice)
	if err != nil {
		return NodeID{}, DeviceID{}, err
	}
	return NodeID(id.tw.Account.NodeID()), DeviceID(id.tw.Device.ID()), nil
}

// ExitClient returns the exit/2 client of one exit. Its sessions are dialed under the per-exit
// client identity (services.md §7); opt supplies the carrier of tests and relay circuits. The
// client pads the first records of its tunnels with the shape profile of the exit's address (H1).
// Close the client when done: it ends the datagram port.
func (r *Runtime) ExitClient(ctx context.Context, card *objects.Card, opt DialOpt) (*exit.Client, error) {
	if card == nil || card.ExitOffer() == nil {
		return nil, fmt.Errorf("%w: not an exit", ErrPeerCard)
	}
	if !r.running() {
		return nil, ErrClosed
	}
	// This node opens exit/2 as a client: the registry must know the service to offer it in the
	// handshake and to open a stream of it.
	spec, ok := service.Builtin(service.Exit)
	if !ok {
		return nil, errors.New("node: exit/2 is not built in")
	}
	for v := range spec.Versions {
		r.services.Use(service.Exit, v)
	}
	dev := DeviceID(card.DeviceID)
	id, err := r.exitIdentity(ctx, dev)
	if err != nil {
		return nil, err
	}
	local := id.local
	opt.Card, opt.Local = card, &local
	port := r.Datagrams(frame.DgramExitUDP)
	cl, err := exit.NewClient(exit.ClientConfig{
		OpenStream: func(ctx context.Context) (*mux.Stream, error) {
			_, st, err := r.Open(ctx, dev, service.Exit, opt)
			return st, err
		},
		SendDatagram: func(typ uint64, payload []byte) error {
			est := r.exitSession(dev, id)
			if est == nil {
				return ErrNoSession
			}
			return est.Session.SendDatagram(typ, payload)
		},
		Shape: exitAddrShape(card),
		// A failure to reach an exit without a cover is told apart from a plain missing path.
		NoCamouflage: !exit.CardHasCamouflage(card),
		Clock:        r.carrierClock,
		OnClose:      func() { _ = port.Close() },
	})
	if err != nil {
		_ = port.Close()
		return nil, err
	}
	if !r.track() {
		_ = port.Close()
		return nil, ErrClosed
	}
	go func() {
		defer r.wg.Done()
		for {
			d, err := port.Receive(r.ctx)
			if err != nil {
				return
			}
			// Only the session under this exit's identity: the datagram port fans in every session.
			if d.Peer.Device == dev && d.Peer.Local.Device != nil && d.Peer.Local.Device.ID() == id.tw.Device.ID() {
				cl.HandleDatagram(d.Payload)
			}
		}
	}()
	return cl, nil
}

// exitSession is the live session to the exit under the exit identity, or nil. A datagram never
// dials: a dead session is the stream's business.
func (r *Runtime) exitSession(dev DeviceID, id *exitIdent) *session.Established {
	table := r.table
	if table == nil {
		return nil
	}
	est := table.Lookup(id.tw.Device.ID(), [32]byte(dev))
	if est == nil || est.Session == nil {
		return nil
	}
	select {
	case <-est.Session.Done():
		return nil
	default:
	}
	return est
}

// exitAddrShape is the shape profile an exit publishes with its address. Addresses of one exit
// share a profile in practice, so the first one that names a profile wins. An exit that names
// none is padded with the built-in default: H1 is never silently off.
func exitAddrShape(card *objects.Card) *shape.Profile {
	for _, a := range card.Addrs() {
		if len(a.GetParams()) == 0 {
			continue
		}
		var sp pb.SuiteParams
		if err := proto.Unmarshal(a.GetParams(), &sp); err != nil || sp.GetShape() == nil {
			continue
		}
		if p, err := shape.Parse(sp.GetShape()); err == nil {
			return p
		}
	}
	p, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		return nil
	}
	return p
}

// exitShape is the shape profile the exit server pads its tunnels with: the profile of its
// address (З2 of the T10.1 acceptance). An explicit ExitShapeProfile wins, then the profile of
// the node's own listeners, then the built-in default; padding is never silently off. Use the
// "none" profile to turn it off on purpose.
func exitShape(cfg Config) *shape.Profile {
	if cfg.ExitShapeProfile != nil {
		return cfg.ExitShapeProfile
	}
	for _, src := range cfg.ListenerSources {
		switch l := src.(type) {
		case Noise3Listener:
			if l.Profile != nil {
				return l.Profile
			}
		case *Noise3Listener:
			if l != nil && l.Profile != nil {
				return l.Profile
			}
		}
	}
	p, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		return nil
	}
	return p
}

// ErrNoRemote says the session to the exit carries no address of the other end (an in-memory
// carrier of a test).
var ErrNoRemote = errors.New("node: the session to the exit has no remote address")

// ExitRemote makes sure the session to the exit under its client identity is up and returns the
// address of its first hop: the exit itself, or the relay in the path. The kill switch of the
// whole-computer mode permits this address and no other (T10.6b).
func (r *Runtime) ExitRemote(ctx context.Context, card *objects.Card) (netip.AddrPort, error) {
	return netip.AddrPort{}, errors.New("not implemented")
}
