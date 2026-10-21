package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/handshake"

	"github.com/echofog/echofog-core/internal/network/bwprobe"
	"github.com/echofog/echofog-core/internal/network/exit_gateway"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"

	"github.com/awnumar/memguard"
	"github.com/dgraph-io/badger/v4"
	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/dispatcher"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/access"
	"github.com/echofog/echofog-core/internal/network/congestion"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/p2p/gossip"
	"github.com/echofog/echofog-core/internal/p2p/holepunch"
	"github.com/echofog/echofog-core/internal/p2p/routing"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	"github.com/echofog/echofog-core/internal/scheduler"
	"github.com/echofog/echofog-core/internal/services"
	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/internal/services/discovery"
	"github.com/echofog/echofog-core/internal/services/filexfer"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/internal/wallet/tonwallet"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/events"
	"github.com/echofog/echofog-core/pkg/logging"
)

type Node struct {
	ID     types.PeerID
	PubKey types.PeerPublicKey

	IsReady bool

	userKey *memguard.Enclave

	identity *identity.ProtectedIdentity
	crypto   *crypto.CryptoEngine

	Cfg       *config.AppConfig
	Storage   storage.Storage
	Transport *network.QuicTransport
	// Net is what the swarm dials and accepts through: QUIC plus the TCP/TLS fallback
	Net            *network.FallbackTransport
	WS             *network.WSTransport // optional: reach this node behind a CDN, and dial ws:// peers
	Dispatcher     *dispatcher.Dispatcher
	Swarm          *swarm.Swarm
	DhtEngine      *dht.EchoFogDHT
	routingTable   *dht.RoutingTable
	NAT            *routing.NATManager
	HolePunch      *holepunch.Service // direct connections between peers behind NATs
	RelayServer    *routing.RelayServer
	RelayClient    *routing.RelayClient
	MailboxManager *messenger.MailboxManager
	Scheduler      *scheduler.TaskScheduler
	EventBus       *events.Bus
	Services       *services.ServiceManager

	Discovery    *discovery.DiscoveryService
	Messenger    *messenger.MessageBroker
	Contact      *contacts.ContactService
	MDNS         *discovery.MDNSService
	CallManager  *call.CallManager
	TonWallet    *tonwallet.Wallet
	Support      *tonwallet.Support // voluntary project share of traffic payments
	exitLedger   *exit_gateway.Ledger
	exitVouchers *exit_gateway.VoucherBook // vouchers from clients' TON vaults, cashed in the background
	exitLedgerMu sync.Mutex
	BWProbe      *bwprobe.Prober
	ExitService  *exit_gateway.ExitService
	Access       *access.Gate    // who may reach this node (hidden and isolated modes)
	Invites      *access.Keyring // the secrets of the hidden nodes this node was invited to
	accessMode   atomic.Pointer[string]
	privacy      privacyHolder // who may write and call (see privacy.go)
	VPNMarket    *vpnmarket.Service
	Files        *filexfer.Service // files sent to and received from contacts and groups
	groupMember  atomic.Pointer[func(group string, peer types.PeerID) bool]

	Logger *slog.Logger

	bootMu sync.Mutex // Cfg.Network.BootstrapNodes is changed by the API while the node runs

	stopOnce sync.Once
	Ctx      context.Context
	Cancel   context.CancelFunc
}

// logLevel is info, or what ECHOFOG_LOG_LEVEL says (debug, info, warn, error): for finding out why
// two nodes do not connect.
func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("ECHOFOG_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func NewNode(parentCtx context.Context, cfg *config.AppConfig, nodeName string) *Node {
	if lvl := logLevel(); lvl < slog.LevelInfo {
		slog.SetLogLoggerLevel(lvl) // the package-level slog calls of the other packages
	}
	eventBus := events.NewBus()
	busLogHandler := events.NewEventLogHandler(eventBus, slog.LevelInfo)
	// One logging setup for the whole process: console, the event bus (live UI), an in-memory ring
	// and a file. Installed as the default slog handler, so every package's slog call is captured,
	// and the Logs view can show and export it. See pkg/logging.
	// The log file is opt-in through ECHOFOG_LOG_FILE so unit tests (which spin up many nodes)
	// do not all fight over one file; the node binary and the desktop app set it. "off" disables
	// it explicitly. The in-memory ring and the event bus are always on.
	logFile := os.Getenv("ECHOFOG_LOG_FILE")
	if logFile == "off" {
		logFile = ""
	}
	compositeLogger := logging.Setup(logging.Options{
		Level:     logLevel(),
		FilePath:  logFile,
		Extra:     []slog.Handler{busLogHandler},
		BaseAttrs: []slog.Attr{slog.String("name", nodeName)},
	})

	parentCtx, cancel := context.WithCancel(parentCtx)

	return &Node{
		Cfg:      cfg,
		Logger:   compositeLogger,
		EventBus: eventBus,
		Ctx:      parentCtx,
		Cancel:   cancel,
	}
}

// tcpCamouflageCert returns the certificate the TCP fallback presents. It loads the operator's
// real certificate for their domain when configured (so an active prober sees a genuine HTTPS
// site), otherwise it generates a self-signed ECDSA certificate for the mimicked name. The
// node's ed25519 identity certificate cannot be reused: a Chrome TLS ClientHello offers no
// ed25519 signature algorithm, so the handshake would fail.
func (n *Node) tcpCamouflageCert() *tls.Certificate {
	if cf, kf := n.Cfg.AntiDPI.TLSCertFile, n.Cfg.AntiDPI.TLSKeyFile; cf != "" && kf != "" {
		cert, err := tls.LoadX509KeyPair(cf, kf)
		if err != nil {
			n.Logger.Error("failed to load anti_dpi TLS certificate; falling back to self-signed", "err", err)
		} else {
			return &cert
		}
	}
	cert, err := mimicry.GenerateCamouflageCert(n.Cfg.AntiDPI.TLSServerName)
	if err != nil {
		n.Logger.Error("failed to generate TCP camouflage certificate; TCP fallback disabled", "err", err)
		return nil
	}
	return &cert
}

func (n *Node) initDB() error {
	if n.Storage != nil {
		return nil
	}
	opts := badger.DefaultOptions(n.Cfg.Storage.DatabasePath)
	opts.Logger = nil
	opts.ValueLogFileSize = 16 * 1024 * 1024
	opts.ValueThreshold = 64 * 1024
	opts.BaseTableSize = 8 * 1024 * 1024
	opts.MemTableSize = 8 * 1024 * 1024

	_ = os.MkdirAll(n.Cfg.Storage.DatabasePath, 0755)

	// a table left unwritten by a killed process would make badger panic on open
	if ids, backup, err := storage.RepairUnwrittenTables(n.Cfg.Storage.DatabasePath); err != nil {
		return fmt.Errorf("the database is damaged and could not be repaired (%w); a copy is in %s", err, backup)
	} else if len(ids) > 0 {
		n.Logger.Warn("Database: removed tables that were never written (the app was closed while saving); the last moments may be missing",
			"tables", ids, "copy_before_repair", backup)
	}

	db, err := badger.Open(opts)
	if err != nil {
		return fmt.Errorf("failed to open badger db: %w", err)
	}
	n.Storage = storage.NewBadgerStorage(db)
	return nil
}

func (n *Node) IsRegistered() bool {
	manifestPath := filepath.Join(n.Cfg.Storage.DatabasePath, "MANIFEST")
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		return true
	}
	return false
}

func (n *Node) Register(appPassword []byte, mnemonicPassphrase []byte) (string, error) {
	if n.IsRegistered() {
		return "", errors.New("node is already registered, please login")
	}

	if err := n.initDB(); err != nil {
		return "", err
	}

	idMgr := identity.NewProtectedIdentity(n.Storage)

	mnemonic, err := idMgr.GenerateNewIdentity(mnemonicPassphrase)
	if err != nil {
		return "", fmt.Errorf("failed to generate identity: %w", err)
	}

	appPassCopy := make([]byte, len(appPassword))
	copy(appPassCopy, appPassword)
	if err = idMgr.Save(appPassCopy); err != nil {
		return "", fmt.Errorf("failed to save identity: %w", err)
	}

	if err = idMgr.Load(appPassword, false); err != nil {
		return "", fmt.Errorf("failed to load identity: %w", err)
	}

	n.identity = idMgr
	n.SetUserKey(appPassword)
	n.IsReady = true
	n.Logger.Info("New identity created successfully")

	mnemonicBytes, err := mnemonic.Show()
	if err != nil {
		return "", fmt.Errorf("failed to read generated mnemonic: %w", err)
	}

	return string(mnemonicBytes), nil
}

func (n *Node) Login(appPassword []byte) error {
	if !n.IsRegistered() {
		return errors.New("node is not registered, please register first")
	}

	if err := n.initDB(); err != nil {
		return err
	}

	idMgr := identity.NewProtectedIdentity(n.Storage)
	err := idMgr.Load(appPassword, false)
	if err != nil {
		// release the database: the next attempt (a fresh node, with the right password) must be
		// able to open it, or one typo locks the user out until the app restarts
		n.Storage.Close()
		n.Storage = nil
		return fmt.Errorf("invalid app password or corrupted identity: %w", err)
	}

	n.identity = idMgr
	n.SetUserKey(appPassword)
	n.IsReady = true
	n.Logger.Info("Login successful")

	return nil
}

func (n *Node) SetUserKey(appPassword []byte) {
	n.userKey = memguard.NewEnclave(appPassword)
}

// VerifyPassword reports whether appPassword unlocks this logged-in node's identity. It reuses the
// already-open storage (no second database handle), so it is safe to call on a running node.
func (n *Node) VerifyPassword(appPassword []byte) error {
	if n.Storage == nil {
		return errors.New("node is not logged in")
	}
	probe := identity.NewProtectedIdentity(n.Storage)
	if err := probe.Load(appPassword, false); err != nil {
		return ErrWrongPassword
	}
	return nil
}

// VerifyProfilePassword reports whether appPassword unlocks the identity stored for cfg's profile,
// without starting the node or leaving its database open. It opens the profile database, tries to
// load the identity, then closes the database again so the caller may safely remove the profile
// directory. Use it to require the real user's password before a destructive action (such as
// deleting a profile) that the WebView could otherwise trigger with no proof the user is present.
func VerifyProfilePassword(cfg *config.AppConfig, appPassword []byte) error {
	n := NewNode(context.Background(), cfg, "verify-password")
	defer n.Cancel()
	if !n.IsRegistered() {
		return errors.New("profile is not registered")
	}
	if err := n.initDB(); err != nil {
		return err
	}
	defer func() {
		if n.Storage != nil {
			n.Storage.Close()
			n.Storage = nil
		}
	}()
	idMgr := identity.NewProtectedIdentity(n.Storage)
	if err := idMgr.Load(appPassword, false); err != nil {
		return ErrWrongPassword
	}
	return nil
}

func (n *Node) Recover(mnemonic string, mnemonicPassphrase []byte, newAppPassword []byte) error {
	if n.IsRegistered() {
		return errors.New("node is already registered on this device, please clear data or use Login")
	}

	if err := n.initDB(); err != nil {
		return err
	}

	idMgr := identity.NewProtectedIdentity(n.Storage)

	err := idMgr.Restore([]byte(mnemonic), mnemonicPassphrase)
	if err != nil {
		return fmt.Errorf("failed to recover identity from mnemonic: %w", err)
	}

	appPassCopy := make([]byte, len(newAppPassword))
	copy(appPassCopy, newAppPassword)
	if err = idMgr.Save(appPassCopy); err != nil {
		return fmt.Errorf("failed to save recovered identity: %w", err)
	}

	if err = idMgr.Load(newAppPassword, false); err != nil {
		return fmt.Errorf("failed to load identity after recovery: %w", err)
	}

	n.identity = idMgr
	n.SetUserKey(newAppPassword)
	n.IsReady = true
	n.Logger.Info("Identity recovered successfully from mnemonic")

	return nil
}

func (n *Node) Start() (err error) {
	// a start that fails half way must not keep the database, sockets or goroutines it opened: the
	// caller will try again with a fresh node on the same profile
	defer func() {
		if err != nil {
			n.shutdown(false)
		}
	}()
	if !n.IsReady {
		return errors.New("node is not ready: you must call Login or Register first")
	}

	n.Logger.Info("EchoFog Core is starting P2P network...", "version", config.CurrentProtocolVersion)

	n.identity.SwitchAccount(0)
	activeAccount, err := n.identity.GetActiveAccount()
	if err != nil {
		return fmt.Errorf("failed to get active account: %w", err)
	}
	// messages, contacts and groups are stored encrypted from here on
	if err := n.sealStorage(activeAccount); err != nil {
		return fmt.Errorf("failed to open encrypted storage: %w", err)
	}
	if err := n.initAccess(); err != nil {
		return err
	}
	if n.gated() {
		n.Logger.Info("ACCESS: this node is "+n.AccessMode()+": only the people with its link, its contacts and those who joined before can reach it", "joined", len(n.Access.Joined()))
	}

	n.ID, _ = n.identity.GetPeerID()
	privBytes, pubBytes, _ := activeAccount.GetIdentityKey()
	lockedPriv, err := privBytes.Open()
	if err != nil {
		return fmt.Errorf("failed to open private key enclave: %w", err)
	}
	defer lockedPriv.Destroy()

	copy(n.PubKey[:], pubBytes)

	tlsConfig, err := n.identity.GenerateTLSConfig()
	if err != nil {
		return fmt.Errorf("tls config failed: %w", err)
	}

	n.Dispatcher = dispatcher.NewDispatcher(n.Ctx)
	n.Dispatcher.SetLogger(n.Logger)
	socketBufBytes := n.Cfg.Network.SocketBufferMB * 1024 * 1024
	cc, err := congestion.Parse(n.Cfg.Network.Congestion, n.Cfg.Network.BrutalMbps)
	if err != nil {
		n.Logger.Warn("network: bad congestion setting, using BBR", "err", err)
		cc = congestion.Config{Mode: congestion.BBR}
	}
	n.Transport, err = network.NewQUICTransportWithOptions(
		n.Cfg.Network.ListenAddr,
		tlsConfig,
		network.GetQuicConfig(),
		n.identity,
		n.Cfg.Network.ProtocolVersion,
		network.TransportOptions{
			DirectSocket:      n.Cfg.Network.DirectSocket,
			SocketBufferBytes: socketBufBytes,
			Obfs:              n.Cfg.AntiDPI.Obfs != config.ObfsOff,
			OwnID:             n.ID,
			ObfsSecret:        n.Cfg.AntiDPI.ObfsPassword,
			Congestion:        cc,
		},
	)
	if err != nil {
		return fmt.Errorf("transport initialization failed: %w", err)
	}
	n.Transport.SetLogger(n.Logger.With("component", "quic_transport"))

	// A node that exposes an explicit public listener must mimic a real site in its own datacenter,
	// or it serves a mismatched SNI/IP that DPI flags; a pure client raises no such listener. When a
	// public node set no mimicry target, try to adopt a real neighbour automatically. If that fails
	// the node still runs — QUIC/UDP needs no camouflage — but without the public TCP/WS listener,
	// and keeps looking for a neighbour to turn TCP on later (retryMimicryTarget).
	n.ensureMimicryTarget()
	deferredTCP, skipWS := "", false
	if err := publicListenerConfigError(n.Cfg); err != nil {
		n.Logger.Warn("ANTI-DPI: no camouflage site found, so this node runs on UDP only for now: peers whose network blocks UDP cannot reach it. It keeps looking for a neighbour site and turns TCP on when it finds one; or set anti_dpi.tls_server_name and anti_dpi.camouflage_addr (see docs/anti-dpi.md)", "reason", err)
		if l := n.Cfg.Network.TCPListenAddr; l != "" && l != "off" && !loopbackListen(l) {
			deferredTCP = l
		}
		skipWS = n.Cfg.Network.WSListenAddr != "" && !loopbackListen(n.Cfg.Network.WSListenAddr)
	}

	var tcp *network.TCPTransport
	if n.Cfg.Network.TCPListenAddr != "off" {
		// A real certificate for your own domain makes an active prober see a genuine HTTPS site;
		// without one the node falls back to the self-signed identity certificate (probe-weak).
		camoCert := n.tcpCamouflageCert()
		stealth := n.Cfg.AntiDPI.StealthPassword
		if stealth == "" {
			stealth = n.Cfg.AntiDPI.ObfsPassword
		}
		mcfg := network.MimicryConfigFrom(network.MimicryOptions{
			StealthPassword: stealth,
			ServerName:      n.Cfg.AntiDPI.TLSServerName,
			CamouflageAddr:  n.Cfg.AntiDPI.CamouflageAddr,
		})
		if camoCert != nil {
			tcp = network.NewTCPTransport(*camoCert, mcfg,
				network.NewDefaultPeerAuthenticator(n.identity, n.Cfg.Network.ProtocolVersion, 5*time.Second))
			if n.Cfg.AntiDPI.Obfs != config.ObfsOff {
				tcp.SetNodeKeys(stealth, n.ID)
			}
			tcp.SetLogger(n.Logger.With("component", "tcp_transport"))
			if n.Cfg.AntiDPI.TLSCertFile == "" {
				n.Logger.Warn("TCP fallback uses a self-signed certificate; set anti_dpi.tls_cert_file for a real one to resist active probing")
			}
		}
	}
	tcpListen := n.Cfg.Network.TCPListenAddr
	if tcpListen == "off" || deferredTCP != "" {
		tcpListen = "" // dial out over TCP, accept on it only once a camouflage site is known
	}
	n.Net = network.NewFallbackTransport(n.Transport, tcp, n.Cfg.Network.Transport, tcpListen)
	n.Net.SetLogger(n.Logger.With("component", "transport"))
	n.Net.SetTCPRemotePort(n.Cfg.Network.TCPRemotePort)
	if n.Cfg.Network.AdvertisePort > 0 {
		n.Net.SetAdvertisedPort(n.Cfg.Network.AdvertisePort)
	}
	n.Net.SetCaps(n.announcedCaps())

	n.Swarm = swarm.NewSwarm(n.Ctx, n.Net, n.ID, n.Dispatcher, n.identity, n.Net.ConnChan(), n.Storage, n.Cfg)
	n.Swarm.SetLogger(n.Logger)
	n.BWProbe = bwprobe.NewForSwarm(n.Swarm)
	if l := bwprobe.DefaultLimits(); n.Cfg.Network.NoBWProbeServe || n.Cfg.Network.BWProbeMBPerMin > 0 {
		l.Serve = !n.Cfg.Network.NoBWProbeServe
		if mb := n.Cfg.Network.BWProbeMBPerMin; mb > 0 {
			l.BytesPerMinute = int64(mb) << 20
		}
		n.BWProbe.SetLimits(l)
	}
	n.ExitService = exit_gateway.NewExitService()
	n.ExitService.SetSelfIPs(n.publicIPs)
	exit_gateway.AttachToSwarm(n.Swarm, n.ExitService)

	// WebSocket transport: lets this node sit behind a CDN (when ws_listen_addr is set) and lets
	// it dial ws:// / wss:// peers either way
	if ws := n.buildWSTransport(); ws != nil {
		n.WS = ws
		n.Swarm.SetWSTransport(ws)
		if n.Cfg.Network.WSListenAddr != "" && skipWS {
			n.Logger.Warn("ANTI-DPI: the public WebSocket listener stays off until a camouflage site is configured", "addr", n.Cfg.Network.WSListenAddr)
		} else if n.Cfg.Network.WSListenAddr != "" {
			if err := ws.Listen(n.Cfg.Network.WSListenAddr); err != nil {
				n.Logger.Error("WebSocket listener failed", "addr", n.Cfg.Network.WSListenAddr, "err", err)
			} else {
				n.Logger.Info("WebSocket endpoint listening", "addr", n.Cfg.Network.WSListenAddr, "path", wsPath(n.Cfg))
			}
		}
	}

	// a server node checks once that its ServerHello matches the site it mimics, so an operator
	// learns if the chosen SNI gives it away; clients (no fallback listener configured) skip it
	tcpListening := tcpListen != ""
	if (n.Cfg.Network.WSListenAddr != "" && !skipWS) || tcpListening {
		go n.checkCamouflageConformance()
	}
	if deferredTCP != "" && tcp != nil {
		go n.retryMimicryTarget(deferredTCP, mimicryRetryEvery)
	}

	gsp := gossip.NewManager(n.Swarm)

	userKey, err := n.userKey.Open()
	if err != nil {
		return fmt.Errorf("failed to open user encryption key: %w", err)
	}
	defer userKey.Destroy()

	n.initTonWallet()

	server := n.Cfg.IsServer()
	if server {
		// a relay or exit holds no chats: frames for the services it does not run cost a map
		// lookup, before any signature check, decryption or disk write
		n.Swarm.DropFrameTypes(serverDroppedTypes...)
		n.Logger.Info("ROLE: server: chat, calls, file transfer and pre-key bundles are off; relay, mailboxes for others, DHT, signaling and exit run")
	}

	n.applyAccess()

	n.Discovery = discovery.NewDiscoveryService(n.Storage, gsp, n.Swarm, n.PubKey)

	if !server {
		x3dhInit, err := crypto.NewX3DHInitializer(activeAccount, n.Storage, userKey.Bytes())
		if err != nil {
			return fmt.Errorf("failed to initialize X3DH stack: %w", err)
		}
		n.crypto = crypto.NewCryptoEngine(n.ID, activeAccount, x3dhInit.GetEngine(), x3dhInit.GetSessionManager(), n.Swarm, n.Logger)

		n.Contact = contacts.NewContactService(n.Storage, n.Swarm)

		n.Messenger = messenger.NewMessageBroker(n.crypto, n.Swarm, gsp, n.identity, n.Storage.(storage.MessageStorage), n.Contact, n.Logger)
		n.Swarm.AddObserver(n.Messenger) // sends queued messages when their recipient connects
		n.Messenger.SetFilter(n.messageVerdict)
		n.Messenger.StartOutbox(n.Ctx)
	}

	routingTable := dht.NewRoutingTable(n.PubKey, n.Storage, n.Swarm)
	n.routingTable = routingTable
	dhtEngine := dht.NewEchoFogDHT(routingTable, n.Swarm)

	go n.Dispatcher.Start()
	if err = n.Net.ListenEarly(n.Ctx); err != nil {
		return fmt.Errorf("transport listen failed: %w", err)
	}

	// the port mDNS announces to the local network is the one QUIC actually bound
	actualPort := n.Transport.UDPPort()

	myID, _ := n.identity.GetPeerID()
	n.MDNS = discovery.NewMDNS(myID, actualPort, n.Swarm)

	n.DhtEngine = dhtEngine
	n.initVPNMarket()
	n.MailboxManager = messenger.NewMailboxManager(n.Storage)
	n.RelayServer = routing.NewRelayServer(n.Swarm, n.MailboxManager, n.Logger)
	n.RelayClient = routing.NewRelayClient(n.Swarm, dhtEngine, n.Logger)
	n.RelayServer.SetPolicy(n.Cfg.Relay.Server)
	n.RelayClient.SetPolicy(n.Cfg.Relay.Client)
	if !server {
		// route messages through a configured private relay even though it does not advertise itself
		n.Messenger.SetRelaySelector(n.RelayClient.IsPreferred)
	}
	if !n.isolated() {
		n.dialConfiguredRelays()
	}
	if !server {
		if err := n.startFiles(); err != nil {
			return fmt.Errorf("file transfers: %w", err)
		}
	}

	// Publish node's own PreKeyBundle locally and to DHT for asynchronous E2EE initiation, and keep
	// it there: the first publication happens before anybody is connected, so it is repeated. A
	// server publishes none: nobody can open a chat with it.
	n.Swarm.SetBundleFetcher(dhtEngine.Get)
	if !n.isolated() && !server {
		go n.publishBundleLoop(dhtEngine)
	}

	n.Services = services.NewServiceManager(n.Logger.With("component", "services"))

	routedServices := []services.Service{n.Discovery}
	if !server {
		routedServices = append(routedServices, n.Messenger)
	}
	routedServices = append(routedServices, dhtEngine, n.RelayServer)
	for _, s := range routedServices {
		if err := n.Services.RegisterRouted(s, n.Dispatcher); err != nil {
			return fmt.Errorf("failed to register routed service %s: %w", s.Name(), err)
		}
	}

	if n.Contact != nil {
		_ = n.Services.Register(n.Contact)
	}

	if !server {
		n.CallManager = call.NewCallManager(n.Swarm, nil, n.crypto, n.ID)
		n.CallManager.SetPeerConnector(n.EnsureConnected)
		_ = n.Services.Register(n.CallManager)
		// a message to somebody we are not connected to opens the connection (DHT, hole punch, relay)
		n.Messenger.SetConnector(func(peer types.PeerID) {
			ctx, cancel := context.WithTimeout(n.Ctx, 25*time.Second)
			defer cancel()
			_ = n.EnsureConnected(ctx, peer)
		})
	}

	if err := n.Services.StartAll(n.Ctx); err != nil {
		return fmt.Errorf("services startup failed: %w", err)
	}

	boot := n.BootstrapNodes()
	if n.isolated() {
		boot = nil // an isolated node joins no network: the people it invited come to it
	}
	n.Swarm.SetBootstrapNodes(boot)
	if len(boot) > 0 {
		n.Swarm.Bootstrap(boot)
	}

	n.Scheduler = scheduler.New(n.Ctx, n.Logger.With("component", "scheduler"))
	if n.EventBus != nil {
		n.Swarm.SetEmitter(n.EventBus)
		if n.CallManager != nil {
			n.CallManager.SetEmitter(n.EventBus)
		}
		n.DhtEngine.SetEmitter(n.EventBus)
	}

	// a node short of peers (it started offline, its network was cut, its peers left) dials the
	// bootstrap nodes, the peers it knows and the DHT's, backing off while that does not help
	_ = n.Scheduler.Schedule("swarm:maintain", 5*time.Second, func(ctx context.Context) error {
		if n.isolated() {
			return nil
		}
		if dialed := n.Swarm.MaintainPeers(); dialed > 0 {
			n.Logger.Info("Swarm: short of peers, dialing bootstrap nodes and known peers", "peers", len(n.Swarm.GetAllPeers()), "dialing", dialed)
		}
		return nil
	})
	if ps := n.Swarm.GetPeerStore(); ps != nil {
		ps.Prune(time.Now())
		_ = n.Scheduler.Schedule("peerstore:prune", time.Hour, func(ctx context.Context) error {
			ps.Prune(time.Now())
			return nil
		})
	}

	// peers connected only to signaling nodes have nobody to relay their traffic: find a relay
	if !n.isolated() {
		_ = n.Scheduler.Schedule("swarm:relay_peer", 20*time.Second, func(ctx context.Context) error {
			n.keepRelayPeer()
			return nil
		})
	}

	// Periodic Swarm inactive sessions prune (prevents zombie session heap leaks)
	if n.Swarm != nil {
		_ = n.Scheduler.Schedule("swarm:sessions_prune", 10*time.Minute, func(ctx context.Context) error {
			n.Swarm.PruneInactiveSessions(15 * time.Minute)
			return nil
		})
	}

	// expired offline messages stop taking disk and quota, and old recipients are forgotten
	if n.MailboxManager != nil {
		_ = n.Scheduler.Schedule("mailbox:cleanup", messenger.CleanupInterval, func(ctx context.Context) error {
			_, err := n.MailboxManager.CleanupExpired()
			return err
		})
	}

	// Periodic DHT cache cleanup and quotas reset
	if n.DhtEngine != nil {
		_ = n.Scheduler.Schedule("dht:cache_prune", 15*time.Minute, func(ctx context.Context) error {
			n.DhtEngine.PruneExpired()
			return nil
		})
		_ = n.Scheduler.Schedule("dht:quotas_reset", 1*time.Hour, func(ctx context.Context) error {
			n.DhtEngine.ResetQuotas()
			return nil
		})
	}

	n.startNAT()

	n.Logger.Info("Node P2P network started successfully", "listen_addr", n.Transport.Addr().String())
	return nil
}

// SetBootstrapNodes replaces the nodes this node joins the network through; a running node's
// maintainer uses the new list from its next round.
func (n *Node) SetBootstrapNodes(list []string) {
	n.bootMu.Lock()
	n.Cfg.Network.BootstrapNodes = append([]string(nil), list...)
	n.bootMu.Unlock()
	if n.Swarm != nil {
		n.Swarm.SetBootstrapNodes(list)
	}
}

// BootstrapNodes returns a copy of the configured bootstrap list.
func (n *Node) BootstrapNodes() []string {
	n.bootMu.Lock()
	defer n.bootMu.Unlock()
	return append([]string(nil), n.Cfg.Network.BootstrapNodes...)
}

// publishBundleLoop puts the node's key bundle in the DHT, soon after start-up (when the first
// connections exist) and then regularly, so that anybody can start an encrypted chat with it
// without a direct connection.
func (n *Node) publishBundleLoop(engine *dht.EchoFogDHT) {
	delays := []time.Duration{2 * time.Second, 10 * time.Second, 40 * time.Second}
	for i := 0; ; i++ {
		pause := 10 * time.Minute
		if i < len(delays) {
			pause = delays[i]
		}
		select {
		case <-n.Ctx.Done():
			return
		case <-time.After(pause):
		}
		bundle, err := n.Swarm.CreateMyPreKeyBundle()
		if err != nil {
			continue
		}
		_ = engine.Set(swarm.BundleKey(n.ID), bundle)
	}
}

// EnsureConnected connects to a peer by its ID: the DHT gives its address, a plain dial is tried,
// and when both sides are behind NATs a hole punch coordinated through a relay.
func (n *Node) EnsureConnected(ctx context.Context, peer types.PeerID) error {
	if n.Swarm.GetPeer(peer) != nil {
		return nil
	}
	// the peer may be unreachable by a plain dial (behind a NAT); that is not fatal, a hole punch
	// through a relay can still connect us, so a missing DHT address is not an error here
	addr, _ := n.DhtEngine.FindPeer(ctx, peer)
	return n.connectToPeer(ctx, peer, addr)
}

func (n *Node) Stop() { n.shutdown(true) }

// shutdown stops everything the node started. closeLog also releases the shared log file; a failed
// start keeps it, since the app goes on logging.
func (n *Node) shutdown(closeLog bool) {
	n.stopOnce.Do(func() {
		// 1. Cancel root context to signal all background loops
		if n.Cancel != nil {
			n.Cancel()
		}
		// 2. Stop node-wide scheduled tasks and wait for task completion
		if n.Scheduler != nil {
			n.Scheduler.Stop()
		}
		// 3. Gracefully stop all services in reverse DAG order
		if n.Services != nil {
			_ = n.Services.StopAll()
		}
		if n.Files != nil {
			n.Files.Stop() // downloads keep their progress for the next start
		}
		// 4. Close Swarm connections, streams, and wait for all swarm goroutines
		if n.Swarm != nil {
			n.Swarm.Close()
		}
		// 5. Stop Dispatcher and drain its worker queues
		if n.Dispatcher != nil {
			n.Dispatcher.Stop()
		}
		// 6. Close transport network sockets
		n.closeExitLedger()
		if n.WS != nil {
			_ = n.WS.Close()
		}
		if n.Net != nil {
			n.Net.Close()
		} else if n.Transport != nil {
			n.Transport.Close()
		}
		// 7. FINALLY close BadgerDB when no background goroutine can access it
		if n.Storage != nil {
			n.Storage.Close()
		}
		n.Logger.Info("Node stopped cleanly")
		if closeLog {
			logging.Close() // flush and release the log file
		}
	})
}

// serverDroppedTypes are the inbound frame and stream types a server-role node discards on
// arrival: direct chat (and group) messages, call signaling and media, file transfer, and requests
// for its pre-key bundle. RelayMessageRequest is not among them: it carries mail for others.
var serverDroppedTypes = []net_types.MessageType{
	net_types.TypeChatMessage,
	net_types.TypeCallSignaling,
	net_types.TypeCallMediaAudio,
	net_types.TypeCallMediaVideo,
	net_types.TypeFileXfer,
	net_types.TypeLocalPublicKeyBundleRequest,
}

// Role is the node's role: config.RoleServer or config.RoleClient.
func (n *Node) Role() string {
	if n.Cfg.IsServer() {
		return config.RoleServer
	}
	return config.RoleClient
}

func (n *Node) SendMessage(targetID types.PeerID, text string) error {
	if n.Messenger == nil {
		return errors.New("messenger not initialized")
	}
	return n.Messenger.SendChatMessage(targetID, text)
}

func (n *Node) GetMailbox() *messenger.MailboxManager {
	return n.MailboxManager
}

func (n *Node) GetMessenger() *messenger.MessageBroker {
	return n.Messenger
}

// announcedCaps is what the node tells peers it offers: always the DHT; a relay, or only help
// with hole punching (signaling), or neither.
func (n *Node) announcedCaps() handshake.Capability {
	if n.gated() {
		return handshake.CapNone // a hidden node is nobody's relay or DHT server: it is not listed
	}
	caps := handshake.CapDHTServer
	switch rs := n.Cfg.Relay.Server; {
	case rs.Enabled && rs.SignalingOnly:
		caps |= handshake.CapSignal
	case rs.Enabled && rs.Private:
		// a private relay is not advertised: strangers must not pick it. The people it allows
		// reach it by its address (relay.client.relays), which bypasses this advert.
	case rs.Enabled:
		caps |= handshake.CapRelay
	}
	return caps
}

// dialConfiguredRelays connects to the relays named in relay.client.relays, so the node can use a
// private relay it was given access to (by its "host:port/<peer id>" address).
func (n *Node) dialConfiguredRelays() {
	for _, addr := range n.Cfg.Relay.Client.Relays {
		a, id, ok := network.SplitPeerAddr(addr)
		if !ok {
			n.Logger.Warn("relay address needs a peer id (host:port/<peer id>)", "addr", addr)
			continue
		}
		n.Swarm.AddPotentialPeer(id, resolveRelayHost(a))
	}
}

// resolveRelayHost resolves the host of a relay address, like the bootstrap path does, so a domain
// name works. A ws:// URL or an unresolvable value is passed through unchanged.
func resolveRelayHost(addr string) string {
	if strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://") {
		return addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ips, err := net.LookupHost(host); err == nil && len(ips) > 0 {
		return net.JoinHostPort(ips[0], port)
	}
	return addr
}

func (n *Node) GetRelayServer() *routing.RelayServer {
	return n.RelayServer
}

func (n *Node) GetRelayClient() *routing.RelayClient {
	return n.RelayClient
}

func (n *Node) GetSelfPeerID() types.PeerID {
	if n.Swarm != nil {
		return n.Swarm.GetSelfID()
	}
	if n.identity != nil {
		id, _ := n.identity.GetPeerID()
		return id
	}
	return types.PeerID{}
}

// buildWSTransport makes the optional WebSocket transport, reusing the same camouflage certificate,
// mimicry config and per-node stealth key as the TCP fallback. It is created even without a
// listener so the node can dial ws:// peers; Listen is called separately when ws_listen_addr is set.
func (n *Node) buildWSTransport() *network.WSTransport {
	cert := n.tcpCamouflageCert()
	if cert == nil {
		return nil
	}
	stealth := n.Cfg.AntiDPI.StealthPassword
	if stealth == "" {
		stealth = n.Cfg.AntiDPI.ObfsPassword
	}
	mcfg := network.MimicryConfigFrom(network.MimicryOptions{
		StealthPassword: stealth,
		ServerName:      n.Cfg.AntiDPI.TLSServerName,
		CamouflageAddr:  n.Cfg.AntiDPI.CamouflageAddr,
	})
	ws := network.NewWSTransport(*cert, mcfg,
		network.NewDefaultPeerAuthenticator(n.identity, n.Cfg.Network.ProtocolVersion, 5*time.Second), wsPath(n.Cfg))
	if n.Cfg.AntiDPI.Obfs != config.ObfsOff {
		ws.SetNodeKeys(stealth, n.ID)
	}
	ws.SetCaps(n.announcedCaps())
	ws.SetLogger(n.Logger.With("component", "ws_transport"))
	return ws
}

func wsPath(cfg *config.AppConfig) string {
	if cfg.Network.WSPath != "" {
		return cfg.Network.WSPath
	}
	return "/echofog"
}

// checkCamouflageConformance probes the mimicked site once and logs whether our ServerHello
// matches its passive fingerprint (TLS version, cipher, key_share group).
func (n *Node) checkCamouflageConformance() {
	cert := n.tcpCamouflageCert()
	if cert == nil {
		return
	}
	serverName := n.Cfg.AntiDPI.TLSServerName
	if serverName == "" {
		serverName = "www.microsoft.com"
	}
	dest := n.Cfg.AntiDPI.CamouflageAddr
	if dest == "" {
		dest = net.JoinHostPort(serverName, "443")
	}
	ctx, cancel := context.WithTimeout(n.Ctx, 20*time.Second)
	defer cancel()
	verdict, ok := mimicry.CheckServerHelloConformance(ctx, mimicry.BuildServerTLSConfig(*cert), dest, serverName)
	if ok {
		n.Logger.Info("Camouflage check: " + verdict)
	} else {
		n.Logger.Warn("Camouflage check: " + verdict)
	}
}
