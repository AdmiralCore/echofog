package node

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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
	if ctx == nil {
		ctx = context.Background()
	}
	card := opt.Card
	if card == nil {
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
		return r.dial(ctx, card, local, opt, extra)
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
	if opt.Local != nil {
		return *opt.Local, nil
	}
	return r.mainLocal(ctx)
}

// dial runs one session.Dial with the runtime's clock, bindings, policy and carrier plan. The
// plan is raced with happy eyeballs (carrier.Dial); the first carrier is handed to session.Dial,
// and a suite retry after REJECT{suite} dials the plan again on a fresh carrier.
func (r *Runtime) dial(ctx context.Context, card *objects.Card, local session.LocalIdentity, opt DialOpt, extra []session.DialOption) (*session.Established, error) {
	attempts := opt.Attempts
	if len(attempts) == 0 {
		attempts = carrier.Plan(card, r.localInfo)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, DeviceID(card.DeviceID))
	}

	req := carrier.DialRequest{Card: card, DialContext: r.cfg.DialContext}
	dialCfg := carrier.DialConfig{Registry: r.registry, Clock: r.carrierClock}

	attempt := firstAttempt(attempts)
	var first mux.Carrier
	switch {
	case opt.Carrier != nil:
		first = opt.Carrier
	case opt.CarrierFunc != nil:
		// The factory is called by session.Dial itself.
	default:
		c, chosen, err := carrier.Dial(ctx, req, attempts, dialCfg)
		if err != nil {
			r.maybeClockCheck()
			return nil, fmt.Errorf("node: carrier dial: %w", err)
		}
		first, attempt = c, chosen
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
		c, _, err := carrier.Dial(ctx, req, attempts, dialCfg)
		return c, err
	}

	opts := make([]session.DialOption, 0, len(extra)+5)
	opts = append(opts,
		session.WithClock(r.carrierClock),
		session.WithOffset(r.clock.Offset()),
		session.WithBindings(r.bindings),
		session.WithPolicy(r.policyFunc),
		session.WithCarrierFunc(carrierFn),
	)
	opts = append(opts, extra...)
	if opt.ExpectedDeviceID != nil {
		opts = append(opts, session.WithExpectedDeviceID([32]byte(*opt.ExpectedDeviceID)))
	}

	est, err := session.Dial(ctx, card, attempt, local, opts...)
	if err != nil {
		r.maybeClockCheck()
		return nil, err
	}
	// The initiator may send on a new stream the moment message 3 is written, while the responder
	// is still finishing the handshake and has not attached its Guard to the session yet. A PING
	// on the control stream is answered only after that attach, so it is a deterministic
	// readiness barrier before the caller opens service streams.
	if est.Role == session.RoleInitiator {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, perr := est.Session.Ping(waitCtx)
		cancel()
		if perr != nil {
			_ = est.Session.Close()
			return nil, fmt.Errorf("node: session readiness: %w", perr)
		}
	}
	r.samplePeer(est)
	return est, nil
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
