// Package config holds the settings of an EchoFog node and their defaults.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CurrentProtocolVersion is the peer protocol version this build speaks.
// 101: mutual peer authentication bound to the transport session.
const CurrentProtocolVersion uint32 = 102

// NetworkName names the network this build belongs to. It goes into invitation links, so the app
// can tell a link of another network (a fork) from one it cannot read. A fork sets its own at
// build time:
//
//	go build -ldflags "-X github.com/echofog/echofog-core/internal/config.NetworkName=myfork"
var NetworkName = "echofog"

// Values of AccessConfig.Mode.
const (
	// AccessPublic: the node is listed like any other and its exit, when shared, is advertised.
	AccessPublic = "public"
	// AccessHidden: the node uses the network as a client (finds peers, relays, hole punching) but
	// is not listed, does not advertise its exit and answers only the peers it invited, its
	// contacts and those who joined before.
	AccessHidden = "hidden"
	// AccessIsolated: the node does not take part in the network at all. It dials nobody and waits
	// for the people it invited to connect to it directly. For a VPS with a public address.
	AccessIsolated = "isolated"
)

// AccessConfig: who may reach this node.
type AccessConfig struct {
	// Mode is AccessPublic (default), AccessHidden or AccessIsolated.
	Mode string `json:"mode"`
	// Secret is the access secret of a hidden or isolated node, base64url (22 characters). Empty:
	// one is generated on first start and kept in the profile. Set it to run several nodes with
	// one link, or to keep it in a secret store.
	Secret string `json:"secret,omitempty"`
	// PublicAddrs are the addresses put into the invitation link ("host:port", or a ws:// URL),
	// when the ones this node observes are not what people should dial (a domain, a port map).
	PublicAddrs []string `json:"public_addrs,omitempty"`
}

// Gated reports whether the node keeps to the people it invited.
func (a AccessConfig) Gated() bool { return a.Mode == AccessHidden || a.Mode == AccessIsolated }

// Values of AppConfig.Role.
const (
	// RoleClient runs everything a person needs: chat, contacts, calls, file transfer, and answers
	// the pre-key bundle requests that start an encrypted chat. The default.
	RoleClient = "client"
	// RoleServer is for a relay or exit on a VPS: it keeps the network part (DHT, peer exchange,
	// relay with mailboxes for others, signaling, exit, adverts, wallet) and runs none of the chat,
	// call and file services, so strangers cannot make it decrypt or store anything for itself.
	RoleServer = "server"
)

// Values of AntiDPIConfig.Obfs.
const (
	ObfsNode = "node"
	ObfsOff  = "off"
)

// DefaultBootstrapNodes are the public nodes a fresh install joins the network through
// ("host:port/<peer id>?sni=<site it imitates>&srv=1", so the very first dial is disguised). Operators add their own with the bootstrap option of the node, and app
// users in Settings. The name stays valid when the node moves to another server; the addresses
// still work where its DNS is blocked.
var DefaultBootstrapNodes = []string{
	"34.158.189.116:443/5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d?sni=d-hp-integrationdb-stag.hp-infra.net&srv=1",
	"34.32.224.253:443/98466572ebb322301eec59937bebc6677023c909471344ff493d036dbb9f8ad0?sni=aifraud.ru&srv=1",
	"echofog-node.duckdns.org:443/5f0909b6648e4adbc08c4d89d9d31ed21d2349ba01ba3cddbea34ef9af2a324d?sni=d-hp-integrationdb-stag.hp-infra.net&srv=1",
}

// SeedNodes adds seed nodes at build time, comma-separated host:port/peerid (the host may be a
// domain name), so a release carries them without a code change:
//
//	go build -ldflags "-X github.com/echofog/echofog-core/internal/config.SeedNodes=seed1.example.org:4242/<peerid>,..."
var SeedNodes = ""

func defaultBootstrap() []string {
	out := append([]string(nil), DefaultBootstrapNodes...)
	for _, s := range strings.Split(SeedNodes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type NetworkConfig struct {
	ListenAddr      string   `json:"listen_addr"`
	BootstrapNodes  []string `json:"bootstrap_nodes"`
	MaxConnections  int      `json:"max_connections"`
	ProtocolVersion uint32   `json:"protocol_version"`
	DirectSocket    bool     `json:"direct_socket"`    // Bypass the DPI/STUN multiplexer for raw 1Gbps+ throughput on public IPs
	SocketBufferMB  int      `json:"socket_buffer_mb"` // Kernel UDP socket buffer size (MB), defaults to 16-32MB

	// Congestion control of QUIC links: "bbr" (default; keeps its speed on lossy links),
	// "cubic" (quic-go's own, which collapses at about 1% loss) or "brutal", which sends at
	// BrutalMbps whatever is lost. Brutal is for a user who knows their upload rate: set too
	// high it floods the line.
	Congestion string `json:"congestion,omitempty"`
	BrutalMbps int    `json:"brutal_mbps,omitempty"`

	// Transport: "auto" (QUIC, TCP/TLS when UDP does not get through), "quic" or "tcp"
	Transport string `json:"transport"`
	// TCPListenAddr for the TCP/TLS fallback. Empty: no inbound TCP listener — a pure client that
	// only dials out over TCP when UDP is blocked (the common case; needs no domain or certificate).
	// An explicit address (e.g. ":443") exposes a public TCP listener and then requires
	// anti_dpi.tls_server_name + anti_dpi.camouflage_addr (see docs/anti-dpi.md); on Linux binding
	// 443 needs CAP_NET_BIND_SERVICE or a redirect. "off" disables the TCP fallback entirely (the
	// node will not even dial out over TCP).
	TCPListenAddr string `json:"tcp_listen_addr"`
	// TCPRemotePort is the port peers are dialled on for the TCP fallback when the network runs
	// it on a fixed port (e.g. 443) separate from QUIC. 0 means use the same port as the address.
	TCPRemotePort int `json:"tcp_remote_port"`

	// AdvertisePort is the public port peers should use to reach this node, when it differs from
	// the listen port because of a port map (e.g. Docker publishes host 443 to the container's
	// 4242). It sets the port in the printed join address and the one announced for the TCP
	// fallback. 0 = use the listen port.
	AdvertisePort int `json:"advertise_port"`

	// AllowPrivateAddrs lets private, link-local and CGNAT peer addresses into the DHT and peer
	// exchange: for a network of private addresses only (a LAN, a Docker test cluster). Off, only
	// public addresses are handed on, as one from somebody's LAN means nothing elsewhere. A node
	// listening on loopback accepts loopback addresses either way (tests on one machine).
	AllowPrivateAddrs bool `json:"allow_private_addrs"`

	// NoAutoDial makes the node dial only what it is told to (the bootstrap list at start, explicit
	// connects, DHT record lookups): no peers from peer exchange, the maintainer or the DHT's own
	// lookups. For tests that build a fixed topology on one machine, where every node could reach
	// every other.
	NoAutoDial bool `json:"no_auto_dial"`

	// Limits on the peers that connect to this node; 0 is the default, -1 no limit (see "Connections
	// and bans" in docs/run-a-node.md). MaxConnections above is how many peers the node dials by
	// itself; these bound what others open towards it.
	MaxInboundPeers   int `json:"max_inbound_peers,omitempty"`    // default 256, 2048 on a server
	MaxPeersPerIP     int `json:"max_peers_per_ip,omitempty"`     // at once from one address (16)
	MaxPeersPerSubnet int `json:"max_peers_per_subnet,omitempty"` // from one /24 or IPv6 /56 (64)
	MaxIDsPerIP       int `json:"max_ids_per_ip,omitempty"`       // different peer IDs per address in 10 minutes (32)
	MaxIDsPerSubnet   int `json:"max_ids_per_subnet,omitempty"`   // and per subnet (128)
	// WSClientIPHeader names the header a CDN puts the client's address in (CF-Connecting-IP for
	// Cloudflare), so the limits count WebSocket peers by their own address and not the CDN's.
	// Set it only when the WebSocket listener is reachable through the CDN alone.
	WSClientIPHeader string `json:"ws_client_ip_header,omitempty"`

	// WSListenAddr makes the node reachable over WebSocket, so it can sit behind a CDN such as
	// Cloudflare (the CDN forwards the WebSocket to this address). Empty disables the listener;
	// the node can still dial ws:// peers without it. Peers reach it at ws(s)://host:port<ws_path>.
	WSListenAddr string `json:"ws_listen_addr"`
	// WSPath is the HTTP path the WebSocket endpoint answers on (default "/echofog").
	WSPath string `json:"ws_path"`

	// STUNServers are asked which public address they see us at. Empty uses a built-in list of
	// servers from unrelated operators; replace it for networks where those are blocked.
	STUNServers []string `json:"stun_servers"`
	// PortMapping asks the home router (UPnP, NAT-PMP) to forward our port, so peers can reach us
	// directly and we can serve as a relay.
	PortMapping bool `json:"port_mapping"`
	// HolePunch lets two peers behind NATs open a direct connection, coordinated through a relay.
	HolePunch bool `json:"hole_punch"`

	// NoBWProbeServe stops the node answering other peers' download speed probes, a 12 MB burst
	// each. BWProbeMBPerMin caps what answering them sends a minute, all peers together (0 = 48).
	NoBWProbeServe  bool `json:"no_bwprobe_serve,omitempty"`
	BWProbeMBPerMin int  `json:"bwprobe_mb_per_min,omitempty"`
}

type AntiDPIConfig struct {
	// Obfs selects the UDP obfuscation: "node" (the default) makes every packet look like random
	// bytes, keyed by the peer ID of the node it goes to, so there is no key shared by the network
	// and none in the program; "off" sends QUIC as it is. With "node", peers are dialed as
	// "host:port/<peer id>".
	Obfs string `json:"obfs"`
	// ObfsPassword is an optional extra secret mixed into every node key, for a private network
	// whose members all know it. Leave it empty for the public network.
	ObfsPassword string `json:"obfs_password"`

	// TCP/TLS camouflage for the fallback transport (used when UDP is blocked). A client proves
	// itself with a token keyed by the peer ID of the node it dials, so scanners that do not know
	// the ID are served the decoy site. StealthPassword is an optional extra secret for a private
	// network; empty uses ObfsPassword.
	StealthPassword string `json:"stealth_password"`
	// TLSServerName is the SNI the fallback presents and mimics, e.g. your own domain. With a
	// real certificate for it (TLSCertFile/TLSKeyFile) an active prober sees a genuine HTTPS
	// site; without one the node falls back to a self-signed certificate (probe-weak).
	TLSServerName string `json:"tls_server_name"`
	TLSCertFile   string `json:"tls_cert_file"`
	TLSKeyFile    string `json:"tls_key_file"`
	// CamouflageAddr is the real "host:port" an active probe is transparently forwarded to
	// (normally a benign page on your own domain). Empty serves a small static page instead.
	CamouflageAddr string `json:"camouflage_addr"`

	// FlowShaping bounds and shapes exit gateway peer sessions to prevent DPI fingerprinting.
	FlowShaping FlowShapingConfig `json:"flow_shaping"`

	// TunnelPadding turns on the TLS-in-TLS defense: the opening of each exit-tunnel data phase is
	// carried in padded frames so a censor cannot see the inner TLS handshake's size/timing shape.
	// A node announces handshake.CapTunnelPadding only when this is on, and a peer pads to it only
	// when it sees that capability, so old nodes interoperate in the plain (unpadded) mode.
	TunnelPadding bool `json:"tunnel_padding"`

	// MicroChunkTCP turns on sub-16KB micro-chunking of the exit-tunnel TCP fallback data phase, to
	// defeat the Russian TSPU "TCP freeze after ~16KB" behaviour (a single TCP socket to a foreign
	// datacentre is throttled once its cumulative bytes cross ~16 KB). The tunnel is carried as
	// self-describing chunks tagged (TunnelID, Seq), reassembled at the exit, and the writer rotates
	// to a fresh socket before any one crosses the freeze counter. Off by default and opt-in: it adds
	// per-chunk framing overhead and only helps on networks that run the TSPU freeze, so turn it on
	// for Russian egress (or let the region signal engage it where HomeCountry/GeoIP places the path
	// in RU). The normal path and the single-region testbed are untouched when it is off.
	MicroChunkTCP bool `json:"micro_chunk_tcp"`

	// RegionAwareObfs uses the cheaper noise obfuscation for connections that stay inside this
	// node's own country or on the local network, and keeps QUIC-REALITY for cross-border ones.
	// REALITY mimics an allowed foreign site so a connection survives a censoring border; a
	// connection that never crosses one does not need it. A peer whose country cannot be placed
	// stays on REALITY, so this never weakens a cross-border link. Off by default; opt-in, because
	// "same country" is not safe where domestic traffic is censored too (noise is what GFW-style
	// DPI flags): turn it on only on networks that do not censor inside their own borders.
	RegionAwareObfs bool `json:"region_aware_obfs"`
	// HomeCountry pins this node's own country as an ISO 3166-1 alpha-2 code (e.g. "DE"). Empty
	// lets the node place its own public address with GeoIP; set it when that lookup is blocked, or
	// to fix the region signal in a controlled network.
	HomeCountry string `json:"home_country,omitempty"`
	// AllowNoiseToServers lets a client dial a server-role node across the border over noise when
	// that node has no known camouflage SNI. Off by default: noise across a border is what a censor
	// flags, so the client waits for the SNI (it arrives with the address) or does not dial. Turn it
	// on to reach your own servers that have no cover site, from a network that does not censor.
	AllowNoiseToServers bool `json:"allow_noise_to_servers,omitempty"`
}

// FlowShapingConfig bounds and shapes exit gateway peer sessions to prevent DPI fingerprinting.
type FlowShapingConfig struct {
	MaxConnections     int           `json:"max_connections"`
	MaxConcurrency     int           `json:"max_concurrency"`
	MaxBytesMin        int64         `json:"max_bytes_min"`
	MaxBytesMax        int64         `json:"max_bytes_max"`
	MaxConnLifetimeMin time.Duration `json:"max_conn_lifetime_min"`
	MaxConnLifetimeMax time.Duration `json:"max_conn_lifetime_max"`
	MaxStreamsPerConn  int           `json:"max_streams_per_conn"`
	MinDialInterval    time.Duration `json:"min_dial_interval"`
}

// DefaultFlowShapingConfig returns the default FlowShaping settings.
func DefaultFlowShapingConfig() FlowShapingConfig {
	return FlowShapingConfig{
		// MaxConnections bounds the parallel session pool to one exit: the swarm keeps up to this
		// many concurrent connections and spreads streams across them, so a single stalled or
		// censor-killed connection does not stall the VPN (2-4 is the useful range).
		MaxConnections:     3,
		MaxConcurrency:     8,
		MaxBytesMin:        15 * 1024 * 1024,
		MaxBytesMax:        45 * 1024 * 1024,
		MaxConnLifetimeMin: 120 * time.Second,
		MaxConnLifetimeMax: 300 * time.Second,
		MaxStreamsPerConn:  500,
		MinDialInterval:    450 * time.Millisecond,
	}
}

// Normalize fills in missing or invalid parameters with safe defaults.
func (c *FlowShapingConfig) Normalize() {
	if c.MaxConnections <= 0 {
		c.MaxConnections = 3
	}
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 8
	}
	if c.MaxBytesMin <= 0 {
		c.MaxBytesMin = 15 * 1024 * 1024
	}
	if c.MaxBytesMax <= 0 {
		c.MaxBytesMax = 45 * 1024 * 1024
	}
	if c.MaxBytesMax < c.MaxBytesMin {
		c.MaxBytesMax = c.MaxBytesMin
	}
	if c.MaxConnLifetimeMin <= 0 {
		c.MaxConnLifetimeMin = 120 * time.Second
	}
	if c.MaxConnLifetimeMax <= 0 {
		c.MaxConnLifetimeMax = 300 * time.Second
	}
	if c.MaxConnLifetimeMax < c.MaxConnLifetimeMin {
		c.MaxConnLifetimeMax = c.MaxConnLifetimeMin
	}
	if c.MaxStreamsPerConn <= 0 {
		c.MaxStreamsPerConn = 500
	}
	if c.MinDialInterval <= 0 {
		c.MinDialInterval = 450 * time.Millisecond
	}
}

// PaymentsConfig covers the wallet-related options of the interface.
type PaymentsConfig struct {
	// SupportAddress is the project's TON address that voluntary support goes to. Empty turns the
	// feature off completely: nothing is shown and nothing is ever set aside.
	SupportAddress string `json:"support_address"`
	// OnRampURL is the card top-up page of the fiat partner, with {address} standing for the
	// user's wallet address. Empty hides the "buy with card" button.
	OnRampURL string `json:"onramp_url"`
}

// RelayServerConfig: what this node offers when it relays circuits for other peers.
type RelayServerConfig struct {
	Enabled     bool  `json:"enabled"`
	PricePerMB  int64 `json:"price_per_mb"` // 0 = free; advertised to clients
	MaxCircuits int   `json:"max_circuits"`
	// SignalingOnly: pass on only the small messages that start direct connections between
	// peers behind NATs (hole punching); no calls, files, messages or tunnels are relayed.
	SignalingOnly bool `json:"signaling_only"`
	// Private: a personal relay that serves only the peers in AllowedPeers. It does not announce
	// itself as a relay to the open network, so strangers do not pick it; the people you allow
	// reach it by its address (see RelayClientConfig.Relays). A public relay (the default) serves
	// everyone.
	Private bool `json:"private"`
	// AllowedPeers lists the peer IDs (64-hex) that may use this relay when it is private.
	AllowedPeers []string `json:"allowed_peers"`

	// Limits on what peers can take from this relay. For each of them 0 means the built-in
	// default (shown in brackets) and a negative value means no limit.

	// MaxCircuitsPerPeer: circuits one peer may hold open at once [16].
	MaxCircuitsPerPeer int `json:"max_circuits_per_peer,omitempty"`
	// MaxCircuitsPerIP: circuits the peers of one IP address (an IPv6 /64) may hold at once [32];
	// one IPv4 /24 or IPv6 /48 may hold twice that. Loopback and LAN addresses are not limited.
	MaxCircuitsPerIP int `json:"max_circuits_per_ip,omitempty"`
	// ReservedCircuitsPct: share of MaxCircuits kept for peers connected for 10 minutes or more,
	// so a burst of fresh identities cannot take every slot [25].
	ReservedCircuitsPct int `json:"reserved_circuits_pct,omitempty"`
	// CircuitIdleSeconds and CircuitIdleKB: a circuit that moved less than CircuitIdleKB in the
	// last CircuitIdleSeconds is idle and closed [300 s, 32 KB].
	CircuitIdleSeconds int `json:"circuit_idle_seconds,omitempty"`
	CircuitIdleKB      int `json:"circuit_idle_kb,omitempty"`
	// CircuitMaxMB: most one circuit may carry before it is closed [4096].
	CircuitMaxMB int64 `json:"circuit_max_mb,omitempty"`
	// PeerMaxMBPerDay: most the circuits one peer opened may carry in a day (UTC) [16384].
	PeerMaxMBPerDay int64 `json:"peer_max_mb_per_day,omitempty"`
	// PeerMaxMbps: speed of all the circuits one peer opened, in Mbit/s [50].
	PeerMaxMbps int `json:"peer_max_mbps,omitempty"`
	// MaxMbps: speed of everything this node relays through circuits, in Mbit/s [no limit].
	MaxMbps int `json:"max_mbps,omitempty"`
}

// RelayClientConfig: how this node picks relays.
type RelayClientConfig struct {
	Enabled                 bool  `json:"enabled"`
	MaxAcceptablePricePerMB int64 `json:"max_acceptable_price_per_mb"` // <= 0: no limit
	MaxAcceptablePingMs     int   `json:"max_acceptable_ping_ms"`      // <= 0: no limit
	DailyBudgetLimit        int64 `json:"daily_budget_limit"`
	MarketAwareSelection    bool  `json:"market_aware_selection"`
	// Relays are specific relays this node connects to on start-up and prefers over the ones it
	// discovers, as "host:port/<peer id>" (or a ws:// URL). Use it to reach a private relay you
	// were given access to; such a relay is used even though it does not advertise itself.
	Relays []string `json:"relays"`
}

// DefaultRelayMaxCircuits is how many circuits a relay carries at once unless told otherwise.
const DefaultRelayMaxCircuits = 200

type RelayConfig struct {
	Server RelayServerConfig `json:"server"`
	Client RelayClientConfig `json:"client"`
}

// ExitConfig limits what peers can take from this node's exit when it shares one. For each field
// 0 means the built-in default (shown in brackets) and a negative value means no limit.
type ExitConfig struct {
	// MaxStreams: TCP tunnels and UDP sessions the exit serves at once, all peers together [4096].
	MaxStreams int `json:"max_streams,omitempty"`
	// MaxStreamsPerPeer and MaxStreamsPerIP: the same for one peer, and for the peers of one IP
	// address (an IPv6 /64) [256, 512]. Loopback and LAN addresses are not limited per IP.
	MaxStreamsPerPeer int `json:"max_streams_per_peer,omitempty"`
	MaxStreamsPerIP   int `json:"max_streams_per_ip,omitempty"`
	// MaxUDPPerPeer: UDP sessions (each one an exit socket) one peer may hold [32].
	MaxUDPPerPeer int `json:"max_udp_per_peer,omitempty"`
	// IdleSeconds: a TCP tunnel that moved nothing either way for this long is closed [300].
	IdleSeconds int `json:"idle_seconds,omitempty"`
	// MaxLifetimeSeconds: a TCP tunnel is closed after this long, busy or not [no limit].
	MaxLifetimeSeconds int `json:"max_lifetime_seconds,omitempty"`
	// FreePeerMbps and FreeIPMbps: on a free exit, the speed one peer, and all the peers of one
	// IP address together, may use, so nobody takes the whole uplink [50, 100]. Peers on the free
	// list (the operator's own devices) and paying peers are not slowed down.
	FreePeerMbps int `json:"free_peer_mbps,omitempty"`
	FreeIPMbps   int `json:"free_ip_mbps,omitempty"`
	// FreePeerMBPerDay and FreeIPMBPerDay: on a free exit, the traffic one peer, and all the peers
	// of one IP address together, may take in a day (UTC), in MB; FreeGBPerMonth: the traffic the
	// whole exit gives away in a calendar month (UTC), in GB, after which it refuses new streams
	// until the month ends. For an operator whose own traffic is paid for by the GB. Unlike the
	// fields above, 0 (and a negative value) means no cap, which is the default. Peers on the free
	// list are never counted.
	FreePeerMBPerDay int64 `json:"free_peer_mb_per_day,omitempty"`
	FreeIPMBPerDay   int64 `json:"free_ip_mb_per_day,omitempty"`
	FreeGBPerMonth   int64 `json:"free_gb_per_month,omitempty"`
}

type StorageConfig struct {
	DatabasePath string `json:"database_path"`
	// DownloadDir is where received files go; empty: the user's Downloads folder, in "EchoFog".
	DownloadDir string `json:"download_dir,omitempty"`
}

// TonConfig selects the TON network of the built-in wallet ("mainnet" | "testnet").
type TonConfig struct {
	Network string `json:"network"`
}

// AppConfig is the complete configuration of an EchoFog node.
type AppConfig struct {
	Network  NetworkConfig  `json:"network"`
	AntiDPI  AntiDPIConfig  `json:"anti_dpi"`
	Payments PaymentsConfig `json:"payments"`
	Relay    RelayConfig    `json:"relay"`
	Exit     ExitConfig     `json:"exit"`
	Storage  StorageConfig  `json:"storage"`
	Ton      TonConfig      `json:"ton"`
	Access   AccessConfig   `json:"access"`
	// Role is RoleClient (default, also when empty) or RoleServer.
	Role string `json:"role,omitempty"`
}

// IsServer reports whether the node runs as a server: network services only, no chat or calls.
func (c *AppConfig) IsServer() bool { return c != nil && c.Role == RoleServer }

// DefaultTonNetwork is the TON network of the built-in wallet: "testnet" while the vault contract
// has not had an independent security audit, so testers never pay real money. A release build
// switches it without a code change:
//
//	go build -ldflags "-X github.com/echofog/echofog-core/internal/config.DefaultTonNetwork=mainnet"
var DefaultTonNetwork = "testnet"

// DefaultConfig returns the defaults for a profile. The database lives under the user's home
// directory, in ~/.echofog/profiles/<profile>.
func DefaultConfig(profileName string) *AppConfig {
	if profileName == "" {
		profileName = "default"
	}
	cfg := &AppConfig{}

	cfg.Ton.Network = DefaultTonNetwork
	cfg.Access.Mode = AccessPublic
	cfg.Role = RoleClient

	cfg.Network.ListenAddr = ":0"
	cfg.Network.MaxConnections = 100
	cfg.Network.ProtocolVersion = CurrentProtocolVersion
	cfg.Network.DirectSocket = false
	cfg.Network.SocketBufferMB = 32
	cfg.Network.BootstrapNodes = defaultBootstrap()
	cfg.Network.PortMapping = true
	cfg.Network.HolePunch = true

	cfg.AntiDPI.Obfs = ObfsNode
	cfg.AntiDPI.FlowShaping = DefaultFlowShapingConfig()
	cfg.AntiDPI.TunnelPadding = true
	cfg.AntiDPI.RegionAwareObfs = false // opt-in: safe only where domestic traffic is not censored
	cfg.AntiDPI.MicroChunkTCP = false   // opt-in: only helps where the TSPU ~16 KB TCP freeze runs
	// No default SNI is baked in: a shared network-wide default (e.g. www.microsoft.com) on a
	// residential or VPS IP is itself an SNI/IP-mismatch fingerprint. A node that exposes a public
	// TCP/WS listener must set anti_dpi.tls_server_name and anti_dpi.camouflage_addr to a real site
	// in its own datacenter (see docs/anti-dpi.md); a pure client raises no public listener and does
	// not need them.

	cfg.Relay.Server.Enabled = true
	cfg.Relay.Server.MaxCircuits = DefaultRelayMaxCircuits

	cfg.Relay.Client.Enabled = true
	cfg.Relay.Client.MaxAcceptablePingMs = 250
	cfg.Relay.Client.DailyBudgetLimit = 5_000_000
	cfg.Relay.Client.MarketAwareSelection = true

	home, _ := os.UserHomeDir()
	cfg.Storage.DatabasePath = filepath.Join(home, ".echofog", "profiles", profileName, "db")

	return cfg
}

// GetAvailableProfiles lists the profiles found under ~/.echofog/profiles.
func GetAvailableProfiles() ([]string, error) {
	home, _ := os.UserHomeDir()
	profilesDir := filepath.Join(home, ".echofog", "profiles")

	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	var profiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			profiles = append(profiles, entry.Name())
		}
	}
	return profiles, nil
}
