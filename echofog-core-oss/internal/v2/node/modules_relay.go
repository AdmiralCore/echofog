package node

import (
	"context"
	"net/netip"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/relay"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// relaysForOthers is the platform policy of relay/2 (platforms.md §2): a node serves circuits for
// others only with the relay role, and never on a metered network or on a phone (the mobile
// profile has no background relaying and a small memory budget).
func relaysForOthers(p Profile) bool {
	return p.HasRole(RoleRelay) && !p.Metered && !p.Mobile()
}

// relayLimits returns the relay limits of the profile: mem_cap_mb allows at most 16 circuits.
func relayLimits(p Profile) relay.Limits { return relay.Limits{MemCapMB: p.MemCapMB} }

// registerRelay registers relay/2 (T6.1) when the profile allows relaying for others. Otherwise the
// service is not offered and a peer's CONNECT gets UNSUPPORTED like for any service the node lacks.
func (r *Runtime) registerRelay() {
	if !relaysForOthers(r.cfg.Profile) {
		return
	}
	spec, ok := service.Builtin(service.Relay)
	if !ok {
		return
	}
	srv := relay.NewServer(relay.ServerConfig{Directory: relayDirectory{r}, Limits: relayLimits(r.cfg.Profile)})
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, r.relayPeer(p, nil), st)
	})
	port := r.Datagrams(frame.DgramRelay)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer port.Close()
		for {
			d, err := port.Receive(r.ctx)
			if err != nil {
				return
			}
			srv.HandleDatagram(r.relayPeer(d.Peer, r.sendDatagramTo(d.Peer.SID)), d.Payload)
		}
	}()
}

// relayPeer describes the authenticated session of the sender; nothing in it comes from a frame.
func (r *Runtime) relayPeer(p Peer, send func(uint64, []byte) error) relay.Peer {
	if send == nil {
		send = r.sendDatagramTo(p.SID)
	}
	return relay.Peer{Node: [32]byte(p.Node), Device: [32]byte(p.Device), SID: p.SID,
		Remote: p.Remote, SendDatagram: send}
}

func (r *Runtime) sendDatagramTo(sid [16]byte) func(uint64, []byte) error {
	return func(typ uint64, payload []byte) error {
		est := r.sessionBySID(sid)
		if est == nil {
			return ErrNoSession
		}
		return est.Session.SendDatagram(typ, payload)
	}
}

func (r *Runtime) sessionBySID(sid [16]byte) *session.Established {
	if r.table == nil {
		return nil
	}
	for _, est := range r.table.Sessions() {
		if est != nil && est.SID == sid && est.Session != nil {
			return est
		}
	}
	return nil
}

// relayDirectory is R's view of its connected nodes: the live sessions of the session table.
type relayDirectory struct{ r *Runtime }

func (d relayDirectory) Lookup(node [32]byte) (relay.Target, bool) {
	if d.r.table == nil {
		return nil, false
	}
	for _, est := range d.r.table.Sessions() {
		if est == nil || est.Peer == nil || est.Session == nil || est.Peer.NodeID != node {
			continue
		}
		select {
		case <-est.Session.Done():
			continue
		default:
		}
		if est.Session.GoingAway() {
			continue
		}
		return relayTarget{d.r, est}, true
	}
	return nil, false
}

type relayTarget struct {
	r   *Runtime
	est *session.Established
}

func (t relayTarget) OpenStream(ctx context.Context) (*mux.Stream, error) {
	return t.est.Session.OpenStream(ctx, service.Relay, 2)
}

// Listed is false for unlisted nodes: they are never served by node ID (services.md §6.3).
func (t relayTarget) Listed() bool { return t.est.Peer.Flags&objects.FlagUnlisted == 0 }

func (t relayTarget) Peer() relay.Peer {
	var remote netip.AddrPort = t.est.Remote
	return relay.Peer{Node: t.est.Peer.NodeID, Device: t.est.Peer.DeviceID, SID: t.est.SID, Remote: remote,
		SendDatagram: func(typ uint64, p []byte) error { return t.est.Session.SendDatagram(typ, p) }}
}

// RelayCandidates lists the nodes this node could use as R: its live sessions, with the card
// flags of each (relay.Choose keeps the relay-flag ones). Relays learned through PEX become
// candidates once a session to them exists; pex asks specifically for relay cards while none is
// known (pex.Service.Ask).
func (r *Runtime) RelayCandidates() []relay.Candidate {
	if r.table == nil {
		return nil
	}
	var out []relay.Candidate
	for _, est := range r.table.Sessions() {
		if est == nil || est.Peer == nil || est.Session == nil {
			continue
		}
		select {
		case <-est.Session.Done():
			continue
		default:
		}
		out = append(out, relay.Candidate{Node: est.Peer.NodeID, Device: est.Peer.DeviceID,
			Flags: est.Peer.Flags, Connected: !est.Session.GoingAway()})
	}
	return out
}
