package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/access"
	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/logging"
	"github.com/echofog/echofog-core/pkg/node"
)

type options struct {
	keys         keyFlags
	listenAddr   string
	mnemonic     string
	mnemonicFile string
	noCreate     bool
	bootstrap    string

	transport, tcpListen string
	tcpRemotePort        int
	wsListen, wsPath     string
	advertisePort        int
	obfs                 string
	stealthPassword      string
	obfsPassword         string
	tlsServerName        string
	tonNetwork           string
	tlsCert, tlsKey      string
	camouflageAddr       string
	stunServers          string
	noPortMapping        bool
	noHolePunch          bool

	relay            bool
	relayPrivate     bool
	relayAllow       string
	relays           string
	signaling        bool
	relayMaxCircuits int

	socks5, useExit string

	exit       bool
	exitPrice  string
	exitFreeKB int
	exitMbps   int
	exitFree   string

	accessMode   string
	accessSecret string
	secretFile   string
	publicAddrs  string

	statusAddr string
	statsEvery time.Duration
	logLevel   string
}

func parseRunFlags(args []string) options {
	var o options
	f := flag.NewFlagSet("run", flag.ExitOnError)
	o.keys.register(f)
	f.StringVar(&o.listenAddr, "listen-addr", getEnv("ECHOFOG_LISTEN_ADDR", ":4242"), "P2P listen address (UDP for QUIC; the TCP fallback uses the same port unless --tcp-listen is set)")
	f.StringVar(&o.mnemonic, "mnemonic", getEnv("ECHOFOG_MNEMONIC", ""), "on first start, restore this identity instead of creating one (prefer --mnemonic-file)")
	f.StringVar(&o.mnemonicFile, "mnemonic-file", getEnv("ECHOFOG_MNEMONIC_FILE", ""), "on first start, restore the identity from the recovery phrase in this file")
	f.BoolVar(&o.noCreate, "no-create", envBool("ECHOFOG_NO_CREATE", false), "refuse to start without an identity instead of creating a new one")
	f.StringVar(&o.bootstrap, "bootstrap", getEnv("ECHOFOG_BOOTSTRAP", ""), "comma-separated host:port/peerid of nodes to join through (host may be a domain name; default: the built-in seed nodes)")

	f.StringVar(&o.transport, "transport", getEnv("ECHOFOG_TRANSPORT", ""), "auto (QUIC, then TCP), quic, or tcp")
	f.StringVar(&o.tcpListen, "tcp-listen", getEnv("ECHOFOG_TCP_LISTEN", ""), "TCP fallback listen address, e.g. :443 (default: the --listen-addr port); 'off' disables it")
	f.IntVar(&o.tcpRemotePort, "tcp-remote-port", int(envInt64("ECHOFOG_TCP_REMOTE_PORT", 0)), "port peers use for the TCP fallback when it differs from the QUIC port (e.g. 443)")
	f.StringVar(&o.wsListen, "ws-listen", getEnv("ECHOFOG_WS_LISTEN", ""), "listen for peers over WebSocket on this address, to sit behind a CDN like Cloudflare (e.g. 127.0.0.1:8080 with the CDN in front); empty disables it")
	f.StringVar(&o.wsPath, "ws-path", getEnv("ECHOFOG_WS_PATH", ""), "HTTP path the WebSocket endpoint answers on (default /echofog)")
	f.IntVar(&o.advertisePort, "advertise-port", int(envInt64("ECHOFOG_ADVERTISE_PORT", 0)), "public port peers use to reach this node when it differs from the listen port (e.g. 443 when a port map publishes host 443 to this node); 0 = the listen port")
	f.StringVar(&o.obfs, "obfs", getEnv("ECHOFOG_OBFS", ""), "UDP obfuscation: node (default; keys derive from each node's peer ID) or off")
	f.StringVar(&o.stealthPassword, "stealth-password", getEnv("ECHOFOG_STEALTH_PASSWORD", ""), "optional extra secret for a private network (TCP camouflage); every node of that network must use the same")
	f.StringVar(&o.obfsPassword, "obfs-password", getEnv("ECHOFOG_OBFS_PASSWORD", ""), "optional extra secret for a private network (UDP obfuscation); every node of that network must use the same")
	f.StringVar(&o.tonNetwork, "ton-network", getEnv("ECHOFOG_TON_NETWORK", ""), "TON network of the node's wallet: testnet or mainnet (default: "+config.DefaultTonNetwork+")")
	f.StringVar(&o.tlsServerName, "tls-server-name", getEnv("ECHOFOG_TLS_SERVER_NAME", ""), "domain the TCP fallback presents; use your own domain together with --tls-cert")
	f.StringVar(&o.tlsCert, "tls-cert", getEnv("ECHOFOG_TLS_CERT", ""), "PEM certificate for --tls-server-name (without one a self-signed certificate is used)")
	f.StringVar(&o.tlsKey, "tls-key", getEnv("ECHOFOG_TLS_KEY", ""), "PEM private key for --tls-cert")
	f.StringVar(&o.camouflageAddr, "camouflage-addr", getEnv("ECHOFOG_CAMOUFLAGE_ADDR", ""), "host:port that scanners probing the TCP port are forwarded to")
	f.StringVar(&o.stunServers, "stun-servers", getEnv("ECHOFOG_STUN_SERVERS", ""), "comma-separated STUN servers (default: a built-in list)")
	f.BoolVar(&o.noPortMapping, "no-port-mapping", envBool("ECHOFOG_NO_PORT_MAPPING", false), "do not ask the router to forward our port (UPnP / NAT-PMP)")
	f.BoolVar(&o.noHolePunch, "no-hole-punch", envBool("ECHOFOG_NO_HOLE_PUNCH", false), "do not try direct connections between peers behind NATs")

	f.BoolVar(&o.relay, "relay", envBool("ECHOFOG_RELAY", true), "relay calls and messages for peers that cannot reach each other directly, and keep messages for offline peers")
	f.BoolVar(&o.relayPrivate, "relay-private", envBool("ECHOFOG_RELAY_PRIVATE", false), "make the relay private: it is not advertised and serves only the peers listed in --relay-allow")
	f.StringVar(&o.relayAllow, "relay-allow", getEnv("ECHOFOG_RELAY_ALLOW", ""), "comma-separated peer IDs allowed to use this relay when it is private")
	f.StringVar(&o.relays, "relays", getEnv("ECHOFOG_RELAYS", ""), "comma-separated relays to connect to and prefer, as host:port/<peer id> (use to reach a private relay you were given access to)")
	f.BoolVar(&o.signaling, "signaling", envBool("ECHOFOG_SIGNALING", false), "signaling only: keep the DHT and help peers behind NATs connect directly (hole punching), but relay no traffic and keep no messages")
	f.IntVar(&o.relayMaxCircuits, "relay-max-circuits", int(envInt64("ECHOFOG_RELAY_MAX_CIRCUITS", 200)), "most relayed connections at once")

	f.StringVar(&o.socks5, "socks5", getEnv("ECHOFOG_SOCKS5", "127.0.0.1:1080"), "local SOCKS5 proxy address used with --use-exit")
	f.StringVar(&o.useExit, "use-exit", getEnv("ECHOFOG_USE_EXIT", ""), "route the local SOCKS5 proxy through an exit: 'auto', a peer ID or an invitation link (echofog://join?...)")

	f.BoolVar(&o.exit, "exit", envBool("ECHOFOG_EXIT", false), "share this connection as an exit node (a public node also publishes it to the DHT; a hidden or isolated one serves only the people with its link)")
	f.StringVar(&o.exitPrice, "exit-price", getEnv("ECHOFOG_EXIT_PRICE", "0"), "exit price in dollars per GB, paid in TON or USDT, e.g. 0.05 (0 = free)")
	f.IntVar(&o.exitFreeKB, "exit-free-kb", int(envInt64("ECHOFOG_EXIT_FREE_KB", 512)), "free test traffic per client per day in KB (0-1024, used when the exit is priced)")
	f.IntVar(&o.exitMbps, "exit-upload-mbps", int(envInt64("ECHOFOG_EXIT_UPLOAD_MBPS", 0)), "upload speed to advertise in Mbit/s (0 = measure against peers, which needs peers connected first; set it on a server)")
	f.StringVar(&o.exitFree, "exit-free-peers", getEnv("ECHOFOG_EXIT_FREE_PEERS", ""), "comma-separated peer IDs this exit serves for free, without quota or payment (e.g. your own devices); replaces the list saved in the data directory")

	f.StringVar(&o.accessMode, "access", getEnv("ECHOFOG_ACCESS", "public"), "who may reach this node: public (listed, its exit advertised), hidden (uses the network as a client, unlisted, answers only people with its link) or isolated (joins no network at all, people with its link connect to it directly; for a server)")
	f.StringVar(&o.accessSecret, "access-secret", getEnv("ECHOFOG_ACCESS_SECRET", ""), "access secret of a hidden or isolated node, 22 characters of base64url (default: generated on first start and kept in the data directory); the same secret on several nodes lets one link in everywhere")
	f.StringVar(&o.secretFile, "access-secret-file", getEnv("ECHOFOG_ACCESS_SECRET_FILE", ""), "file holding the access secret (for example a Docker or Kubernetes secret); used when --access-secret is empty")
	f.StringVar(&o.publicAddrs, "public-addr", getEnv("ECHOFOG_PUBLIC_ADDR", ""), "comma-separated addresses put into the invitation link (host:port or a ws:// URL), when the observed one is not what people should dial")

	f.StringVar(&o.statusAddr, "status-addr", getEnv("ECHOFOG_STATUS_ADDR", "127.0.0.1:7070"), "local address of the status endpoint used by \"status\" and \"wallet\" ('' disables it; keep it on loopback)")
	every, _ := time.ParseDuration(getEnv("ECHOFOG_STATS_EVERY", "1m"))
	f.DurationVar(&o.statsEvery, "stats-every", every, "how often to log a line with peers, relayed traffic and earnings (0 = never)")
	f.StringVar(&o.logLevel, "log-level", getEnv("ECHOFOG_LOG_LEVEL", "info"), "debug, info, warn or error")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: echofog-node run [flags]\n\nStarts the node. Without an identity in the data directory it creates one and prints the\nrecovery phrase (or restores one with --mnemonic-file).")
		f.PrintDefaults()
	}
	_ = f.Parse(args)
	return o
}

func (o options) config() *config.AppConfig {
	cfg := config.DefaultConfig("")
	cfg.Network.ListenAddr = o.listenAddr
	cfg.Storage.DatabasePath = filepath.Join(o.keys.dataDir, "db")
	if b := splitList(o.bootstrap); len(b) > 0 {
		cfg.Network.BootstrapNodes = b
	}
	if o.transport != "" {
		cfg.Network.Transport = o.transport
	}
	// A server node accepts TCP peers by default, on the QUIC port: it is the camouflaged path for
	// people whose network blocks UDP. --tcp-listen moves it (e.g. :443) or turns it off.
	if o.tcpListen != "" {
		cfg.Network.TCPListenAddr = o.tcpListen
	} else {
		cfg.Network.TCPListenAddr = o.listenAddr
	}
	cfg.Network.TCPRemotePort = o.tcpRemotePort
	cfg.Network.AdvertisePort = o.advertisePort
	cfg.Network.WSListenAddr = o.wsListen
	if o.wsPath != "" {
		cfg.Network.WSPath = o.wsPath
	}
	if s := splitList(o.stunServers); len(s) > 0 {
		cfg.Network.STUNServers = s
	}
	if o.noPortMapping {
		cfg.Network.PortMapping = false
	}
	if o.noHolePunch {
		cfg.Network.HolePunch = false
	}
	if o.obfs != "" {
		cfg.AntiDPI.Obfs = o.obfs
	}
	cfg.AntiDPI.ObfsPassword = o.obfsPassword
	cfg.AntiDPI.StealthPassword = o.stealthPassword
	switch o.tonNetwork {
	case "":
	case "testnet", "mainnet":
		cfg.Ton.Network = o.tonNetwork
	default:
		fatal("--ton-network: %q is not testnet or mainnet", o.tonNetwork)
	}
	if o.tlsServerName != "" {
		cfg.AntiDPI.TLSServerName = o.tlsServerName
	}
	cfg.AntiDPI.TLSCertFile = o.tlsCert
	cfg.AntiDPI.TLSKeyFile = o.tlsKey
	cfg.AntiDPI.CamouflageAddr = o.camouflageAddr
	cfg.Relay.Server.Enabled = o.relay || o.signaling
	cfg.Relay.Server.SignalingOnly = o.signaling
	cfg.Relay.Server.Private = o.relayPrivate
	cfg.Relay.Server.AllowedPeers = splitCSV(o.relayAllow)
	cfg.Relay.Client.Relays = splitCSV(o.relays)
	if o.relayMaxCircuits > 0 {
		cfg.Relay.Server.MaxCircuits = o.relayMaxCircuits
	}
	switch o.accessMode {
	case "", config.AccessPublic:
	case config.AccessHidden, config.AccessIsolated:
		cfg.Access.Mode = o.accessMode
		// a hidden node is nobody's relay: strangers must not learn it exists
		cfg.Relay.Server.Enabled = false
	default:
		fatal("--access: %q is not public, hidden or isolated", o.accessMode)
	}
	cfg.Access.Secret = o.accessSecret
	if cfg.Access.Secret == "" && o.secretFile != "" {
		raw, err := os.ReadFile(o.secretFile)
		if err != nil {
			fatal("--access-secret-file: %v", err)
		}
		cfg.Access.Secret = strings.TrimSpace(string(raw))
	}
	cfg.Access.PublicAddrs = splitCSV(o.publicAddrs)
	return cfg
}

// splitCSV turns a comma-separated flag value into a trimmed, non-empty list.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func listenPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return strings.TrimPrefix(addr, ":")
}

func runCmd(args []string) int {
	o := parseRunFlags(args)
	fmt.Println("EchoFog node", version)
	if o.logLevel != "" {
		_ = os.Setenv("ECHOFOG_LOG_LEVEL", o.logLevel) // read by the node's logger
	}
	if os.Getenv("ECHOFOG_LOG_FILE") == "" {
		_ = os.Setenv("ECHOFOG_LOG_FILE", logging.DefaultFilePath()) // persist logs for a running node
	}
	if o.obfs != "" && o.obfs != config.ObfsNode && o.obfs != config.ObfsOff {
		fatal("--obfs: %q is not node or off", o.obfs)
	}
	price, err := parsePricePerGB(o.exitPrice)
	if err != nil {
		fatal("--exit-price: %v", err)
	}
	if err := os.MkdirAll(o.keys.dataDir, 0o700); err != nil {
		fatal("cannot create the data directory: %v", err)
	}
	pass, err := o.keys.nodePassword()
	if err != nil {
		fatal("cannot set up the identity password: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	echoNode := node.NewNode(ctx, o.config(), "echofog-node")

	switch {
	case echoNode.IsRegistered():
		fmt.Println("Existing identity found, logging in")
		if err := echoNode.Login([]byte(pass)); err != nil {
			fatal("login failed: %v (a wrong --password, or the data directory belongs to another password)", err)
		}
	case o.mnemonic != "" || o.mnemonicFile != "":
		phrase, err := readMnemonic(o.mnemonic, o.mnemonicFile)
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println("Restoring the identity from the recovery phrase")
		if err := echoNode.Recover(phrase, nil, []byte(pass)); err != nil {
			fatal("cannot restore the identity: %v", err)
		}
	case o.noCreate:
		fatal("no identity in %s: create one with \"echofog-node init\" or restore it with \"echofog-node restore\"", o.keys.dataDir)
	default:
		fmt.Println("No identity yet, creating a new one")
		phrase, err := echoNode.Register([]byte(pass), nil)
		if err != nil {
			fatal("cannot create the identity: %v", err)
		}
		printPhrase(phrase)
	}

	if err := echoNode.Start(); err != nil {
		fatal("cannot start the node: %v", err)
	}
	started := time.Now()

	h := api.NewNodeApi(echoNode)
	if free := splitCSV(o.exitFree); len(free) > 0 {
		if err := h.SetExitFreePeers(free); err != nil {
			fatal("--exit-free-peers: %v", err)
		}
	}
	if o.exit {
		go startSharing(h, price, o.exitFreeKB, o.exitMbps, echoNode.AccessMode() != config.AccessPublic)
	}
	if o.useExit != "" {
		go startClientVPN(h, o.useExit, o.socks5)
	}
	go logTopExits(ctx, h)

	col := newCollector(echoNode, h, o, started)
	go col.run(ctx)
	if o.statusAddr != "" {
		if err := serveStatus(ctx, o.statusAddr, col); err != nil {
			fmt.Printf("Status endpoint not started: %v\n", err)
		}
	}
	if o.statsEvery > 0 {
		go col.logEvery(ctx, o.statsEvery)
	}

	printSummary(o, echoNode, h)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nShutting down...")
	if o.useExit != "" {
		_ = h.StopVPN()
	}
	if o.exit {
		_ = h.StopSharing()
	}
	echoNode.Stop()
	fmt.Println("Stopped.")
	return 0
}

func printSummary(o options, n *node.Node, h *api.NodeApi) {
	peerID := n.ID.String()
	fmt.Println("------------------------------------------------------------------")
	fmt.Printf("Peer ID:       %s\n", peerID)
	fmt.Printf("Listening on:  %s (UDP, and TCP for the fallback)\n", o.listenAddr)
	fmt.Printf("Join address:  %s\n", joinAddress(n, o.listenAddr))
	if w, err := h.GetTonWallet(); err == nil {
		fmt.Printf("TON wallet:    %s (%s)\n", w.Address, w.Network)
	}
	gated := n.AccessMode() != config.AccessPublic
	switch n.AccessMode() {
	case config.AccessHidden:
		fmt.Println("Access:        hidden: not listed, answers only people with its link and those who joined")
	case config.AccessIsolated:
		fmt.Println("Access:        isolated: joins no network, people with its link connect to it directly")
	}
	if gated {
		if link, err := n.InviteLink(); err == nil {
			fmt.Printf("Invite link:   %s\n", link)
		} else {
			fmt.Println("Invite link:   echofog-node invite   (once the node knows its public address)")
		}
	}
	if gated {
		fmt.Println("Relay:         off (a hidden node relays for nobody)")
	} else if o.signaling {
		fmt.Println("Relay:         signaling only: DHT and hole punching help, no traffic is relayed")
	} else if o.relay && o.relayPrivate {
		fmt.Printf("Relay:         private, up to %d connections at once, serving %d allowed peer(s)\n", o.relayMaxCircuits, len(splitCSV(o.relayAllow)))
		fmt.Println("               (not advertised; allowed peers reach it with --relays " + joinAddress(n, o.listenAddr) + ")")
	} else if o.relay {
		fmt.Printf("Relay:         public, up to %d connections at once\n", o.relayMaxCircuits)
	} else {
		fmt.Println("Relay:         off")
	}
	if rc := len(splitCSV(o.relays)); rc > 0 {
		fmt.Printf("Using relays:  %d configured\n", rc)
	}
	if o.exit {
		fmt.Printf("Exit:          sharing, $%s per GB (paid in TON or USDT), %d KB free per client per day\n", o.exitPrice, o.exitFreeKB)
		if free := h.GetExitFreePeers(); len(free) > 0 {
			fmt.Printf("               free without limits for %d peer(s) on the free list\n", len(free))
		}
	}
	if o.useExit != "" {
		exit := o.useExit
		if access.IsLink(exit) {
			exit = "from the invitation link" // the link carries the secret: keep it out of the log
		}
		fmt.Printf("SOCKS5 proxy:  %s through exit %s\n", localProxyAddr(o.socks5), exit)
	}
	if o.statusAddr != "" {
		fmt.Printf("Status:        echofog-node status   (http://%s/status)\n", o.statusAddr)
	}
	fmt.Println("------------------------------------------------------------------")
	fmt.Println("Running. Press Ctrl+C to stop.")
}

// joinAddress is what other nodes put in --bootstrap to join through this one. The public IP is
// known once a STUN server or a peer has told us; until then it is a placeholder.
func joinAddress(n *node.Node, listen string) string {
	host := "<this server's public IP or domain>"
	if n.NAT != nil {
		if pub := n.NAT.GetPublicAddr(); pub != "" {
			if h, _, err := net.SplitHostPort(pub); err == nil {
				host = h
			}
		}
	}
	port := listenPort(listen)
	if n.Cfg.Network.AdvertisePort > 0 {
		port = strconv.Itoa(n.Cfg.Network.AdvertisePort)
	}
	return host + ":" + port + "/" + n.ID.String()
}
