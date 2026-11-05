package node

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// DHTNamespace is the store namespace of the DHT routing table (AD13: persistent state lives in
// the store).
const DHTNamespace = "v2/dht/routing"

// registerDHT opens the DHT routing table (T5.1a) and starts its maintenance loop. The dht/2
// service itself (T5.1b) and the lookup client (T5.2) register here too when they are merged.
//
// The table is fed by every authenticated session: serve.go calls noteSession with the verified
// peer card, so an entry's address can only come from the node's own signed card (dht.md §5).
func (r *Runtime) registerDHT() {
	var persistent KV
	if r.cfg.Store != nil {
		persistent = r.cfg.Store.Store(DHTNamespace)
	}
	tbl, err := dht.Open(dht.Config{
		Self:     dht.DeviceID(r.identity.DeviceID),
		Store:    persistent,
		Now:      r.clock.Now,
		LowPower: r.cfg.Profile.LowPower,
		MemCapMB: r.cfg.Profile.MemCapMB,
		Pinger:   dhtPinger{h: r},
		Log:      r.log,
	})
	if err != nil {
		r.log.Warn("node: dht routing table", "err", err)
		return
	}
	r.mu.Lock()
	r.dht = tbl
	r.mu.Unlock()
	tbl.Start(r.ctx)
}

// noteSession adds the card of an authenticated session to the routing table. A hidden node's
// unlisted card is dropped by the table itself.
func (r *Runtime) noteSession(card *objects.Card) {
	if card == nil {
		return
	}
	r.mu.Lock()
	tbl := r.dht
	r.mu.Unlock()
	if tbl != nil {
		tbl.AddFromSession(card)
	}
}

// RoutingTable returns the DHT routing table for the dht/2 server (T5.1b) and the lookup client
// (T5.2). It is nil before Start.
func (r *Runtime) RoutingTable() *dht.Table {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dht
}

// dhtPinger checks liveness over the session layer: it reuses a live session when there is one
// and otherwise dials the card, then pings the control stream.
type dhtPinger struct{ h *Runtime }

func (p dhtPinger) Ping(ctx context.Context, card *objects.Card) error {
	if p.h == nil || card == nil {
		return errors.New("node: dht ping without a card")
	}
	_, sess, err := p.h.Session(ctx, DeviceID(card.DeviceID), DialOpt{Card: card})
	if err != nil {
		if errors.Is(err, ErrBackground) {
			// A suspended node does not dial; that says nothing about the peer.
			return dht.ErrUnavailable
		}
		return err
	}
	_, err = sess.Ping(ctx)
	return err
}
