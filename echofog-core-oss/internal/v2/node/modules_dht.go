package node

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
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
		// The paths that accept a session feed the table; they must not wait for a liveness check
		// of a full bucket (T5.1b-dht200).
		AsyncChecks: true,
		Log:         r.log,
	})
	if err != nil {
		r.log.Warn("node: dht routing table", "err", err)
		return
	}
	srv, err := dht.NewServer(dht.ServerConfig{
		Self:         dht.DeviceID(r.identity.DeviceID),
		Table:        tbl,
		Reached:      r.peers.Serve,
		ReleaseCheck: r.cfg.ReleaseCheck,
		Now:          r.clock.Now,
		MemCapMB:     r.cfg.Profile.MemCapMB,
		Log:          r.log,
	})
	if err != nil {
		r.log.Warn("node: dht server", "err", err)
		return
	}
	r.mu.Lock()
	r.dht = tbl
	r.dhtServer = srv
	r.mu.Unlock()
	r.startDHTClient(tbl)
	tbl.Start(r.ctx)

	// A node without the dht-server role does not answer other nodes' dht/2 requests, but it
	// still searches (T5.2) and learns routing entries. Without a registered handler the service
	// answers UNSUPPORTED (services.md §1).
	if r.cfg.Profile.HasRole(RoleDHTServer) {
		if spec, ok := service.Builtin(service.DHT); ok {
			r.Register(spec, r.handleDHT)
		}
	} else {
		// Offered as a client so that a server selects dht/2 for our own streams.
		r.services.Use(service.DHT, dht.Version)
	}
}

// handleDHT adapts the authenticated peer of a dht/2 stream to the storing server.
func (r *Runtime) handleDHT(ctx context.Context, p Peer, st *mux.Stream) {
	r.mu.Lock()
	srv := r.dhtServer
	r.mu.Unlock()
	if srv == nil {
		_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
		return
	}
	peer := dht.Peer{Device: [32]byte(p.Device), SID: p.SID}
	if p.Card != nil && p.Card.Role() == objects.RoleServer {
		peer.Server = true
	}
	// A relayed session shows the relay's address, not the publisher's: only a direct path says
	// which network the publisher is in. Anything else counts as a unit of its own.
	if p.Path == mux.PathDirect && p.Remote.IsValid() {
		peer.Unit = dht.UnitOf(p.Remote.Addr())
	}
	srv.Handle(ctx, peer, st)
}

// DHTServer returns the storing side of dht/2 (T5.1b), for the lookup client (T5.2) to cache
// popular records. It is nil before Start.
func (r *Runtime) DHTServer() *dht.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dhtServer
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

// DHT returns the lookup and publishing client of dht/2 (T5.2): Get, Put, FindNode and the
// node-ID resolver. It is nil before Start.
func (r *Runtime) DHT() *dht.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dhtClient
}

// startDHTClient builds the lookup client over the routing table and starts its republication
// loop. The table's start-up self-lookup and bucket refreshes use the same client.
func (r *Runtime) startDHTClient(tbl *dht.Table) {
	var memory KV
	if r.cfg.Store != nil {
		memory = r.cfg.Store.Store(DHTMemoryNamespace)
	}
	cl, err := dht.NewClient(dht.ClientConfig{
		Self:      dht.DeviceID(r.identity.DeviceID),
		Table:     tbl,
		Transport: dhtTransport{r: r},
		Memory:    memory,
		Now:       r.clock.Now,
		Own:       r.ownDHTRecords,
		Log:       r.log,
	})
	if err != nil {
		r.log.Warn("node: dht client", "err", err)
		return
	}
	r.mu.Lock()
	r.dhtClient = cl
	r.mu.Unlock()
	tbl.SetLookup(cl.Lookup)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		cl.Run(r.ctx)
	}()
}

// DHTMemoryNamespace is the store namespace of the DHT reader memory (dht.md §4.2, AD13).
const DHTMemoryNamespace = "v2/dht/seen"

// ownDHTRecords are the records this node publishes by itself: its public card, if it has one
// (a node without a cover, or a hidden one, has none: D37). Other records (prekeys, device list,
// mailbox hints) are registered by their owners with Client.Publish.
func (r *Runtime) ownDHTRecords() [][]byte {
	raw, ok := r.cards.Own(objects.ScopePublic)
	if !ok {
		return nil
	}
	return [][]byte{raw}
}

// dhtTransport sends dht/2 requests over the session layer: one stream per request, on a live
// session when there is one and otherwise on a new one (the dial also gives the routing table
// the card of the peer, serve.go).
type dhtTransport struct{ r *Runtime }

func (t dhtTransport) open(ctx context.Context, to *objects.Card) (*mux.Stream, error) {
	if to == nil {
		return nil, errors.New("node: dht query without a card")
	}
	opt := DialOpt{Card: to}
	if hook := t.r.dhtDialHook; hook != nil {
		opt = hook(to)
	}
	_, st, err := t.r.Open(ctx, DeviceID(to.DeviceID), service.DHT, opt)
	return st, err
}

// roundTrip sends one request and reads its answer within RequestTimeout.
func (t dhtTransport) roundTrip(ctx context.Context, to *objects.Card, typ uint64, req uint64, payload []byte) (frame.Frame, error) {
	st, err := t.open(ctx, to)
	if err != nil {
		return frame.Frame{}, err
	}
	defer st.Close()
	if err := st.WriteFrame(typ, 0, payload); err != nil {
		return frame.Frame{}, err
	}
	deadline := time.Now().Add(dht.RequestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = st.SetReadDeadline(deadline)
	return st.ReadFrame()
}

func newReqID() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("node: no randomness: " + err.Error())
	}
	return binary.BigEndian.Uint64(b[:])
}

func (t dhtTransport) FindNode(ctx context.Context, to *objects.Card, target [32]byte) ([][]byte, error) {
	req := newReqID()
	payload, err := dht.MarshalFindNode(req, target)
	if err != nil {
		return nil, err
	}
	f, err := t.roundTrip(ctx, to, dht.TypeFindNode, req, payload)
	if err != nil {
		return nil, err
	}
	return dht.DecodeNodes(req, f.Type, f.Payload)
}

func (t dhtTransport) FindValue(ctx context.Context, to *objects.Card, key [32]byte) ([]byte, [][]byte, error) {
	req := newReqID()
	payload, err := dht.MarshalFindValue(req, key)
	if err != nil {
		return nil, nil, err
	}
	f, err := t.roundTrip(ctx, to, dht.TypeFindValue, req, payload)
	if err != nil {
		return nil, nil, err
	}
	return dht.DecodeValue(req, f.Type, f.Payload)
}

func (t dhtTransport) Store(ctx context.Context, to *objects.Card, record []byte) (uint32, error) {
	req := newReqID()
	payload, err := dht.MarshalStore(req, record)
	if err != nil {
		return 0, err
	}
	f, err := t.roundTrip(ctx, to, dht.TypeStore, req, payload)
	if err != nil {
		return 0, err
	}
	return dht.DecodeStored(req, f.Type, f.Payload)
}
