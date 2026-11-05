package node

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// acceptLoop accepts carriers from one listener and turns each completed handshake into a table
// entry and a service loop. Refusals are ordinary protocol behaviour (a hidden node closes
// silently, an expired card gets REJECT), so they are not runtime errors.
func (r *Runtime) acceptLoop(l carrier.Listener) {
	defer r.wg.Done()
	for r.ctx.Err() == nil {
		local, err := r.mainLocal(r.ctx)
		if err != nil {
			if r.ctx.Err() == nil {
				r.listenerStopped(err)
			}
			return
		}
		onEstablished, onPathFailed := r.pathHooks()
		cfg := session.AcceptConfig{
			Local:         local,
			Hidden:        r.Self().Visibility != cards.Visible,
			Passes:        r.cfg.Passes,
			Puzzle:        r.cfg.Puzzle,
			Bindings:      r.bindings,
			Control:       r.control,
			Clock:         r.carrierClock,
			Offset:        r.clock.Offset(),
			Kind:          mux.PathDirect,
			Policy:        r.policyFunc,
			Verifier:      nil,
			Binder:        r.binder,
			LowPower:      r.cfg.Profile.LowPower,
			IdlePadding:   r.cfg.ShapeProfile,
			OnEstablished: onEstablished,
			OnPathFailed:  onPathFailed,
		}
		est, err := session.Accept(r.ctx, l, cfg)
		switch {
		case err == nil:
		case r.ctx.Err() != nil:
			return
		case errors.Is(err, session.ErrPathBound):
			continue
		default:
			r.log.Debug("node: accepted carrier refused", "err", err)
			continue
		}

		kept, err := r.table.Add(local, est)
		if err != nil {
			_ = est.Session.Close()
			r.log.Warn("node: session table rejected an accepted session", "err", err)
			continue
		}
		if kept != est {
			// Add drains the loser by the duplicate rule of wire.md §7.3.
			continue
		}
		if r.Background() == BackgroundSuspended {
			// suspended closes sessions with GOAWAY (platforms.md §2); a peer that dialed us
			// during the suspension is told to come back later.
			_ = kept.GoAway(r.ctx, frame.CodeGoingAway, "background-suspended", r.cfg.RetryAfter)
			continue
		}
		r.samplePeer(kept)
		r.ensureServing(kept)
	}
}

// ensureServing starts exactly one service loop and one datagram pump per live session. A
// session can be reached both by dialing and by accepting, so the guard is per *mux.Session.
func (r *Runtime) ensureServing(est *session.Established) {
	if est == nil || est.Session == nil {
		return
	}
	if _, loaded := r.serving.LoadOrStore(est.Session, struct{}{}); loaded {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.serving.Delete(est.Session)
		peer := r.peerOf(est)
		r.noteSession(peer.Card)
		r.wg.Add(1)
		go r.greaseLoop(est.Session)
		go r.datagramPump(est, peer)
		r.serveSession(est, peer)
	}()
}

// serveSession dispatches incoming service streams of one session to the modules.
func (r *Runtime) serveSession(est *session.Established, peer Peer) {
	for {
		st, err := est.Session.AcceptStream(r.ctx)
		if err != nil {
			return
		}
		go r.dispatch(st, peer)
	}
}

// dispatch maps one stream to its service handler. A pair that was not selected, or that this
// side does not serve, gets UNSUPPORTED and a normal FIN (wire.md §6.3); it is never abuse.
func (r *Runtime) dispatch(st *mux.Stream, peer Peer) {
	o := st.Open()
	var h Handler
	if st.Selected() {
		h = r.handlerFor(o.Service, o.Version)
	}
	if h == nil {
		_ = st.WriteFrame(frame.TypeUnsupported, 0, frame.Unsupported{Service: o.Service}.Append(nil))
		_ = st.Close()
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			// A service bug must not take the whole node down; the stream is closed with the
			// internal code and the other sessions keep running.
			r.log.Error("node: service handler panicked", "service", o.Service, "recover", rec)
			_ = st.CloseWithCode(frame.CodeInternal, "")
		}
	}()
	h(r.ctx, peer, st)
}

// peerOf builds the authenticated view of the session. The card was verified by the handshake;
// it is verified once more against the corrected clock so services get the parsed object without
// trusting the handshake struct.
func (r *Runtime) peerOf(est *session.Established) Peer {
	p := Peer{
		SID:    est.SID,
		Path:   est.Kind,
		Remote: est.Remote,
		Local:  est.Local,
	}
	if est.Peer == nil {
		return p
	}
	p.Node = NodeID(est.Peer.NodeID)
	p.Device = DeviceID(est.Peer.DeviceID)
	card, err := objects.VerifyCard(est.Peer.Raw, r.clock.Now())
	if err != nil {
		r.log.Warn("node: peer card no longer verifies", "device", p.Device, "err", err)
		return p
	}
	p.Card = card
	return p
}
