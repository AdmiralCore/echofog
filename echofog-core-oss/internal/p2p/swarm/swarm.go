package swarm

import (
	"context"
	"log/slog"
	"sync/atomic"

	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/handshake"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/peerstore"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"
)

type Swarm struct {
	activePeers map[types.PeerID]*Peer
	mu          sync.RWMutex

	rpcHandlers map[net_types.MessageType]RPCServiceFunc
	rpcMu       sync.RWMutex

	fastHandlers     map[net_types.MessageType]FastFrameHandler
	fastHandlersMu   sync.RWMutex
	streamHandlers   map[net_types.MessageType]StreamHandler
	streamHandlersMu sync.RWMutex

	frameHandlers   []FrameHandler
	frameHandlersMu sync.RWMutex

	drop dropState // frame types discarded on arrival (DropFrameTypes)

	logger *slog.Logger
	wg     sync.WaitGroup

	observers   []TopologyObserver
	fetchBundle BundleFetcher
	emitter     events.Emitter

	dispatcher *dispatcher.Dispatcher
	storage    storage.Storage

	selfID     types.PeerID
	selfPubKey types.PeerPublicKey

	cardMu       sync.Mutex
	card         []byte    // our bundle record without one-time prekeys, sent with init packets
	cardAge      time.Time // when card was made
	announcedSNI string
	mailboxes    []MailboxHint // the relays that keep our mail while we are away (under cardMu)

	mbxMu sync.Mutex
	mbx   map[types.PeerID][]MailboxHint // other peers' mailbox relays, as their records named them

	// padsTunnels mirrors whether this node announced handshake.CapTunnelPadding, so exit-tunnel
	// wiring can decide to pad without recomputing the gating logic that lives in the node layer.
	padsTunnels atomic.Bool

	peerSNIMu  sync.RWMutex
	peerFacts  map[types.PeerID]peerFact // camouflage SNI and server flag per peer (swarm_servers.go)
	sameRegion func(addr string) bool    // region policy of the noise guard, nil: nothing is in the home region
	explicit   map[types.PeerID]struct{} // peers the operator listed (bootstrap): dialed whatever they announce
	guards     guardSet                  // the camouflaged servers a user's node keeps its cross-border links to (swarm_guards.go)
	guardsFree atomic.Bool               // guard_mode "auto" stands down: the node is placed in a country that does not censor (swarm_guard_auto.go)

	domestic atomic.Pointer[func(addr string) bool] // servers of the own country (swarm_guard_domestic.go)

	// exitPools marks peers that run a parallel session pool (the exit a VPN tunnels through):
	// addPeer keeps several concurrent connections to such a peer instead of one, and tunnel
	// streams spread across them. A mesh peer is never marked and keeps strict one-connection
	// semantics. Guarded by exitMu.
	exitMu    sync.RWMutex
	exitPools map[types.PeerID]*exitPoolState

	netTransport network.Transport
	wsTransport  WSTransport // optional: dial and listen over WebSocket (behind a CDN)

	peerStore *peerstore.PeerStore

	identity *identity.ProtectedIdentity

	sessionManager *SessionManager

	cfg *config.AppConfig

	addrPolicy atomic.Pointer[network.AddrPolicy] // which peer addresses are handed on and kept

	bootMu    sync.RWMutex
	bootstrap []string // nodes the maintainer dials when peers run short

	sourcesMu  sync.RWMutex
	candidates []CandidateSource // more peers to dial (the DHT routing table)
	learned    []func(pub types.PeerPublicKey, addr string, caps handshake.Capability)

	maint    maintainState
	autoDial atomic.Bool // dial peers learned from others on our own (network.no_auto_dial unsets)
	noDial   atomic.Bool // an isolated node: it dials nobody, the people it invited connect to it
	// selfAddrs are addresses that turned out to be this node's own (announced in the DHT or by
	// peers under an older ID): dialing them again would only reach ourselves
	selfAddrs sync.Map

	bytesRx atomic.Uint64
	bytesTx atomic.Uint64

	lim limits // inbound caps, stream slots, misbehaviour scores (limits.go)

	ctx    context.Context
	cancel context.CancelFunc
}

func NewSwarm(ctx context.Context, netTransport network.Transport, selfID types.PeerID, d *dispatcher.Dispatcher, identity *identity.ProtectedIdentity, connChan <-chan network.NewConnEvent, storage storage.Storage, cfg *config.AppConfig) *Swarm {
	ctx, cancel := context.WithCancel(ctx)

	s := &Swarm{
		activePeers:    make(map[types.PeerID]*Peer),
		dispatcher:     d,
		selfID:         selfID,
		storage:        storage,
		cfg:            cfg,
		sessionManager: NewSessionManager(),
		identity:       identity,
		ctx:            ctx,
		cancel:         cancel,
		netTransport:   netTransport,
		peerStore:      peerstore.NewPeerStore(storage, nil),
		fastHandlers:   make(map[net_types.MessageType]FastFrameHandler),
		exitPools:      make(map[types.PeerID]*exitPoolState),
		logger:         slog.Default().With("component", "swarm"),
	}

	s.loadPeerFacts()
	s.sessionManager.sender = s
	s.sessionManager.ctx = s.ctx
	s.initLimits()

	listen := ""
	if netTransport != nil && netTransport.Addr() != nil {
		listen = netTransport.Addr().String()
	}
	allowPrivate := cfg != nil && cfg.Network.AllowPrivateAddrs
	s.SetAddrPolicy(network.PolicyFor(listen, allowPrivate))
	s.autoDial.Store(cfg == nil || !cfg.Network.NoAutoDial)
	// the start-up dials go first; the maintainer steps in if they are not enough
	s.maint.next = time.Now().Add(maintainBackoffMin)

	s.wg.Add(1)
	go s.registrationLoop(connChan)

	s.initCoreRPCs()
	s.registerObservedAddr()

	return s
}

// SetLogger replaces the logger the swarm writes to.
func (s *Swarm) SetLogger(log *slog.Logger) {
	if s != nil && log != nil {
		s.logger = log
	}
}

func (s *Swarm) log() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

func (s *Swarm) registrationLoop(ch <-chan network.NewConnEvent) {
	defer s.wg.Done()
	s.findAndConnectToPeers()

	for {
		select {
		case <-s.ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}

			if s.CheckOnBan(event.PeerID) {
				_ = event.Conn.CloseWithError(0x01, "banned")
				continue
			}

			var release func()
			if !event.IsOut {
				r, err := s.admitInbound(event)
				if err != nil {
					s.log().Debug("Swarm: inbound peer refused", "peer", event.PeerID.String(), "addr", event.RemoteIP.String(), "reason", err)
					_ = event.Conn.CloseWithError(network.ErrCodeSpamDetected, "limited")
					continue
				}
				release = r
			}

			// An exit peer running a parallel session pool keeps several concurrent connections:
			// attach this one as an extra link alongside the existing primary instead of
			// replacing it. Returns true only when it took ownership of the connection.
			if s.tryAttachExitLink(event) {
				continue
			}

			p := s.addPeer(event.PeerPubKey, event.PeerID, event.Conn, event.Addr, event.IsOut, event.Dialable, event.RemoteIP, release, event.Reality)
			if p == nil {
				continue // duplicate connection, already closed
			}
			p.SetCaps(event.Caps)
			go p.transport.StartLoops()
		}
	}
}

// Close shuts the swarm down: it cancels the context and closes every peer connection.
func (s *Swarm) Close() {
	s.cancel()
	if s.peerStore != nil {
		_ = s.peerStore.Close()
	}
	s.mu.Lock()
	for _, p := range s.activePeers {
		p.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// GetStorage returns the storage backend the swarm persists peers and bundles to.
func (s *Swarm) GetStorage() storage.Storage {
	return s.storage
}

// GetSessionManager returns the manager that holds the per-peer session state.
func (s *Swarm) GetSessionManager() *SessionManager {
	return s.sessionManager
}

// GetDispatcher returns the dispatcher that routes decoded messages to services.
func (s *Swarm) GetDispatcher() *dispatcher.Dispatcher {
	return s.dispatcher
}

// GetContext returns the swarm's context, which is cancelled when it closes.
func (s *Swarm) GetContext() context.Context {
	return s.ctx
}

// GetSelfID returns this node's own peer ID.
func (s *Swarm) GetSelfID() types.PeerID {
	return s.selfID
}

// GetMyPubKey returns this node's own public key.
func (s *Swarm) GetMyPubKey() types.PeerPublicKey {
	if s.selfPubKey != (types.PeerPublicKey{}) {
		return s.selfPubKey
	}
	if s.identity != nil {
		key, _ := s.identity.GetPublicKey()
		copy(s.selfPubKey[:], key)
	}
	return s.selfPubKey
}

// PruneInactiveSessions drops sessions idle for longer than maxIdle and returns how many it removed.
func (s *Swarm) PruneInactiveSessions(maxIdle time.Duration) int {
	if s.sessionManager != nil {
		return s.sessionManager.PruneInactive(maxIdle)
	}
	return 0
}

// SetEmitter attaches the node event bus for network telemetry
func (s *Swarm) SetEmitter(e events.Emitter) {
	s.mu.Lock()
	s.emitter = e
	s.mu.Unlock()
}

func (s *Swarm) emit(topic string, payload any) {
	s.mu.RLock()
	e := s.emitter
	s.mu.RUnlock()
	if e != nil {
		e.Emit(topic, payload)
	}
}

// TrafficStats returns the running totals of bytes received and sent across all peers.
func (s *Swarm) TrafficStats() (rx uint64, tx uint64) {
	return s.bytesRx.Load(), s.bytesTx.Load()
}

// Config returns the AppConfig this Swarm was created with, or nil.
func (s *Swarm) Config() *config.AppConfig {
	return s.cfg
}
