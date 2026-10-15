package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network/mimicry"
)

// isPublicNode reports whether this node exposes an explicit inbound TCP/WS listener on a
// non-loopback address (and so is visible to active probes). A pure client sets neither address
// and only dials out. A listener bound to loopback is not public: that is the CDN / reverse-proxy
// setup (for example --ws-listen 127.0.0.1:8080 behind Cloudflare), where the outside world sees
// the CDN's IP and the operator's own domain, so the SNI/IP-mismatch concern does not apply.
func isPublicNode(cfg *config.AppConfig) bool {
	tcp := cfg.Network.TCPListenAddr
	publicTCP := tcp != "" && tcp != "off" && !loopbackListen(tcp)
	publicWS := cfg.Network.WSListenAddr != "" && !loopbackListen(cfg.Network.WSListenAddr)
	return publicTCP || publicWS
}

// loopbackListen reports whether a listen address ("host:port") binds only to loopback.
// An empty host (":443") listens on every interface, so it is public.
func loopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// publicListenerConfigError enforces the anti-DPI role split. A pure client raises no inbound
// TCP/WS listener (empty network.tcp_listen_addr and network.ws_listen_addr); it only dials out, so
// it never presents a mimicked SNI or a server certificate to a prober and needs no mimicry target.
//
// A node that exposes an explicit public listener (a fixed tcp_listen_addr such as ":443", or any
// ws_listen_addr) is visible to active probes. It must mimic a real site hosted in its own
// datacenter — anti_dpi.tls_server_name and anti_dpi.camouflage_addr both set — so the SNI it
// presents is consistent with its IP's subnet/ASN. A shared or empty default (e.g. www.microsoft.com
// on a residential or VPS IP) is itself an SNI/IP-mismatch fingerprint, so the node refuses to start
// such a listener until both are set. ensureMimicryTarget fills them automatically from a neighbour
// when it can; this is the backstop when it cannot.
func publicListenerConfigError(cfg *config.AppConfig) error {
	if !isPublicNode(cfg) {
		return nil
	}
	if cfg.AntiDPI.TLSServerName != "" && cfg.AntiDPI.CamouflageAddr != "" {
		return nil
	}
	kind := "TCP"
	publicTCP := cfg.Network.TCPListenAddr != "" && cfg.Network.TCPListenAddr != "off"
	switch {
	case publicTCP && cfg.Network.WSListenAddr != "":
		kind = "TCP and WebSocket"
	case cfg.Network.WSListenAddr != "":
		kind = "WebSocket"
	}
	return fmt.Errorf("refusing to start a public %s listener: could not find a camouflage neighbour automatically, so set anti_dpi.tls_server_name and anti_dpi.camouflage_addr to a real site hosted in this node's own datacenter (same subnet/ASN), so the mimicked SNI matches the IP; without them the node serves a mismatched SNI that DPI flags. A pure client (no tcp_listen_addr/ws_listen_addr) needs neither. See docs/anti-dpi.md", kind)
}

// neighborCacheName is the file (in the profile directory) where an adopted camouflage neighbour is
// remembered, so later restarts do not scan again while the neighbour still holds.
const neighborCacheName = "anti_dpi_neighbour.json"

// ensureMimicryTarget fills anti_dpi.tls_server_name and camouflage_addr for a public node that has
// not set them: first from the on-disk cache (if the remembered neighbour is still alive and still
// answers like us), otherwise from a gentle scan of nearby IPs. If nothing is found the fields stay
// empty and publicListenerConfigError then refuses the listener with guidance.
func (n *Node) ensureMimicryTarget() {
	if !isPublicNode(n.Cfg) {
		return
	}
	if n.Cfg.AntiDPI.TLSServerName != "" && n.Cfg.AntiDPI.CamouflageAddr != "" {
		return // the operator configured it explicitly; never override
	}

	ourProfile, err := ownServerProfile()
	if err != nil {
		n.Logger.Warn("anti-DPI: could not measure our own ServerHello; skipping neighbour discovery", "err", err)
		return
	}

	cachePath := filepath.Join(filepath.Dir(n.Cfg.Storage.DatabasePath), neighborCacheName)
	if nb, ok := loadNeighborCache(cachePath); ok {
		ctx, cancel := context.WithTimeout(n.Ctx, 5*time.Second)
		alive := mimicry.VerifyNeighbor(ctx, nb, ourProfile)
		cancel()
		if alive {
			n.applyNeighbor(nb)
			n.Logger.Info("anti-DPI: reusing the cached camouflage neighbour", "server_name", nb.ServerName, "forward_to", nb.Addr)
			return
		}
		n.Logger.Info("anti-DPI: the cached camouflage neighbour is gone; scanning again", "was", nb.ServerName)
	}

	ownIP, ok := egressIPv4()
	if !ok {
		n.Logger.Warn("anti-DPI: could not determine a public IPv4 address for neighbour discovery; set anti_dpi.tls_server_name and camouflage_addr manually")
		return
	}
	ctx, cancel := context.WithTimeout(n.Ctx, 20*time.Second)
	defer cancel()
	nb, ok := mimicry.ScanForNeighbor(ctx, ownIP, ourProfile, n.Logger)
	if !ok {
		return
	}
	n.applyNeighbor(nb)
	if err := saveNeighborCache(cachePath, nb); err != nil {
		n.Logger.Warn("anti-DPI: could not cache the camouflage neighbour", "err", err)
	}
}

func (n *Node) applyNeighbor(nb mimicry.Neighbor) {
	if n.Cfg.AntiDPI.TLSServerName == "" {
		n.Cfg.AntiDPI.TLSServerName = nb.ServerName
	}
	if n.Cfg.AntiDPI.CamouflageAddr == "" {
		n.Cfg.AntiDPI.CamouflageAddr = nb.Addr
	}
}

// ownServerProfile measures the ServerHello our fallback server would send (TLS version, cipher,
// key-share group), using a throwaway camouflage certificate — those fields do not depend on the
// certificate's name, only on our cipher and curve preferences.
func ownServerProfile() (mimicry.ServerProfile, error) {
	cert, err := mimicry.GenerateCamouflageCert("")
	if err != nil {
		return mimicry.ServerProfile{}, err
	}
	return mimicry.OwnServerProfile(mimicry.BuildServerTLSConfig(cert))
}

// egressIPv4 returns the local IPv4 address the kernel would use to reach the internet. On a VPS or
// relay that is the public IP; no packet is sent (UDP "connect" only selects a route).
func egressIPv4() (netip.Addr, bool) {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return netip.Addr{}, false
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	return a, a.Is4()
}

func loadNeighborCache(path string) (mimicry.Neighbor, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return mimicry.Neighbor{}, false
	}
	var nb mimicry.Neighbor
	if err := json.Unmarshal(data, &nb); err != nil || nb.ServerName == "" || nb.Addr == "" {
		return mimicry.Neighbor{}, false
	}
	return nb, true
}

func saveNeighborCache(path string, nb mimicry.Neighbor) error {
	data, err := json.MarshalIndent(nb, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
