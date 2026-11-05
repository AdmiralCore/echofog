package session

import (
	"context"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/shape"
)

// DialOption changes one Dial call. The zero options keep the production behaviour: a carrier
// from carrier.Dial, GREASE on (D18), the system clock.
type DialOption func(*dialConfig)

type dialConfig struct {
	carrier    mux.Carrier
	carrierFn  func(context.Context) (mux.Carrier, error)
	used       bool
	expected   *[32]byte
	clock      carrier.Clock
	grease     bool
	offset     time.Duration
	verifier   handshake.CardVerifier
	bindings   handshake.KeyBinder
	control    *Control
	timers     mux.Timers
	policy     PolicyFunc
	dialConfig carrier.DialConfig
	pathKind   mux.PathKind
	lowPower   bool
	idlePad    *shape.Profile
	idleRand   *mrand.Rand
	onEst      func(*mux.Session)
	onFail     func(mux.PathFailure)
}

// WithCarrier supplies the carrier directly (relay circuits, tests). It is used once: a suite
// retry after REJECT{suite} needs a new carrier and then fails with ErrConfig.
func WithCarrier(c mux.Carrier) DialOption {
	return func(o *dialConfig) { o.carrier = c }
}

// WithCarrierFunc supplies a carrier factory, so the one permitted retry after REJECT{suite}
// runs on a fresh carrier.
func WithCarrierFunc(fn func(context.Context) (mux.Carrier, error)) DialOption {
	return func(o *dialConfig) { o.carrierFn = fn }
}

// WithExpectedDeviceID sets the responder device ID expected in XX without a card (bootstrap
// lists, handshake §4.2). It sets the rid binding of §3.3.
func WithExpectedDeviceID(id [32]byte) DialOption {
	return func(o *dialConfig) { o.expected = &id }
}

// WithClock injects the clock used for every handshake deadline and for card checks (AD13).
func WithClock(c carrier.Clock) DialOption {
	return func(o *dialConfig) { o.clock = c }
}

// WithGrease turns the GREASE value in HS_HELLO, features and services on or off (default on,
// D18). It exists for vectors and for a peer that cannot ignore GREASE.
func WithGrease(on bool) DialOption {
	return func(o *dialConfig) { o.grease = on }
}

// WithOffset applies a learned clock offset to every timestamp and deadline (T3.8).
func WithOffset(d time.Duration) DialOption {
	return func(o *dialConfig) { o.offset = d }
}

// WithVerifier replaces the card verifier (default handshake.DefaultCardVerifier).
func WithVerifier(v handshake.CardVerifier) DialOption {
	return func(o *dialConfig) { o.verifier = v }
}

// WithBindings sets the hs-key-to-device binding table the handshake checks (handshake §7).
func WithBindings(b handshake.KeyBinder) DialOption {
	return func(o *dialConfig) { o.bindings = b }
}

// WithControl installs the CARD_UPDATE handler (objects.md §3.6). A session table passes its
// own handler here.
func WithControl(c *Control) DialOption {
	return func(o *dialConfig) { o.control = c }
}

// WithTimers replaces the mux session timers (tests and platform profiles). Zero fields keep
// the specification defaults.
func WithTimers(t mux.Timers) DialOption {
	return func(o *dialConfig) { o.timers = t }
}

// WithPolicy builds the mux stream policy from the negotiated services. The runtime passes
// service.Registry.Policy so service-local framing knows its per-type limits (T3.7a).
func WithPolicy(p PolicyFunc) DialOption {
	return func(o *dialConfig) { o.policy = p }
}

// WithDialConfig passes carrier dial options (registry, network memory, clock) through.
func WithDialConfig(c carrier.DialConfig) DialOption {
	return func(o *dialConfig) { o.dialConfig = c }
}

// WithPathKind sets the kind of the carrier this dial creates: direct (default) or relay
// (wire.md §8). The relay suite itself is T6.1; tests and relay circuits pass PathRelay here.
func WithPathKind(kind mux.PathKind) DialOption {
	return func(o *dialConfig) { o.pathKind = kind }
}

// WithLowPower marks the platform as low_power (platforms.md §2): no standby paths, the
// [40, 55] s keepalive, and no idle-padding feature offered.
func WithLowPower(on bool) DialOption {
	return func(o *dialConfig) { o.lowPower = on }
}

// WithIdlePadding supplies the shape profile whose idle_pad_ms drives PADDING-only frames when
// the idle-padding feature was negotiated (wire.md §6.4, §9). r is for deterministic tests.
func WithIdlePadding(profile *shape.Profile, r *mrand.Rand) DialOption {
	return func(o *dialConfig) { o.idlePad, o.idleRand = profile, r }
}

// WithOnEstablished receives the mux session as soon as it exists, before Dial returns. The
// runtime registers it in its PATH_BIND binder here, closing the window in which a peer could
// bind a second path before the session was known (wire.md §8.2).
func WithOnEstablished(f func(*mux.Session)) DialOption {
	return func(o *dialConfig) { o.onEst = f }
}

// WithOnPathFailed receives every path failure of the session (wire.md §8.3). The path policy
// uses it to release carrier slots and start a backoff.
func WithOnPathFailed(f func(mux.PathFailure)) DialOption {
	return func(o *dialConfig) { o.onFail = f }
}

func newDialConfig(opts []DialOption) *dialConfig {
	o := &dialConfig{grease: true, clock: systemClock{}, pathKind: mux.PathDirect}
	for _, opt := range opts {
		opt(o)
	}
	if o.clock == nil {
		o.clock = systemClock{}
	}
	return o
}

// Dial opens a session to the peer whose card is card, on attempt (from carrier.Plan). With a
// card it runs XKhfs; without one it runs XXhfs, which needs a throwaway identity unless an
// expected device ID is supplied (handshake §4.2).
//
// After a REJECT{suite} it retries exactly once on a fresh carrier with the full original
// offered list and the best suite of the intersection (handshake §3.1).
func Dial(ctx context.Context, card *objects.Card, attempt carrier.Attempt, local LocalIdentity, opts ...DialOption) (*Established, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	o := newDialConfig(opts)
	clk := o.clock

	var remote *handshake.CardInfo
	if card != nil {
		info, err := verifyRemote(card, clk.Now().Add(o.offset), o.verifier)
		if err != nil {
			return nil, err
		}
		remote = info
	}
	if local.Device == nil {
		return nil, fmt.Errorf("%w: no local device", ErrConfig)
	}
	if len(local.Card) == 0 {
		return nil, fmt.Errorf("%w: no local card for message 3", ErrConfig)
	}

	var offered []uint16
	for try := 0; ; try++ {
		c, err := o.open(ctx, card, attempt)
		if err != nil {
			return nil, err
		}
		est, next, err := dialOnce(ctx, c, card, remote, attempt, local, o, offered)
		if next != nil {
			_ = c.Close()
			if try >= 1 {
				// Exactly one retry after REJECT{suite} (handshake §3.1); err carries the
				// rejection the second responder sent.
				return nil, err
			}
			offered = next
			continue
		}
		if err != nil {
			_ = c.Close()
		}
		return est, err
	}
}

// open returns the carrier: the injected one, the injected factory, or carrier.Dial.
func (o *dialConfig) open(ctx context.Context, card *objects.Card, attempt carrier.Attempt) (mux.Carrier, error) {
	if o.carrierFn != nil {
		return o.carrierFn(ctx)
	}
	if o.carrier != nil {
		if o.used {
			return nil, fmt.Errorf("%w: no carrier left for a suite retry", ErrConfig)
		}
		o.used = true
		return o.carrier, nil
	}
	cfg := o.dialConfig
	if cfg.Clock == nil {
		cfg.Clock = o.clock
	}
	c, _, err := carrier.Dial(ctx, carrier.DialRequest{Card: card, Addr: attempt.Addr}, []carrier.Attempt{attempt}, cfg)
	return c, err
}

// dialOnce runs one handshake on one carrier. A non-nil next means the peer sent REJECT{suite}
// and the caller must try once more with the returned suite preference.
func dialOnce(ctx context.Context, c mux.Carrier, card *objects.Card, remote *handshake.CardInfo,
	attempt carrier.Attempt, local LocalIdentity, o *dialConfig, offered []uint16) (*Established, []uint16, error) {
	clk := o.clock
	static, err := staticKey(local.Device)
	if err != nil {
		return nil, nil, err
	}
	cfg := handshake.InitiatorConfig{
		Static:           static,
		Card:             local.Card,
		Remote:           remote,
		ExpectedDeviceID: o.expected,
		Suites:           local.Suites,
		Offered:          offered,
		Features:         FeaturesFor(local.Features, o.lowPower),
		Services:         local.Services,
		AccessSecret:     local.AccessSecret,
		Now:              clk.Now,
		Offset:           o.offset,
		Grease:           o.grease,
		Throwaway:        local.Throwaway,
		Cards:            o.verifier,
		Bindings:         o.bindings,
	}
	cb := c.ChannelBinding()
	ini, err := handshake.NewInitiator(cfg, cb[:])
	if err != nil {
		return nil, nil, err
	}
	g := mux.NewGuard(c)
	raw, err := c.OpenStream(ctx)
	if err != nil {
		return nil, nil, err
	}
	if raw.ID() != 0 {
		return nil, nil, fmt.Errorf("%w: the handshake must run on stream 0, got %d", ErrConfig, raw.ID())
	}

	hello, msg1, err := ini.Start()
	if err != nil {
		return nil, nil, err
	}
	if err := writeFlight(raw, clk, hello, msg1); err != nil {
		return nil, nil, err
	}

	rd := frame.NewReader(raw, nil)
	whole := clk.Now().Add(handshake.HandshakeTimeout)
	for {
		f, err := readFrame(rd, raw, messageDeadline(clk, whole))
		if err != nil {
			return nil, nil, wrapSilent(err)
		}
		switch f.Type {
		case frame.TypeHsRetry:
			h, m, err := ini.HandleRetryContext(ctx, f.Payload)
			if err != nil {
				return nil, nil, wrapSilent(err)
			}
			if err := writeFlight(raw, clk, h, m); err != nil {
				return nil, nil, err
			}
		case frame.TypeReject:
			rej, perr := handshake.ParseReject(f.Payload)
			hello2, _, err := ini.HandleReject(f.Payload)
			if err != nil {
				if perr == nil {
					return nil, nil, rejection(*rej)
				}
				return nil, nil, wrapSilent(err)
			}
			// The next flight must run on a fresh carrier: the responder closed this one. Resend
			// the complete original offered list, GREASE value included; only the best common
			// suite moves to the front (B3).
			h1, err := handshake.ParseHello(hello)
			if err != nil {
				return nil, nil, wrapSilent(err)
			}
			h2, err := handshake.ParseHello(hello2)
			if err != nil {
				return nil, nil, wrapSilent(err)
			}
			return nil, preferSuite(h1.Offered, h2.Chosen), rejection(*rej)
		case frame.TypeHsMsg:
			msg3, hs, err := ini.HandleMsg2(f.Payload)
			if err != nil {
				return nil, nil, mapHandshakeError(err)
			}
			if err := writeFrame(raw, clk, frame.TypeHsMsg, msg3); err != nil {
				return nil, nil, err
			}
			est, err := finish(c, g, raw, hs, RoleInitiator, o.pathKind, local, remoteOfCard(attempt.Addr),
				o.control, o.timers, o.policy, o.lowPower,
				newIdlePadding(o.idlePad, o.idleRand, o.lowPower), o.onEst, o.onFail)
			return est, nil, err
		default:
			return nil, nil, wrapSilent(fmt.Errorf("unexpected %s before the handshake completed", frame.TypeName(f.Type)))
		}
	}
}

func writeFlight(w mux.RawStream, clk carrier.Clock, hello, msg1 []byte) error {
	if err := w.SetWriteDeadline(clk.Now().Add(handshake.MessageTimeout)); err != nil {
		return err
	}
	buf := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeHsHello, Payload: hello})
	buf = frame.AppendFrame(buf, frame.Frame{Type: frame.TypeHsMsg, Payload: msg1})
	_, err := w.Write(buf)
	return err
}

// preferSuite moves chosen to the front, keeping the rest in the original order.
func preferSuite(suites []uint16, chosen uint16) []uint16 {
	out := make([]uint16, 0, len(suites))
	out = append(out, chosen)
	for _, s := range suites {
		if s != chosen {
			out = append(out, s)
		}
	}
	return out
}
