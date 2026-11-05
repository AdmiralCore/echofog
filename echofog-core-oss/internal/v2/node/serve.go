package node

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// beforeAdd is a test seam that runs right before an accepted session enters the table.
var beforeAdd func(*session.Established)

// acceptLoop accepts carriers from one listener and turns each completed handshake into a table
// entry and a service loop. Refusals are ordinary protocol behaviour (a hidden node closes
// silently, an expired card gets REJECT), so they are not runtime errors.
func (r *Runtime) acceptLoop(ctx context.Context, l carrier.Listener) {
	defer r.wg.Done()
	for ctx.Err() == nil {
		local, err := r.mainLocal(ctx)
		if err != nil {
			if ctx.Err() == nil {
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
		est, err := session.Accept(ctx, l, cfg)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return
		case errors.Is(err, session.ErrPathBound):
			continue
		default:
			r.log.Debug("node: accepted carrier refused", "err", err)
			continue
		}

		if est.Access == nil || r.accessMgr == nil {
			r.takeAccepted(ctx, local, est, nil, nil)
			continue
		}
		// A hidden node books the pass (redeem or renew) before the session enters the table, off
		// the accept loop: a slow store write of one contact must not stop the others from being
		// accepted (T3.6b). A pass that cannot be booked is not kept: fail closed.
		if r.Background() == BackgroundSuspended {
			// A pass is not spent while suspended: the peer is told to come back later.
			_ = est.GoAway(ctx, frame.CodeGoingAway, "background-suspended", r.cfg.RetryAfter)
			continue
		}
		select {
		case r.admitSem <- struct{}{}:
		case <-ctx.Done():
			_ = est.Session.Close()
			return
		}
		r.wg.Add(1)
		go func(local session.LocalIdentity, est *session.Established) {
			defer r.wg.Done()
			defer func() { <-r.admitSem }()
			adm, card, err := r.admit(ctx, est)
			if err != nil {
				r.log.Debug("node: access session refused", "err", err)
				_ = est.Session.Close()
				return
			}
			r.takeAccepted(ctx, local, est, &adm, card)
			if err := r.RefreshAccess(ctx); err != nil {
				// The pass is booked and the session is valid; a failed listener refresh must
				// not drop it. The next RefreshAccess retries from the same persistent state.
				r.log.Warn("node: access refresh after a session", "err", err)
			}
		}(local, est)
	}
}

// takeAccepted enters an accepted (and, on a hidden node, already admitted) session into the
// table and starts serving it. adm is non-nil for a session booked with a pass; such a session
// receives card, the peer card of the contact (handshake.md section 5).
func (r *Runtime) takeAccepted(ctx context.Context, local session.LocalIdentity, est *session.Established, adm *admission, card []byte) {
	if adm != nil {
		r.markAdmitted(est, *adm)
	}
	if beforeAdd != nil {
		beforeAdd(est)
	}
	kept, err := r.table.Add(local, est)
	if err != nil {
		r.forgetAdmitted(est)
		_ = est.Session.Close()
		r.log.Warn("node: session table rejected an accepted session", "err", err)
		return
	}
	if kept != est {
		// Add drains the loser by the duplicate rule of wire.md section 7.3.
		r.forgetAdmitted(est)
		return
	}
	if r.Background() == BackgroundSuspended {
		// suspended closes sessions with GOAWAY (platforms.md section 2); a peer that dialed us
		// during the suspension is told to come back later.
		r.forgetAdmitted(est)
		_ = kept.GoAway(ctx, frame.CodeGoingAway, "background-suspended", r.cfg.RetryAfter)
		return
	}
	if adm != nil && len(card) > 0 {
		if err := est.PushCard(card); err != nil && !errors.Is(err, mux.ErrFeatureNotSelected) {
			// The peer card also travels in P2 on later sessions; a push failure is not fatal.
			r.log.Debug("node: access peer card push failed", "err", err)
		}
	}
	r.samplePeer(kept)
	r.ensureServing(kept)
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
		defer r.forgetAdmitted(est)
		peer := r.peerOf(est)
		r.noteSession(peer.Card)
		r.notePeerSession(peer, est.Remote)
		r.wg.Add(1)
		go r.greaseLoop(est.Session)
		go r.datagramPump(est, peer)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.pex.Run(r.ctx, est.Session, r.cfg.Profile.LowPower)
		}()
		r.serveSession(est, peer)
	}()
}

// notePeerSession records a working session in the pex candidate table: the device becomes tried,
// and the 24-hour PEX confirmation is refreshed when the session used an address of its card.
func (r *Runtime) notePeerSession(p Peer, remote netip.AddrPort) {
	if r.peers == nil || p.Card == nil {
		return
	}
	r.peers.NoteSession(p.Card, remote, r.clock.Now())
}

// ownPublicCard returns the node's current public card, verified against the corrected clock, or
// nil when the node has no public card (D37).
func (r *Runtime) ownPublicCard(now time.Time) *objects.Card {
	ic, err := r.cfg.Cards.Current(context.Background(), objects.ScopePublic)
	if err != nil {
		return nil
	}
	c, err := objects.VerifyCard(ic.Envelope, now)
	if err != nil {
		return nil
	}
	return c
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
