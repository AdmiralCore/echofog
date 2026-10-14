// Package config holds the settings of an EchoFog node and their defaults.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// CurrentProtocolVersion is the peer protocol version this build speaks.
// 101: mutual peer authentication bound to the transport session.
const CurrentProtocolVersion uint32 = 101

// Values of AntiDPIConfig.Obfs.
const (
	ObfsNode = "node"
	ObfsOff  = "off"
)

// DefaultBootstrapNodes are the public nodes a fresh install joins the network through ("host:port").
// It is empty until the project's starter nodes are published; operators set their own with the
// bootstrap option of the node.
var DefaultBootstrapNodes = []string{}

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

	// Transport: "auto" (QUIC, TCP/TLS when UDP does not get through), "quic" or "tcp"
	Transport string `json:"transport"`
	// TCPListenAddr for the TCP/TLS fallback; empty means the QUIC port number, "off" disables it.
	// Set ":443" to blend in with normal HTTPS (on Linux binding 443 needs CAP_NET_BIND_SERVICE
	// or a redirect).
	TCPListenAddr string `json:"tcp_listen_addr"`
	// TCPRemotePort is the port peers are dialled on for the TCP fallback when the network runs
	// it on a fixed port (e.g. 443) separate from QUIC. 0 means use the same port as the address.
	TCPRemotePort int `json:"tcp_remote_port"`

	// AdvertisePort is the public port peers should use to reach this node, when it differs from
	// the listen port because of a port map (e.g. Docker publishes host 443 to the container's
	// 4242). It sets the port in the printed join address and the one announced for the TCP
	// fallback. 0 = use the listen port.
	AdvertisePort int `json:"advertise_port"`

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

type RelayConfig struct {
	Server RelayServerConfig `json:"server"`
	Client RelayClientConfig `json:"client"`
}

type StorageConfig struct {
	DatabasePath string `json:"database_path"`
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
	Storage  StorageConfig  `json:"storage"`
	Ton      TonConfig      `json:"ton"`
}

// DefaultConfig returns the defaults for a profile. The database lives under the user's home
// directory, in ~/.echofog/profiles/<profile>.
func DefaultConfig(profileName string) *AppConfig {
	if profileName == "" {
		profileName = "default"
	}
	cfg := &AppConfig{}

	cfg.Ton.Network = "mainnet"

	cfg.Network.ListenAddr = ":0"
	cfg.Network.MaxConnections = 100
	cfg.Network.ProtocolVersion = CurrentProtocolVersion
	cfg.Network.DirectSocket = false
	cfg.Network.SocketBufferMB = 32
	cfg.Network.BootstrapNodes = defaultBootstrap()
	cfg.Network.PortMapping = true
	cfg.Network.HolePunch = true

	cfg.AntiDPI.Obfs = ObfsNode
	cfg.AntiDPI.TLSServerName = "www.microsoft.com"

	cfg.Relay.Server.Enabled = true
	cfg.Relay.Server.MaxCircuits = 50

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
