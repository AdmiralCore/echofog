package node

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// Session returns a live session to the peer device, dialing it when the table has none. opt
// carries the peer card (when the caller already has one), an alternate local identity or a
// one-shot carrier for tests and relay circuits.
func (r *Runtime) Session(ctx context.Context, peer DeviceID, opt DialOpt) (Peer, *mux.Session, error) {
	est, p, err := r.established(ctx, peer, opt)
	if err != nil {
		return Peer{}, nil, err
	}
	return p, est.Session, nil
}

// Open returns a stream of the selected version of a service (service.Open). It dials the peer
// when needed and starts serving the session in both directions.
func (r *Runtime) Open(ctx context.Context, peer DeviceID, svc uint64, opt DialOpt) (Peer, *mux.Stream, error) {
	for try := 0; ; try++ {
		est, p, err := r.established(ctx, peer, opt)
		if err != nil {
			return Peer{}, nil, err
		}
		pol := r.services.Policy(toChoices(est.Services))
		st, err := service.Open(ctx, est.Session, pol, svc)
		if err == nil {
			return p, st, nil
		}
		// The table may have handed us a session that the duplicate rule or the idle sweeper
		// is draining (T3.2 risk 1). One retry redials through Table.Get, which drops an
		// unusable session.
		if try == 0 && (errors.Is(err, mux.ErrGoingAway) || errors.Is(err, mux.ErrSessionClosed) || errors.Is(err, mux.ErrPathFailed)) {
			continue
		}
		return Peer{}, nil, fmt.Errorf("node: open service %d: %w", svc, err)
	}
}

func (r *Runtime) established(ctx context.Context, peer DeviceID, opt DialOpt) (*session.Established, Peer, error) {
	if !r.running() {
		return nil, Peer{}, ErrClosed
	}
	if r.Background() == BackgroundSuspended {
		return nil, Peer{}, ErrBackground
	}
	if ctx == nil {
		ctx = context.Background()
	}
	card := opt.Card
	if card == nil {
		// A module may answer on a session the runtime already has, for example the echo
		// service replying to a datagram of the peer that just sent it. The card is only
		// needed to dial a new session, so reuse a live main-identity session first.
		if est := r.liveMainSession(peer); est != nil {
			r.ensureServing(est)
			return est, r.peerOf(est), nil
		}
		card = r.cardBook.Latest([32]byte(peer))
		if card == nil {
			return nil, Peer{}, fmt.Errorf("%w: %s", ErrNoPeerCard, peer)
		}
	}
	if card.DeviceID != [32]byte(peer) {
		return nil, Peer{}, fmt.Errorf("%w: card signer %x, want %s", ErrPeerCard, card.DeviceID, peer)
	}

	local, err := r.dialIdentity(ctx, opt)
	if err != nil {
		return nil, Peer{}, err
	}
	table := r.table
	if table == nil {
		return nil, Peer{}, ErrClosed
	}
	est, err := table.Get(ctx, local, [32]byte(peer), func(ctx context.Context, extra ...session.DialOption) (*session.Established, error) {
		if opt.ViaRelay && opt.Carrier == nil && opt.CarrierFunc == nil {
			return r.dialViaRelay(ctx, card, local, opt, extra)
		}
		est, err := r.dial(ctx, card, local, opt, extra)
		if err != nil && ctx.Err() == nil && relayFallback(err, opt) {
			viaRelay, rerr := r.dialViaRelay(ctx, card, local, opt, extra)
			if rerr == nil {
				return viaRelay, nil
			}
			if errors.Is(rerr, ErrNoDomesticEntry) || errors.Is(rerr, ErrRouteUnsupported) {
				return nil, rerr
			}
		}
		if err == nil && opt.Carrier == nil && opt.CarrierFunc == nil && opt.CarrierCard == nil {
			_, dest, dec := r.cardRoute(card)
			r.noteRoute(card.DeviceID, dec, dest, false)
		}
		return est, err
	})
	if err != nil {
		return nil, Peer{}, err
	}
	r.ensureServing(est)
	return est, r.peerOf(est), nil
}

// dialIdentity returns the local identity of the handshake: the main one, or the one supplied by
// the caller (a throwaway identity for XX to a bare address, an identity for one exit).
func (r *Runtime) dialIdentity(ctx context.Context, opt DialOpt) (session.LocalIdentity, error) {
	local := session.LocalIdentity{}
	if opt.Local != nil {
		local = *opt.Local
	} else {
		var err error
		local, err = r.mainLocal(ctx)
		if err != nil {
			return session.LocalIdentity{}, err
		}
	}
	if len(opt.AccessSecret) > 0 {
		local.AccessSecret = append([]byte(nil), opt.AccessSecret...)
	}
	return local, nil
}

// dial runs one session.Dial with the runtime's clock, bindings, policy and carrier plan. The
// plan is raced with happy eyeballs (carrier.Dial); the first carrier is handed to session.Dial,
// and a suite retry after REJECT{suite} dials the plan again on a fresh carrier.
func (r *Runtime) dial(ctx context.Context, card *objects.Card, local session.LocalIdentity, opt DialOpt, extra []session.DialOption) (*session.Established, error) {
	attempts := opt.Attempts
	// A bootstrap dial has no card: the carrier gets a shell that holds only the masking key of
	// the list, and the handshake runs XX (T5.5).
	carrierCard := card
	if opt.CarrierCard != nil {
		carrierCard, card = opt.CarrierCard, nil
	}
	if len(attempts) == 0 && card != nil {
		attempts = carrier.Plan(card, r.planInfo())
	}
	// The country policy (T6.4) drops the attempts the network class forbids. A relay circuit, an
	// injected carrier is not a direct dial to a card address. A bootstrap dial is one, to the address
	// of the list: it has no card, but the row for that address applies all the same (L6).
	var probeBudget int
	if (card != nil || opt.CarrierCard != nil) && opt.Carrier == nil && opt.CarrierFunc == nil {
		var perr error
		attempts, probeBudget, perr = r.applyRoutePolicy(card, attempts, opt.Probe)
		if perr != nil {
			return nil, perr
		}
	}
	if len(attempts) == 0 {
		if card == nil {
			return nil, ErrNoRoute
		}
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, DeviceID(card.DeviceID))
	}

	req := carrier.DialRequest{Card: carrierCard, DialContext: r.cfg.DialContext}
	if probeBudget > 0 {
		req.DialContext = r.probeDialContext(probeBudget)
	}
	dialCfg := carrier.DialConfig{Registry: r.registry, Clock: r.carrierClock}

	var reserved []session.Pair
	var chosenPair session.Pair
	havePair := false

	attempt := firstAttempt(attempts)
	var first mux.Carrier
	switch {
	case opt.Carrier != nil:
		first = opt.Carrier
	case opt.CarrierFunc != nil:
		// The factory is called by session.Dial itself. An injected carrier is not counted by
		// the H11 policy: it is not a TLS dial to the card's address.
	default:
		var rerr error
		attempts, reserved, rerr = r.reserveAttempts(attempts)
		if rerr != nil {
			return nil, rerr
		}
		attempt = firstAttempt(attempts)
		c, chosen, err := carrier.Dial(ctx, req, attempts, dialCfg)
		if err != nil {
			if ctx.Err() != nil {
				r.releaseReservations(reserved)
			} else {
				r.failReservations(reserved)
			}
			r.maybeClockCheck()
			return nil, fmt.Errorf("%w: %w", errCarrierDial, err)
		}
		first, attempt = c, chosen
		chosenPair = pairOfAttempt(chosen)
		havePair = true
		r.settleReservations(reserved, chosenPair)
	}

	provided := opt.Carrier
	fn := opt.CarrierFunc
	var once sync.Once
	carrierFn := func(ctx context.Context) (mux.Carrier, error) {
		switch {
		case provided != nil:
			var c mux.Carrier
			once.Do(func() { c, provided = provided, nil })
			if c != nil {
				return c, nil
			}
			return nil, fmt.Errorf("%w: no carrier left for a suite retry", session.ErrConfig)
		case fn != nil:
			return fn(ctx)
		}
		var c mux.Carrier
		once.Do(func() { c, first = first, nil })
		if c != nil {
			return c, nil
		}
		// The one suite retry stays on the chosen address/pair (H11): it is a fresh carrier for
		// the same handshake, not a new route.
		c, _, err := carrier.Dial(ctx, req, []carrier.Attempt{attempt}, dialCfg)
		return c, err
	}

	opts := make([]session.DialOption, 0, len(extra)+8)
	onEstablished, onPathFailed := r.pathHooks()
	opts = append(opts,
		session.WithClock(r.carrierClock),
		session.WithBindings(r.bindings),
		session.WithPolicy(r.policyFunc),
		session.WithCarrierFunc(carrierFn),
		session.WithPathKind(opt.PathKind),
		session.WithLowPower(r.cfg.Profile.LowPower),
		session.WithIdlePadding(r.cfg.ShapeProfile, nil),
		session.WithOnEstablished(onEstablished),
		session.WithOnPathFailed(onPathFailed),
	)
	opts = append(opts, extra...)
	if opt.ExpectedDeviceID != nil {
		opts = append(opts, session.WithExpectedDeviceID([32]byte(*opt.ExpectedDeviceID)))
	}

	est, err := session.Dial(ctx, card, attempt, local, opts...)
	if err != nil {
		r.maybeClockCheck()
		if havePair {
			if ctx.Err() == nil && penalizeDial(err) {
				r.policy.Penalize(chosenPair)
			} else {
				r.policy.Release(chosenPair)
			}
		}
		return nil, err
	}
	if havePair {
		r.policy.Succeeded(chosenPair)
		if !r.recordPair(est.SID, 0, chosenPair, est.Session.Done()) {
			r.policy.Release(chosenPair)
		}
	}
	if !est.Unverified {
		// The clock of an unauthenticated responder says nothing (handshake.md section 4.2).
		r.samplePeer(est)
	}
	return est, nil
}

// liveMainSession returns a live session of the main local identity to peer, without dialing.
// It lets a module answer on a session it did not open with an explicit card.
func (r *Runtime) liveMainSession(peer DeviceID) *session.Established {
	table := r.table
	if table == nil || r.cfg.Device == nil {
		return nil
	}
	local := r.cfg.Device.ID()
	for _, est := range table.Sessions() {
		if est == nil || est.Peer == nil || est.Session == nil {
			continue
		}
		if DeviceID(est.Peer.DeviceID) != peer || est.Local.Device == nil || est.Local.Device.ID() != local {
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
		return est
	}
	return nil
}

func firstAttempt(attempts []carrier.Attempt) carrier.Attempt {
	if len(attempts) == 0 {
		return carrier.Attempt{}
	}
	return attempts[0]
}

// maybeClockCheck runs the D36 check after every direct L0 attempt failed. The clock gates it to
// once per 10 minutes, and it never blocks the caller.
func (r *Runtime) maybeClockCheck() {
	if r.ctx == nil || r.ctx.Err() != nil {
		return
	}
	go func() {
		if _, err := r.clock.MaybeCheckCDN(r.ctx, true); err != nil &&
			!errors.Is(err, clock.ErrNoHTTPClient) && r.ctx.Err() == nil {
			r.log.Debug("node: clock check after a failed dial", "err", err)
		}
	}()
}
