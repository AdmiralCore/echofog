package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/geoip"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Errors of the runtime.
var (
	// ErrConfig is a local wiring error (no identity, store or card builder).
	ErrConfig = errors.New("node: invalid configuration")
	// ErrClosed is returned after Stop.
	ErrClosed = errors.New("node: closed")
	// ErrNoPeerCard means the runtime has no verified card for the device, so it cannot dial.
	ErrNoPeerCard = errors.New("node: no card for the peer device")
	// ErrPeerCard is a card that does not match the requested device.
	ErrPeerCard = errors.New("node: peer card does not match the device")
	// ErrNoRoute means carrier.Plan found no usable direct attempt (relay is T6.1).
	ErrNoRoute = errors.New("node: no carrier route to the peer")
	// ErrNoSession means a path operation needs a live session to the peer.
	ErrNoSession = errors.New("node: no live session to the peer")
	// ErrTooManyPaths means the platform or the standby limit forbids another path now.
	ErrTooManyPaths = errors.New("node: too many paths to the peer")
	// ErrPathFeature means the peer did not negotiate path-migration.
	ErrPathFeature = errors.New("node: path-migration was not negotiated")
)

// Config assembles one runtime.
type Config struct {
	// Account and Device are the local identity. Required.
	Account *identity.Account
	Device  *identity.Device
	// Cards builds and remembers the node's own cards. Required.
	Cards *cards.CardBuilder
	// Store is the persistent store; the runtime opens its own namespaces in it. It does not
	// close the store: the owner keeps it across runtime restarts. Required.
	Store *store.Store

	// Clock is the corrected clock (T3.8). When nil the runtime creates one over HTTP.
	Clock *clock.Clock
	// HTTP is the single HTTP client of the node (02-architecture §3.1). Nil creates a plain
	// stdlib client; production supplies the browser-shaped, proxied one (T11.6).
	HTTP *http.Client
	// Log receives structured diagnostics. Nil means slog.Default.
	Log *slog.Logger
	// Events receives runtime events (D37 operator warnings, clock warnings). Nil logs them.
	Events EventSink
	// Profile is the platform profile and flags (platforms.md §2).
	Profile Profile

	// Listeners are already built carriers; ListenerSources are built at Start.
	Listeners       []carrier.Listener
	ListenerSources []ListenerSource
	// Registry replaces the carrier suite registry (tests, a build without a suite).
	Registry *carrier.Registry
	// CarrierClock is the raw carrier clock for tests (AD13). Nil means the system clock. When a
	// custom Clock is supplied, both must read the same raw time: the handshake adds
	// Clock.Offset() to CarrierClock.Now().
	CarrierClock carrier.Clock
	// LocalInfo feeds the D21 carrier plan (own public addresses, country lookup, UDP flag).
	LocalInfo carrier.LocalInfo
	// DialContext opens carrier sockets through the node's proxy (T11.6).
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)

	// Suites are the L1 suites the node offers or accepts. Nil means XK before XX.
	Suites []uint16
	// Features are the L2 features offered in handshakes (GREASE is added by service.Offer).
	Features []uint32
	// Passes resolves invitation and personal pass secrets for a hidden responder (T3.6).
	Passes handshake.PassStore
	// Puzzle enables HS_RETRY under load (T3.5).
	Puzzle *handshake.PuzzleConfig
	// RetryAfter is the GOAWAY retry_after used for runtime shutdown and idle closes, in seconds.
	RetryAfter uint32
	// Country is the offline GeoIP lookup; nil means internal/v2/geoip.
	Country func(netip.Addr) (string, bool)
	// ShapeProfile is the local shape profile used for idle PADDING when the idle-padding
	// feature is negotiated (wire.md §6.4, §9). Nil disables idle padding.
	ShapeProfile *shape.Profile
}

// Runtime is one running v2 node (02-architecture AD5).
type Runtime struct {
	cfg    Config
	log    *slog.Logger
	events EventSink
	clock  *clock.Clock
	http   *http.Client

	carrierClock carrier.Clock
	registry     *carrier.Registry
	services     *service.Registry
	localInfo    carrier.LocalInfo

	cardBook *session.CardBook
	bindings *session.KeyBindings
	abuse    *session.AbuseLog
	control  *session.Control
	binder   *mux.Binder
	policy   *session.PathPolicy

	// pathPairs maps a bound path to the carrier pair it was opened to (H11/H15). Only paths
	// this node opened as a carrier client are counted.
	pathsMu   sync.Mutex
	pathPairs map[pathKey]session.Pair

	identity Identity
	vis      cards.Visibility
	cards    cardSource

	mu        sync.Mutex
	handlers  map[[2]uint64]Handler
	table     *session.Table
	listeners []carrier.Listener
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	serving   sync.Map
	dgrams    *datagramBroker
	modules   sync.Once
	started   bool
	stopped   bool
	noCover   bool
}

var _ Host = (*Runtime)(nil)

// New validates the configuration and opens the persistent session state. Listeners are not
// started until Start.
func New(cfg Config) (*Runtime, error) {
	switch {
	case cfg.Account == nil:
		return nil, fmt.Errorf("%w: no account", ErrConfig)
	case cfg.Device == nil:
		return nil, fmt.Errorf("%w: no device", ErrConfig)
	case cfg.Cards == nil:
		return nil, fmt.Errorf("%w: no card builder", ErrConfig)
	case cfg.Store == nil:
		return nil, fmt.Errorf("%w: no store", ErrConfig)
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	events := cfg.Events
	if events == nil {
		events = logSink{log: log}
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	carrierClock := cfg.CarrierClock
	if carrierClock == nil {
		carrierClock = systemClock{}
	}

	r := &Runtime{
		cfg:          cfg,
		log:          log,
		events:       events,
		http:         httpClient,
		carrierClock: carrierClock,
		services:     service.NewRegistry(),
		handlers:     map[[2]uint64]Handler{},
		binder:       mux.NewBinder(),
		policy:       session.NewPathPolicy(session.PathPolicyConfig{Clock: carrierClock}),
		pathPairs:    map[pathKey]session.Pair{},
	}
	r.dgrams = newDatagramBroker(r)

	r.clock = cfg.Clock
	if r.clock == nil {
		c, err := clock.New(clock.Config{HTTP: httpClient, Warn: r.clockWarn})
		if err != nil {
			return nil, fmt.Errorf("node: clock: %w", err)
		}
		r.clock = c
	}

	reg, err := suiteRegistry(cfg, carrierClock)
	if err != nil {
		return nil, err
	}
	r.registry = reg

	r.cardBook, err = session.OpenCardBook(cfg.Store.Store(session.CardsNamespace))
	if err != nil {
		return nil, fmt.Errorf("node: card book: %w", err)
	}
	r.abuse, err = session.NewAbuseLog(cfg.Store.Store(session.AbuseNamespace))
	if err != nil {
		return nil, fmt.Errorf("node: abuse log: %w", err)
	}
	r.bindings, err = session.NewKeyBindings(cfg.Store.Store(session.BindingsNamespace), r.abuse)
	if err != nil {
		return nil, fmt.Errorf("node: key bindings: %w", err)
	}
	r.control = session.NewControl(session.ControlConfig{
		Cards:      r.cardBook,
		Abuse:      r.abuse,
		Clock:      r.carrierClock,
		OffsetFunc: r.clock.Offset,
	})

	r.vis = cfg.Cards.Visibility()
	r.identity = Identity{
		NodeID:     NodeID(cfg.Account.NodeID()),
		DeviceID:   DeviceID(cfg.Device.ID()),
		Account:    cfg.Account.Public(),
		Device:     cfg.Device,
		Visibility: r.vis,
	}
	r.cards = cardSource{builder: cfg.Cards, book: r.cardBook}

	r.localInfo = cfg.LocalInfo
	if r.localInfo.CountryOf == nil {
		r.localInfo.CountryOf = r.Country
	}
	return r, nil
}

// Start opens the session table, builds listeners, checks the D37 public-card condition and
// starts accepting. It returns after the listeners are up; sessions are served in the background.
func (r *Runtime) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return ErrClosed
	}
	if r.started {
		r.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	r.ctx, r.cancel = runCtx, cancel

	table, err := session.NewTable(session.TableConfig{
		MemCapMB:   r.cfg.Profile.MemCapMB,
		RetryAfter: r.cfg.RetryAfter,
		Clock:      r.carrierClock,
		Control:    r.control,
	})
	if err != nil {
		cancel()
		r.mu.Unlock()
		return fmt.Errorf("node: session table: %w", err)
	}
	r.table = table

	listeners := append([]carrier.Listener(nil), r.cfg.Listeners...)
	for i, src := range r.cfg.ListenerSources {
		if src == nil {
			closeListeners(listeners)
			_ = table.Close()
			cancel()
			r.mu.Unlock()
			return fmt.Errorf("%w: listener source %d is nil", ErrConfig, i)
		}
		l, lerr := src.Listen(runCtx)
		if lerr != nil {
			closeListeners(listeners)
			_ = table.Close()
			cancel()
			r.mu.Unlock()
			return fmt.Errorf("node: building listener %d: %w", i, lerr)
		}
		listeners = append(listeners, l)
	}
	r.listeners = listeners
	r.mu.Unlock()

	// D37: a visible node without a camouflaged listener has no public card. This is not a
	// startup error; the operator is told and the node keeps serving peers and invitees.
	if _, err := r.ownCards(ctx); err != nil {
		r.cleanupAfterStartFailure()
		return fmt.Errorf("node: own cards: %w", err)
	}

	r.modules.Do(r.registerModules)

	for _, l := range listeners {
		r.wg.Add(1)
		go r.acceptLoop(l)
	}

	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	return nil
}

// Stop announces shutdown, closes the listeners and stops the session table. It is idempotent and
// does not close the store: the owner decides whether the profile outlives the runtime.
func (r *Runtime) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	started := r.started
	cancel := r.cancel
	listeners := r.listeners
	table := r.table
	r.mu.Unlock()

	if !started {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	closeListeners(listeners)
	if table != nil {
		_ = table.Close()
	}
	r.dgrams.close()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) cleanupAfterStartFailure() {
	r.mu.Lock()
	listeners := r.listeners
	table := r.table
	r.mu.Unlock()
	closeListeners(listeners)
	if table != nil {
		_ = table.Close()
	}
	if r.cancel != nil {
		r.cancel()
	}
}

func closeListeners(ls []carrier.Listener) {
	for _, l := range ls {
		if l != nil {
			_ = l.Close()
		}
	}
}

func (r *Runtime) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started && !r.stopped
}

// Self returns the local identity.
func (r *Runtime) Self() Identity { return r.identity }

// Cards returns the node's card view for services.
func (r *Runtime) Cards() CardSource { return r.cards }

// Store returns one persistent namespace (T4.3).
func (r *Runtime) Store(ns string) KV { return r.cfg.Store.Store(ns) }

// Clock returns the corrected clock (T3.8).
func (r *Runtime) Clock() Clock { return r.clock }

// HTTP returns the node's single HTTP client (02-architecture §3.1).
func (r *Runtime) HTTP() *http.Client { return r.http }

// Profile returns the platform profile.
func (r *Runtime) Profile() Profile { return r.cfg.Profile }

// Events returns the runtime event sink.
func (r *Runtime) Events() EventSink { return r.events }

// Country maps an address to an ISO country using the offline database (wire.md §1 rule 3).
func (r *Runtime) Country(a netip.Addr) (string, bool) {
	if r.cfg.Country != nil {
		return r.cfg.Country(a)
	}
	return geoip.CountryOf(a)
}

// Devices returns the device IDs of an account from its newest verified device list.
func (r *Runtime) Devices(_ context.Context, node NodeID) ([]DeviceID, error) {
	dl := r.cardBook.DeviceList([32]byte(node))
	if dl == nil {
		return nil, fmt.Errorf("node: no device list for %s", node)
	}
	out := make([]DeviceID, 0, len(dl.Devices()))
	for _, d := range dl.Devices() {
		k, err := keys.Parse(d.GetKey())
		if err != nil {
			continue
		}
		if err := keys.CheckEd25519Public(k.Key()); err != nil {
			continue
		}
		out = append(out, DeviceID(k.ID()))
	}
	return out, nil
}

// Register adds one service version set and its handler to the runtime. It is the service
// registry of T3.7a over service.Spec (services.md §2–§9); the modules.go files call it.
func (r *Runtime) Register(spec service.Spec, h Handler) {
	if len(spec.Versions) == 0 {
		panic("node: service registered without versions")
	}
	if h == nil {
		panic("node: nil service handler")
	}
	for v := range spec.Versions {
		r.services.ServeSpec(spec, v, nil)
	}
	r.mu.Lock()
	for v := range spec.Versions {
		r.handlers[[2]uint64{spec.ID, v}] = h
	}
	r.mu.Unlock()
}

func (r *Runtime) handlerFor(svc, version uint64) Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handlers[[2]uint64{svc, version}]
}

// mainLocal builds the session identity of the main account from the current card material. It is
// called for every handshake so a card reissued by the builder (T4.2a) is used without a restart.
func (r *Runtime) mainLocal(ctx context.Context) (session.LocalIdentity, error) {
	peer, err := r.cfg.Cards.Current(ctx, objects.ScopePeer)
	if err != nil {
		return session.LocalIdentity{}, fmt.Errorf("node: peer card: %w", err)
	}
	own, err := r.ownCards(ctx)
	if err != nil {
		return session.LocalIdentity{}, err
	}
	suites := r.cfg.Suites
	if len(suites) == 0 {
		suites = append([]uint16(nil), handshake.DefaultSuites...)
	}
	return session.LocalIdentity{
		Device:   r.cfg.Device,
		Card:     peer.Envelope,
		OwnCards: own,
		Suites:   suites,
		Features: session.FeaturesFor(r.cfg.Features, r.cfg.Profile.LowPower),
		// The handshake adds its own GREASE service (D18), so the registry offer must not add a
		// second one: two random GREASE IDs occasionally collide and break "strictly ascending".
		Services: handshakeServices(r.services.Offer(false)),
	}, nil
}

// ownCards returns the card material for handshakes. On a visible node without a cover the
// builder has no public card (D37); fall back to the peer card and tell the operator.
func (r *Runtime) ownCards(ctx context.Context) (handshake.OwnCards, error) {
	own, err := r.cfg.Cards.OwnCards(ctx)
	if err == nil {
		if r.vis == cards.Visible {
			if _, ok := own.Current[uint32(objects.ScopePublic)]; !ok {
				r.noCoverEvent()
			}
		}
		return own, nil
	}
	if !errors.Is(err, cards.ErrNoPublicCard) {
		return handshake.OwnCards{}, fmt.Errorf("node: own cards: %w", err)
	}
	peer, perr := r.cfg.Cards.Current(ctx, objects.ScopePeer)
	if perr != nil {
		return handshake.OwnCards{}, fmt.Errorf("node: peer card: %w", perr)
	}
	r.noCoverEvent()
	return handshake.OwnCards{
		Current: map[uint32]handshake.OwnCard{
			uint32(objects.ScopePeer): {Seq: peer.Seq, Envelope: peer.Envelope},
		},
		Issued: map[uint64]handshake.IssuedCard{},
	}, nil
}

func (r *Runtime) noCoverEvent() {
	r.mu.Lock()
	if r.noCover {
		r.mu.Unlock()
		return
	}
	r.noCover = true
	r.mu.Unlock()
	r.events.Emit(Event{
		Kind: EventNoCover,
		Message: "node has no camouflaged listener: the public card is not published, " +
			"only peers and invitees are served (wire.md §2.1 D37)",
		At: time.Now(),
	})
}

func (r *Runtime) clockWarn(offset time.Duration) {
	r.events.Emit(Event{
		Kind:    EventClockWarning,
		Message: "the system clock differs from the network by more than 60 s",
		At:      time.Now(),
		Err:     fmt.Errorf("offset %s", offset),
	})
}

func (r *Runtime) policyFunc(choices []handshake.ServiceChoice) mux.StreamPolicy {
	return r.services.Policy(toChoices(choices))
}

func toChoices(choices []handshake.ServiceChoice) []service.Choice {
	out := make([]service.Choice, 0, len(choices))
	for _, c := range choices {
		out = append(out, service.Choice{ID: uint64(c.ID), Version: uint64(c.Version)})
	}
	return out
}

func handshakeServices(offers []service.Offer) []handshake.ServiceOffer {
	out := make([]handshake.ServiceOffer, 0, len(offers))
	for _, o := range offers {
		if o.ID > 0xffffffff {
			continue
		}
		versions := make([]uint32, 0, len(o.Versions))
		for _, v := range o.Versions {
			if v <= 0xffffffff {
				versions = append(versions, uint32(v))
			}
		}
		if len(versions) == 0 {
			continue
		}
		out = append(out, handshake.ServiceOffer{ID: uint32(o.ID), Versions: versions})
	}
	return out
}

func (r *Runtime) samplePeer(est *session.Established) {
	if est == nil || est.Peer == nil || est.PeerTime == 0 {
		return
	}
	r.clock.AddPeerSample(clock.PeerID(est.Peer.DeviceID), time.Unix(int64(est.PeerTime), 0))
}

// cardSource is Host.Cards over the card builder and the persistent card book.
type cardSource struct {
	builder *cards.CardBuilder
	book    *session.CardBook
}

func (c cardSource) Own(scope objects.Scope) ([]byte, bool) {
	ic, err := c.builder.Current(context.Background(), scope)
	if err != nil {
		return nil, false
	}
	return ic.Envelope, true
}

func (c cardSource) Peer(device DeviceID) *objects.Card {
	return c.book.Latest([32]byte(device))
}

func (c cardSource) Devices(node NodeID) *objects.DeviceList {
	return c.book.DeviceList([32]byte(node))
}
