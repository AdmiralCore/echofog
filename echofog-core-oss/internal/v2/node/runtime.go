package node

import (
	"context"
	"errors"
	"fmt"
	"github.com/echofog/echofog-core/internal/v2/call"
	"github.com/echofog/echofog-core/internal/v2/call/callnet"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/geoip"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mailbox"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/pex"
	"github.com/echofog/echofog-core/internal/v2/portmap"
	"github.com/echofog/echofog-core/internal/v2/release"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
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
	// ErrNoDomesticEntry: the country policy requires a first hop inside the client's country and
	// the node has none (T6.4). It is an event for the application, not a quiet degradation.
	ErrNoDomesticEntry = errors.New("node: no entry node in the own country")
	// ErrRouteUnsupported: the policy requires an entry in the client's country but the rules name
	// no verified range (allowed_cidrs), so v2.0 does not support this network (T6.4, H14).
	ErrRouteUnsupported = errors.New("node: v2.0 does not support this network without a verified entry")
	// ErrNoSession means a path operation needs a live session to the peer.
	ErrNoSession = errors.New("node: no live session to the peer")
	// ErrTooManyPaths means the platform or the standby limit forbids another path now.
	ErrTooManyPaths = errors.New("node: too many paths to the peer")
	// ErrPathFeature means the peer did not negotiate path-migration.
	ErrPathFeature = errors.New("node: path-migration was not negotiated")
	// ErrBackground means the node is suspended and does not open new sessions
	// (platforms.md §2: suspended closes sessions with GOAWAY).
	ErrBackground = errors.New("node: background mode is suspended")
)

// Config assembles one runtime.
type Config struct {
	// Account and Device are the local identity. Required.
	Account *identity.Account
	Device  *identity.Device
	// Cards builds and remembers the node's own cards. Required.
	Cards *cards.CardBuilder
	// Chat turns chat/2 on (T7.1/T7.2): the prekeys of the device are published, BUNDLE_GET and DELIVER are
	// served and the outbox runs. ChatEvents receives incoming messages and statuses, ChatMail is the
	// mailbox client for recipients that are away (mailbox/2 client, T7.4; nil until then).
	Chat       bool
	ChatEvents chat.Events
	ChatMail   chat.Mail
	// Files turns files/2 on (T8.1/T11.3d): the node serves the files it offers and the app fetches the
	// ones it is offered through OpenFiles. FilesLimits bound what the server holds; FilesOnDone is called
	// when a recipient confirmed it got a whole file.
	Files       bool
	FilesLimits files.Limits
	FilesOnDone func(fid [16]byte, node [32]byte)
	// MailboxRelays are the mailbox relays M where this node keeps its queues while it is away (T7.4);
	// nil means the node has no queue. MailboxVia are the forwarding relays F it hands deposits to
	// (default: the same). ChatContacts lists the accounts told about a new queue.
	MailboxRelays func() [][32]byte
	MailboxVia    func() [][32]byte
	ChatContacts  func() [][32]byte
	// MailboxWait is the long-poll time of the fetch loop; zero means 30 s.
	MailboxWait time.Duration

	// Store is the persistent store; the runtime opens its own namespaces in it. It does not
	// close the store: the owner keeps it across runtime restarts. Required.
	Store *store.Store

	// Clock is the corrected clock (T3.8). When nil the runtime creates one over HTTP.
	Clock *clock.Clock
	// HTTP is the single HTTP client of the node (02-architecture §3.1). Nil leaves it unset and
	// the CDN clock check off: production supplies a uTLS Chrome-hello client that goes through
	// the node's proxy settings (T11.6, handshake.md D36); the runtime never builds one of its own.
	HTTP *http.Client
	// Log receives structured diagnostics. Nil means slog.Default.
	Log *slog.Logger
	// Events receives runtime events (D37 operator warnings, clock warnings). Nil logs them.
	Events EventSink
	// Profile is the platform profile and flags (platforms.md §2).
	Profile Profile
	// ReleaseCheck verifies a release/2 object against the compiled release keys and the threshold
	// (objects.ReleaseState.Check; wired by T4.5). Nil: this node stores no release in the DHT.
	ReleaseCheck func(raw []byte, now time.Time) error
	// ReleaseKeys are the release public keys compiled into the build (objects.md §9, D16). With
	// them the runtime keeps the release manifest (T4.5): it checks DHT records, offers the
	// manifest in pex/2, refuses peers below min_supported with REJECT{version} and reports
	// available updates. Nil: no manifest handling.
	ReleaseKeys []keys.AKey
	// ReleaseSiteURL is the https URL of the manifest on the download page; empty disables that
	// source (the DHT and pex/2 remain).
	ReleaseSiteURL string
	// ReleasePoll overrides the 24-hour poll interval of the DHT and the download page (tests).
	ReleasePoll time.Duration
	// GreaseInterval returns the delay before the next GREASE frame of a session (D18). Nil
	// means a uniform random interval in [greaseMin, greaseMax]; tests inject a short one.
	GreaseInterval func() time.Duration

	// Listeners are already built carriers; ListenerSources are built at Start.
	Listeners []carrier.Listener
	// ContactListeners are listeners that carry the contacts masking key (L0 of the peer card).
	// They are started with Listeners; the runtime keeps their key set equal to the access
	// manager's current and previous contacts keys, so a rotation (D11) reaches L0 (T3.6b).
	// A listener that cannot replace its keys in place is closed on rotation: fail closed.
	ContactListeners []carrier.Listener
	// ContactListenerSources are built at Start like ListenerSources and treated like ContactListeners.
	ContactListenerSources []ListenerSource
	ListenerSources        []ListenerSource
	// Registry replaces the carrier suite registry (tests, a build without a suite).
	Registry *carrier.Registry
	// CarrierClock is the raw carrier clock for tests (AD13). Nil means the system clock. When a
	// custom Clock is supplied, both must read the same raw time. The handshake and card
	// lifetimes use CarrierClock.Now(); only the L0 first packets add Clock.Offset().
	CarrierClock carrier.Clock
	// LocalInfo feeds the D21 carrier plan (own public addresses, country lookup, UDP flag).
	LocalInfo carrier.LocalInfo
	// DialContext opens carrier sockets through the node's proxy (T11.6).
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// FreezeStall is how long a flow that wrote and heard nothing back counts as stopped when it
	// is looking for the freeze of a network (T6.4d); zero means 10 s.
	FreezeStall time.Duration
	// FreezeFlowAge is the oldest a flow may be, counted from its open, for its stop to count as the
	// freeze: the handshake and the first data, not a long request; zero means 30 s.
	FreezeFlowAge time.Duration

	// Suites are the L1 suites the node offers or accepts. Nil means XK before XX.
	Suites []uint16
	// Features are the L2 features offered in handshakes (GREASE is added by service.Offer).
	Features []uint32
	// Passes resolves invitation and personal pass secrets for a hidden responder (T3.6).
	Passes handshake.PassStore
	// Access is the invitation, pass and masking-key manager of a hidden node (T3.6b). When set
	// it is also the responder's PassStore: the runtime redeems or renews access after a
	// hidden-node session, installs its invitation listeners and follows the contacts-key
	// rotation.
	Access *access.Manager
	// AccessListener builds the L0 listener of one invitation slot from its live masking keys.
	// Nil builds the built-in noise3 listener from the slot address; a non-noise3 suite needs a
	// factory. The factory must not retain the private key bytes.
	AccessListener AccessListenerFactory
	// AccessReissued receives the invitations that Revoke and RotateKeys reissued. The holders
	// have no session yet, so the only delivery path is the application (a messenger message).
	// The payload contains new secrets and links: the sink must never log it.
	AccessReissued func([]access.Minted)
	// Puzzle enables HS_RETRY under load (T3.5).
	Puzzle *handshake.PuzzleConfig
	// RetryAfter is the GOAWAY retry_after used for runtime shutdown and idle closes, in seconds.
	RetryAfter uint32
	// Country is the offline GeoIP lookup; nil means internal/v2/geoip.
	Country func(netip.Addr) (string, bool)
	// RoutePolicy is the country policy table (T6.4); nil means the built-in table.
	RoutePolicy *routepolicy.Rules
	// NetClass is the initial kind of the client's network (user setting or the platform report);
	// the application changes it with SetNetClass. Unknown counts as wired.
	NetClass routepolicy.NetClass
	// ShapeProfile is the local shape profile used for idle PADDING when the idle-padding
	// feature is negotiated (wire.md §6.4, §9). Nil disables idle padding.
	ShapeProfile *shape.Profile
	// ExitShapeProfile is the shape profile of the exit address: the profile H1 pads the first
	// records of every exit tunnel with (services.md §7). Nil takes the profile of the node's
	// listeners; padding is never silently off.
	ExitShapeProfile *shape.Profile
	// PunchNet supplies the datagram side of hole punching (T6.2). Nil disables punching: sessions
	// through a relay stay relayed.
	PunchNet PunchNetwork
	// PortMap holds the test seams and tuning of port mapping (T6.5b). Whether it runs is
	// Profile.PortMapping; Enabled here is ignored. Zero values select the production gateway.
	PortMap portmap.Config
}

// Runtime is one running v2 node (02-architecture AD5).
type Runtime struct {
	lastSkewEvent atomic.Int64 // unix nanoseconds of the last EventClockSkew
	cfg           Config
	log           *slog.Logger
	events        EventSink
	clock         *clock.Clock
	http          *http.Client

	carrierClock carrier.Clock
	registry     *carrier.Registry
	services     *service.Registry
	localInfo    carrier.LocalInfo
	relayc       *relayClient

	// rules is the country policy table; netClass the kind of the client's network (T6.4).
	rules     *routepolicy.Rules
	netClass  atomic.Uint32
	routeMu   sync.Mutex
	routeInfo map[DeviceID]RouteReport
	// freeze marks a network whose TCP stops at 12-20 KiB (T6.4d); netID names the network.
	freeze *routepolicy.FreezeTracker
	// narrow marks a network that cannot carry the first packets of noise3 (T3.12).
	narrow *carrier.NarrowPath
	netMu  sync.Mutex
	netID  string
	// flowSeq numbers the flows the freeze watcher sees.
	flowSeq atomic.Uint64

	// The sources of the own country (T6.4c); countryLast is the one last reported.
	countryMu   sync.Mutex
	countryUser string
	countryHint string
	countryLast string

	cardBook *session.CardBook
	bindings *session.KeyBindings
	abuse    *session.AbuseLog
	limits   *session.Limits
	control  *session.Control
	observer *session.Observer
	// pm forwards the listener ports at the home router (T6.5b); pmApply serializes the cards
	// following it, pmPorts is what Start asked for.
	pm      *portmap.Manager
	pmApply sync.Mutex
	pmMu    sync.Mutex
	pmPorts []mappedPort
	binder  *mux.Binder
	policy  *session.PathPolicy

	// pathPairs maps a bound path to the carrier pair it was opened to (H11/H15). Only paths
	// this node opened as a carrier client are counted. deadPaths remembers failures that
	// arrived before the pair was recorded, so a late record cannot resurrect a slot.
	pathsMu   sync.Mutex
	pathPairs map[pathKey]session.Pair
	deadPaths map[pathKey]struct{}
	// repairing holds the sessions a repair is running for; repairOpt is what a repair dials with.
	repairing map[[16]byte]bool
	repairOpt DialOpt

	identity Identity
	vis      cards.Visibility
	cards    cardSource
	peers    *pex.Store
	pex      *pex.Service

	mu         sync.Mutex
	handlers   map[[2]uint64]Handler
	table      *session.Table
	listeners  []carrier.Listener
	background Background
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	serving    sync.Map
	dgrams     *datagramBroker
	// call media (T9.3): the transport over live sessions, call_route and the signal port
	callTr            *callnet.Transport
	callSvc           *call.Service
	callAdmit         atomic.Pointer[func([32]byte) bool]
	callRoute         atomic.Uint32
	callSignals       atomic.Pointer[signalHolder]
	dht               *dht.Table
	dhtServer         *dht.Server
	dhtClient         *dht.Client
	releaseM          *release.Manager
	devList           ownDevices
	chat              *chat.Service
	files             *files.Server
	chatKeys          *chatcrypto.Crypto
	mailRecv          *mailbox.Receiver
	mailboxClientOpts func([32]byte) DialOpt
	mailSrv           *mailbox.Server
	// dhtDialHook lets a test choose the dial options of a DHT query (in-memory carriers).
	dhtDialHook func(*objects.Card) DialOpt
	// mailboxDialHook is the same for the dials of the forwarding relay F.
	mailboxDialHook func(*objects.Card) DialOpt
	modules         sync.Once
	// exitMu guards exitIdents, the per-exit client identities loaded in this run (T10.2).
	exitMu     sync.Mutex
	exitIdents map[DeviceID]*exitIdent
	started    bool
	stopped    bool
	noCover    bool
	startedAt  time.Time

	// localMu guards localAddrs, which a network change may replace while sessions dial.
	localMu    sync.RWMutex
	localAddrs []netip.Addr

	// accessMgr is the invitation/pass manager of a hidden node (T3.6b). accessMu guards the
	// installed invitation listeners and the contacts key the card builder uses.
	accessMgr   *access.Manager
	accessMu    sync.Mutex
	accessSlots map[int]*accessSlot
	// contactsMu guards the contacts-key state below. It is taken before accessMu and never while
	// holding it, so a slow listener build does not hold up the card builder (and vice versa).
	contactsMu        sync.Mutex
	accessContacts    keys.AKey
	accessContactsSet bool
	contactLs         []carrier.Listener
	contactPrints     []accessKeyPrint
	// admitMu guards admitted: the sessions that passed handleAccess with a pass. Only they get
	// the peer card (objects.md §10.3); a session leaves the set when it ends or its pass dies.
	admitMu  sync.Mutex
	admitted map[*session.Established]admission
	// admitSem bounds the concurrent pass bookings of accepted sessions.
	admitSem chan struct{}
	// hsSem bounds the goroutines that run pre-authentication handshakes (serve.go).
	hsSem chan struct{}
}

var _ Host = (*Runtime)(nil)

// New validates the configuration and opens the persistent session state. Listeners are not
// started until Start.
func New(cfg Config) (*Runtime, error) {
	cfg.Profile = cfg.Profile.WithDefaults()
	if err := cfg.Profile.Validate(); err != nil {
		return nil, err
	}
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

	// Access is always the responder's PassStore: redeeming a pass the handshake did not use
	// would be inconsistent (T3.6b).
	if cfg.Access != nil {
		cfg.Passes = cfg.Access
	}

	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	events := cfg.Events
	if events == nil {
		events = logSink{log: log}
	}
	// Without cfg.HTTP the CDN clock check stays off (handshake.md D36, audit M1 B-4): the node
	// never builds a Go-TLS client of its own, whose fingerprint and environment proxy would
	// betray it.
	httpClient := cfg.HTTP
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
		deadPaths:    map[pathKey]struct{}{},
		repairing:    map[[16]byte]bool{},
		background:   cfg.Profile.Background,
		rules:        cfg.RoutePolicy,
	}
	r.freeze = routepolicy.NewFreezeTracker(carrierClock.Now)
	r.narrow = carrier.NewNarrowPath(carrierClock, r.narrowMarked)
	if r.rules == nil {
		r.rules = routepolicy.Default()
	}
	r.netClass.Store(uint32(cfg.NetClass))
	r.hsSem = make(chan struct{}, maxHandshaking)
	r.dgrams = newDatagramBroker(r)
	r.accessMgr = cfg.Access
	if r.accessMgr != nil {
		r.accessSlots = make(map[int]*accessSlot)
		r.admitted = make(map[*session.Established]admission)
		r.admitSem = make(chan struct{}, maxAdmitting)
	}

	r.clock = cfg.Clock
	if r.clock == nil {
		ccfg := clock.Config{Warn: r.clockWarn}
		if httpClient != nil {
			ccfg.HTTP = httpClient
		}
		c, err := clock.New(ccfg)
		if err != nil {
			return nil, fmt.Errorf("node: clock: %w", err)
		}
		r.clock = c
	}

	reg, err := suiteRegistry(cfg, l0Clock{raw: carrierClock, offset: r.clock.Offset})
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
	if r.limits, err = session.NewLimits(session.LimitsConfig{Now: r.carrierClock.Now, Abuse: r.abuse}); err != nil {
		return nil, fmt.Errorf("node: limits: %w", err)
	}
	r.bindings, err = session.NewKeyBindings(cfg.Store.Store(session.BindingsNamespace), r.abuse)
	if err != nil {
		return nil, fmt.Errorf("node: key bindings: %w", err)
	}
	r.observer = session.NewObserver(session.ObserverConfig{
		Now:      r.clock.Now,
		OnChange: r.publicAddrChanged,
	})
	pmCfg := cfg.PortMap
	pmCfg.Enabled = cfg.Profile.PortMapping
	if pmCfg.Clock == nil {
		pmCfg.Clock = r.carrierClock
	}
	pmCfg.OnChange = r.portMapChanged
	r.pm = portmap.New(pmCfg)
	r.relayc = newRelayClient(r)
	if b, ok := cfg.PunchNet.(interface{ bind(*Runtime) }); ok {
		b.bind(r)
	}
	if !relaysForOthers(cfg.Profile) {
		// N6: a card must not announce a relay that would answer UNSUPPORTED.
		if err := cfg.Cards.ClearFlags(objects.FlagRelay); err != nil {
			return nil, fmt.Errorf("node: card relay flag: %w", err)
		}
	}
	r.control = session.NewControl(session.ControlConfig{
		Cards:    r.cardBook,
		Abuse:    r.abuse,
		Clock:    r.carrierClock,
		Observer: r.observer,
		OnPunch:  r.relayc.onPunch,
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

	if r.releaseM, err = r.newReleaseManager(); err != nil {
		return nil, fmt.Errorf("node: release manifest: %w", err)
	}

	peers, err := pex.Open(pex.Options{KV: cfg.Store.Store(pex.Namespace), Log: log, Now: r.clock.Now})
	if err != nil {
		return nil, fmt.Errorf("node: pex candidates: %w", err)
	}
	r.peers = peers
	r.pex, err = pex.New(pex.Config{
		Store:      peers,
		SelfNode:   [32]byte(r.identity.NodeID),
		SelfDevice: [32]byte(r.identity.DeviceID),
		OwnCard:    r.ownPublicCard,
		Now:        r.clock.Now,
		Log:        log,
		Release:    r.pexRelease(),
		OnRelease:  r.pexOnRelease(),
	})
	if err != nil {
		return nil, fmt.Errorf("node: pex service: %w", err)
	}

	r.localInfo = cfg.LocalInfo
	if r.localInfo.CountryOf == nil {
		r.localInfo.CountryOf = r.Country
	}
	// The app's local addrs callback replaces net.Interfaces on mobile (platforms.md §1.1,
	// §2); an explicit Config.LocalInfo still wins.
	r.localAddrs = append([]netip.Addr(nil), cfg.LocalInfo.Addrs...)
	if len(r.localAddrs) == 0 && cfg.Profile.LocalAddrs != nil {
		r.localAddrs = append([]netip.Addr(nil), cfg.Profile.LocalAddrs()...)
	}
	// udp=blocked leaves only TCP suites in the D21 plan (platforms.md §2).
	if cfg.Profile.UDP == UDPBlocked {
		r.localInfo.UDPBlocked = true
	}
	return r, nil
}

// planInfo is the D21 view of this node with the current local addresses.
func (r *Runtime) planInfo() carrier.LocalInfo {
	info := r.localInfo
	r.localMu.RLock()
	info.Addrs = append([]netip.Addr(nil), r.localAddrs...)
	r.localMu.RUnlock()
	return info
}

// RefreshLocalAddrs re-reads the platform local_addrs callback and returns the addresses the
// carrier plan now uses. The app calls it when the OS reports a network change; wiring that
// event to the runtime is T3.3 (platforms.md §2).
func (r *Runtime) RefreshLocalAddrs() []netip.Addr {
	if r.cfg.Profile.LocalAddrs != nil {
		addrs := append([]netip.Addr(nil), r.cfg.Profile.LocalAddrs()...)
		r.localMu.Lock()
		r.localAddrs = addrs
		r.localMu.Unlock()
	}
	r.noteCountry()
	r.localMu.RLock()
	defer r.localMu.RUnlock()
	return append([]netip.Addr(nil), r.localAddrs...)
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
	var contactLs []carrier.Listener
	for i, src := range r.cfg.ContactListenerSources {
		if src == nil {
			closeListeners(listeners)
			_ = table.Close()
			cancel()
			r.mu.Unlock()
			return fmt.Errorf("%w: contact listener source %d is nil", ErrConfig, i)
		}
		l, lerr := src.Listen(runCtx)
		if lerr != nil {
			closeListeners(listeners)
			_ = table.Close()
			cancel()
			r.mu.Unlock()
			return fmt.Errorf("node: building contact listener %d: %w", i, lerr)
		}
		listeners = append(listeners, l)
		contactLs = append(contactLs, l)
	}
	for _, l := range r.cfg.ContactListeners {
		if l == nil {
			continue
		}
		listeners = append(listeners, l)
		contactLs = append(contactLs, l)
	}
	r.contactLs = contactLs
	r.listeners = listeners
	r.mu.Unlock()

	// Platform events that describe this process, reported once at startup: reduced memory
	// protection (T4.1) and a visibility that changed since the builder was created.
	r.watchMemoryProtection()
	r.noteVisibility(r.cfg.Cards.Visibility())

	// D37: a visible node without a camouflaged listener has no public card. This is not a
	// startup error; the operator is told and the node keeps serving peers and invitees.
	if _, err := r.ownCards(ctx); err != nil {
		r.cleanupAfterStartFailure()
		return fmt.Errorf("node: own cards: %w", err)
	}

	r.modules.Do(r.registerModules)
	if r.releaseM != nil {
		if err := r.releaseM.Err(); err != nil {
			// Fail closed (T4.5 L2): the stored chain is unreadable, so manifests are refused and
			// polling would only fail. The operator repairs the profile (Release().Reset()).
			r.events.Emit(Event{Kind: EventReleaseUnavailable, Message: "the stored release manifest chain is unavailable", At: r.clock.Now(), Err: err})
		} else {
			r.wg.Add(1)
			go r.releaseLoop(runCtx)
		}
	}
	if err := r.startAccess(runCtx); err != nil {
		r.cleanupAfterStartFailure()
		return fmt.Errorf("node: access listeners: %w", err)
	}

	r.startPortMapping(listeners[:len(r.cfg.Listeners)+len(r.cfg.ListenerSources)])

	for _, l := range listeners {
		r.wg.Add(1)
		go r.acceptLoop(runCtx, l)
	}

	if r.vis != cards.Visible && r.accessMgr != nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.relayc.registerLoop(runCtx)
		}()
	}

	r.mu.Lock()
	r.started = true
	r.startedAt = time.Now()
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
	dhtTable := r.dht
	r.mu.Unlock()

	if !started {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	closeListeners(listeners)
	// the router forgets the forwarded ports now, not when their lifetime runs out
	_ = r.pm.Close(ctx)
	if dhtTable != nil {
		// Stop saves as well; the explicit save keeps the runtime contract visible and is
		// idempotent.
		if err := dhtTable.Save(); err != nil {
			r.log.Warn("node: saving the dht routing table", "err", err)
		}
		dhtTable.Stop()
	}
	if table != nil {
		_ = table.Close()
	}
	r.stopAccess()
	r.dgrams.close()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		r.destroyExitIdentities()
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
	r.stopAccess()
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

// Self returns the local identity. Visibility follows a card builder update (T3.7a risk 4).
func (r *Runtime) Self() Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.identity
}

// Cards returns the node's card view for services.
func (r *Runtime) Cards() CardSource { return r.cards }

// Peers returns the pex/2 candidate table (T5.4). It is created in New, so it is available before
// the runtime starts and survives a runtime restart on the same store.
func (r *Runtime) Peers() PeerStore { return r.peers }

// Store returns one persistent namespace (T4.3).
func (r *Runtime) Store(ns string) KV { return r.cfg.Store.Store(ns) }

// Clock returns the corrected clock (T3.8).
func (r *Runtime) Clock() Clock { return r.clock }

// HTTP returns the node's single HTTP client (02-architecture §3.1).
func (r *Runtime) HTTP() *http.Client { return r.http }

// Profile returns the platform profile. Background is the current app lifecycle state.
func (r *Runtime) Profile() Profile {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.cfg.Profile
	p.Roles = append([]Role(nil), p.Roles...)
	return p
}

// noteVisibility applies the current visibility of the card builder. A change is reported once
// so the app and the D37 public-card condition follow a card reissue without a restart.
func (r *Runtime) noteVisibility(v cards.Visibility) {
	r.mu.Lock()
	if r.vis == v {
		r.mu.Unlock()
		return
	}
	r.vis = v
	r.identity.Visibility = v
	r.mu.Unlock()
	r.events.Emit(Event{
		Kind:       EventVisibility,
		Message:    fmt.Sprintf("node visibility is now %s", v),
		At:         time.Now(),
		Visibility: v,
	})
}

// listenerStopped reports an accept loop that ended on a local error; the listener no longer
// accepts, so silent degradation is not an option (T3.7a risk 3).
func (r *Runtime) listenerStopped(err error) {
	r.log.Warn("node: accept loop stopped", "err", err)
	r.events.Emit(Event{
		Kind:    EventListenerStopped,
		Message: "a listener stopped accepting: no new sessions arrive on it",
		At:      time.Now(),
		Err:     err,
	})
}

// track counts one more goroutine of the runtime in r.wg, unless Stop has begun. Goroutines that
// are started from code the runtime does not track (service handlers, relay callbacks, callers of
// Open) must be counted through it: Add under r.mu cannot overlap the wait of Stop, which takes the
// same lock to set stopped before it waits. A false result means the runtime has stopped and the
// goroutine must not be started; the caller runs the goroutine itself and calls r.wg.Done when it ends.
func (r *Runtime) track() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	r.wg.Add(1)
	return true
}

// Background returns the current app lifecycle state.
func (r *Runtime) Background() Background {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.background
}

// SetBackground applies the app lifecycle state to live sessions (platforms.md §2). suspended
// sends GOAWAY to every session and refuses new outgoing ones; limited keeps one session to a
// guard or mailbox relay and closes the rest. foreground has no immediate effect: sessions are
// re-established lazily.
func (r *Runtime) SetBackground(bg Background) error {
	if bg == "" {
		bg = BackgroundForeground
	}
	switch bg {
	case BackgroundForeground, BackgroundLimited, BackgroundSuspended:
	default:
		return fmt.Errorf("%w: background %q", ErrConfig, bg)
	}

	r.mu.Lock()
	if r.background == bg {
		r.mu.Unlock()
		return nil
	}
	r.background = bg
	r.cfg.Profile.Background = bg
	table := r.table
	r.mu.Unlock()

	var keep *session.Established
	switch bg {
	case BackgroundSuspended:
	case BackgroundLimited:
		if table != nil {
			keep = limitedKeep(table.Sessions())
		}
	}

	var victims []*session.Established
	if table != nil {
		for _, est := range table.Sessions() {
			if est != keep && liveSession(est) {
				victims = append(victims, est)
			}
		}
	}
	closed := len(victims)
	if len(victims) > 0 {
		// Announce under the runtime lock: Stop takes the same lock before it waits, so a
		// wg.Add here can never race with wg.Wait and the goroutines are drained by Stop.
		r.mu.Lock()
		var ctx context.Context
		if r.started && !r.stopped && r.ctx != nil {
			ctx = r.ctx
			r.wg.Add(len(victims))
		}
		r.mu.Unlock()
		if ctx != nil {
			for _, est := range victims {
				go func(est *session.Established) {
					defer r.wg.Done()
					_ = est.GoAway(ctx, frame.CodeGoingAway, "background-"+string(bg), r.cfg.RetryAfter)
				}(est)
			}
		}
	}
	r.events.Emit(Event{
		Kind:       EventBackground,
		Message:    fmt.Sprintf("background mode %s: %d session(s) told to go away", bg, closed),
		At:         time.Now(),
		Background: bg,
		Closed:     closed,
	})
	return nil
}

// limitedKeep picks the one session limited mode may keep: a mailbox relay first, then a relay
// (the guard class of PEX, T5.4), then the most recently active session.
func limitedKeep(sessions []*session.Established) *session.Established {
	score := func(est *session.Established) int {
		if est == nil || est.Peer == nil {
			return 0
		}
		switch {
		case est.Peer.Flags&objects.FlagMailbox != 0:
			return 2
		case est.Peer.Flags&objects.FlagRelay != 0:
			return 1
		}
		return 0
	}
	var best *session.Established
	for _, est := range sessions {
		if !liveSession(est) {
			continue
		}
		if best == nil || score(est) > score(best) ||
			(score(est) == score(best) && lastActivity(est).After(lastActivity(best))) {
			best = est
		}
	}
	return best
}

func liveSession(est *session.Established) bool {
	if est == nil || est.Session == nil {
		return false
	}
	select {
	case <-est.Session.Done():
		return false
	default:
		return !est.Session.GoingAway()
	}
}

func lastActivity(est *session.Established) time.Time {
	if est == nil || est.Session == nil {
		return time.Time{}
	}
	return est.Session.LastActivity()
}

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
	// A card reissue (T4.2a) may change visibility; the runtime follows it without a restart.
	r.noteVisibility(r.cfg.Cards.Visibility())
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
	features := append([]uint32(nil), r.cfg.Features...)
	if r.cfg.Profile.LowPower {
		// A low_power node does not offer idle padding (platforms.md §2).
		features = withoutFeature(features, idlePaddingFeature)
	}
	return session.LocalIdentity{
		Device:   r.cfg.Device,
		Card:     peer.Envelope,
		OwnCards: own,
		Suites:   suites,
		Features: features,
		// The handshake adds its own GREASE service (D18), so the registry offer must not add a
		// second one: two random GREASE IDs occasionally collide and break "strictly ascending".
		Services: handshakeServices(r.services.Offer(false)),
	}, nil
}

// withoutFeature returns features without id.
func withoutFeature(features []uint32, id uint32) []uint32 {
	out := features[:0]
	for _, f := range features {
		if f != id {
			out = append(out, f)
		}
	}
	return out
}

// ownCards returns the card material for handshakes. On a visible node without a cover the
// builder has no public card (D37); fall back to the peer card and tell the operator.
func (r *Runtime) ownCards(ctx context.Context) (handshake.OwnCards, error) {
	own, err := r.cfg.Cards.OwnCards(ctx)
	if err == nil {
		if r.Self().Visibility == cards.Visible {
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

// clockSkewEvery limits how often the UI is told about a clock that is too far off.
const clockSkewEvery = 10 * time.Minute

// noteClockSkew tells the UI, once per 10 minutes, that a handshake failed because the device clock
// differs by more than the 600 s the handshake tolerates (handshake.md section 7). The tolerance
// itself is not widened and the offset plays no part in the handshake; the user must fix the clock.
func (r *Runtime) noteClockSkew(err error) {
	var rej *session.ErrRejected
	skew := errors.Is(err, handshake.ErrClockSkew) ||
		(errors.As(err, &rej) && rej.Reason == handshake.ReasonClockSkew)
	if !skew {
		return
	}
	now := time.Now().UnixNano()
	last := r.lastSkewEvent.Load()
	if last != 0 && now-last < int64(clockSkewEvery) {
		return
	}
	if !r.lastSkewEvent.CompareAndSwap(last, now) {
		return
	}
	r.events.Emit(Event{
		Kind: EventClockSkew,
		Message: "Часы устройства отличаются от часов сети больше чем на 10 минут, поэтому соединение " +
			"не установлено. Нужно исправить дату и время в настройках системы.",
		At:  time.Now(),
		Err: err,
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

// samplePeer gives the clock the time of the peer of a session this node dialed. Incoming sessions
// never call it: anyone can open one (handshake.md section 7).
func (r *Runtime) samplePeer(est *session.Established) {
	if est == nil || est.Peer == nil || est.PeerTime == 0 {
		return
	}
	r.clock.AddOutgoingSample(est.Remote.Addr(), time.Unix(int64(est.PeerTime), 0))
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
