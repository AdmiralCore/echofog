package node

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/pkg/logging"
)

// A connection report: what a tester sends when EchoFog does not connect (or does), with nothing
// that identifies them. It names the country and the provider, the NAT, which transports got
// through, how the node joins the network, the VPN state and the latest problems from the log —
// and no IP address, no peer ID, no link, no contact, no site visited. Nothing is sent anywhere:
// the user reads it, then copies it or saves it, and posts it themselves.

// ConnectionReport is the report as text, and the fields an issue form takes on its own.
type ConnectionReport struct {
	Text     string `json:"text"`
	Country  string `json:"country"`  // ISO code, "" not known
	Provider string `json:"provider"` // the ISP, "" not known
	Version  string `json:"version"`
}

// reportProblems is how many of the latest warnings and errors the report carries.
const reportProblems = 15

// ConnectionReport puts the report together; appVersion is the app's own version.
func (n *NodeApi) ConnectionReport(appVersion string) (ConnectionReport, error) {
	if n.node == nil || n.node.Cfg == nil {
		return ConnectionReport{}, fmt.Errorf("node is not running")
	}
	cfg := n.node.Cfg
	r := ConnectionReport{Version: appVersion}
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("EchoFog connection report — no addresses, IDs, links or contacts")
	line("app: %s  protocol: %d  os: %s/%s", orUnknown(appVersion), config.CurrentProtocolVersion, runtime.GOOS, runtime.GOARCH)

	r.Country, r.Provider = n.ownCountryAndProvider()
	line("country: %s  provider: %s", orUnknown(r.Country), orUnknown(r.Provider))

	if n.node.Swarm != nil {
		reach := n.node.RelayReachability()
		reachable := "yes"
		if reach.Addr == "" {
			reachable = "no"
			if reach.Code != "" {
				reachable += " (" + reach.Code + ")"
			}
		}
		line("nat: %s  port mapped: %s  reachable from outside: %s", orUnknown(reach.NAT), yesNo(reach.Mapped), reachable)

		udp := "yes"
		if n.node.Swarm.PrefersTCPHere() {
			udp = "no, TCP goes first on this network"
		}
		line("transport: %s  QUIC (UDP) gets through here: %s", orDefault(cfg.Network.Transport, network.ModeAuto), udp)

		kinds := n.node.Swarm.LinkKinds()
		total := 0
		var parts []string
		for _, k := range []string{"quic-reality", "quic", "tcp", "ws"} {
			if kinds[k] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", k, kinds[k]))
				total += kinds[k]
			}
		}
		line("links: %d (%s)", total, orDefault(strings.Join(parts, ", "), "none"))
	}

	if g, err := n.GetGuardStatus(); err == nil {
		gs := g.Mode
		if g.Active {
			gs += fmt.Sprintf(", active, %d of %d guards up", g.Connected, g.Wanted)
		} else {
			gs += ", not active"
		}
		if g.StoodDown {
			gs += ", stood down (the country does not censor)"
		}
		if len(g.Pinned) > 0 {
			gs += fmt.Sprintf(", %d pinned relay(s)", len(g.Pinned))
		}
		line("guard mode: %s", gs)
	}
	proxy := "none"
	if cfg.Network.Proxy != "" {
		proxy = "SOCKS5 (Tor or other)"
	}
	line("proxy: %s  calls: %s  access: %s  region-aware obfs: %s", proxy, n.node.CallRoute(), n.node.AccessMode(), yesNo(cfg.AntiDPI.RegionAwareObfs))

	vpn := n.GetVPNStatus()
	if vpn.Running || vpn.WholeComputer {
		v := "connected"
		if !vpn.Running {
			v = "not connected"
		}
		v += ", exit " + orUnknown(vpn.ExitState)
		if vpn.TunnelReady {
			v += ", tunnel ready"
		}
		line("vpn: %s  whole computer: %s  kill switch: %s  blocked: %s", v, yesNo(vpn.WholeComputer), yesNo(vpn.KillSwitch), yesNo(vpn.Blocked))
		if vpn.Switches > 0 {
			line("vpn switches: %d (last: %s)", vpn.Switches, orUnknown(vpn.LastSwitch.Reason))
		}
		if vpn.Refused > 0 {
			line("streams the exit refused: %d (last: %s)", vpn.Refused, orUnknown(vpn.LastRefusalReason))
		}
		if vpn.WholeComputerError != "" {
			line("whole computer: %s", cleanReportText(vpn.WholeComputerError))
		}
	} else {
		line("vpn: off")
	}

	problems := recentProblems(reportProblems)
	if len(problems) == 0 {
		line("recent problems: none")
	} else {
		line("recent problems (the last %d warnings and errors, cleaned):", len(problems))
		for _, p := range problems {
			line("  %s", p)
		}
	}
	r.Text = b.String()
	return r, nil
}

// ownCountryAndProvider looks up the node's own public address in the GeoIP cache, asking a
// service once (a few seconds at most) when the provider is not known yet. Nothing when the node
// is behind a proxy: it does not learn its own address then.
func (n *NodeApi) ownCountryAndProvider() (country, provider string) {
	if n.node.NAT == nil {
		return "", ""
	}
	ip := geoIP(n.node.NAT.GetPublicAddr())
	if ip == "" {
		return n.homeCountry(), ""
	}
	st := n.historyStorage()
	m := loadGeoCache(st)
	if e, _ := cachedGeo(m, ip, time.Now()); e.CountryCode != "" && e.Org != "" {
		return e.CountryCode, e.Org
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := n.geo().lookup(ctx, ip)
	if err != nil {
		return n.homeCountry(), ""
	}
	e.At = time.Now().Unix()
	m[ip] = e
	saveGeoCache(st, m)
	return e.CountryCode, e.Org
}

// reportDropAttrs are log attributes that point at somebody or somewhere; the report leaves them out.
var reportDropAttrs = map[string]bool{
	"addr": true, "remote_addr": true, "local_addr": true, "host": true, "ip": true, "url": true,
	"link": true, "peer": true, "peer_id": true, "peerid": true, "from": true, "to": true, "exit": true,
	"relay": true, "guard": true, "target": true, "id": true, "contact": true, "alias": true,
	"profile": true, "path": true, "file": true, "dir": true, "sni": true, "domain": true, "listen_addr": true,
}

var (
	reportHexID = regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`)
	reportLink  = regexp.MustCompile(`(?i)\b(?:echofog|https?|wss?)://\S+`)
	// host:port and bare host names with a dot (a site, a relay's name); "1.5s" and versions are kept
	reportHost = regexp.MustCompile(`\b(?:[a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}(?::\d+)?\b`)
)

// cleanReportText hides what could point at a person or a place in a free-text message.
func cleanReportText(s string) string {
	s = reportLink.ReplaceAllString(s, "[link]")
	s = reportHexID.ReplaceAllString(s, "[id]")
	s = reportHost.ReplaceAllStringFunc(s, func(h string) string {
		if strings.HasSuffix(strings.ToLower(strings.SplitN(h, ":", 2)[0]), ".go") {
			return h // a source file in a stack trace
		}
		return "[host]"
	})
	return logging.MaskIPs(s)
}

// recentProblems is the latest warnings and errors from the log, oldest first, cleaned for a
// report.
func recentProblems(limit int) []string {
	var out []string
	entries := logging.Recent(0)
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		if e.Level != "warn" && e.Level != "error" {
			continue
		}
		keys := make([]string, 0, len(e.Attrs))
		for k := range e.Attrs {
			if !reportDropAttrs[strings.ToLower(k)] && k != "name" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(e.Time.Format("15:04:05"))
		b.WriteString(" " + strings.ToUpper(e.Level) + " " + cleanReportText(e.Message))
		for _, k := range keys {
			b.WriteString(" " + k + "=" + cleanReportText(e.Attrs[k]))
		}
		out = append(out, b.String())
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func orUnknown(s string) string { return orDefault(s, "unknown") }

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
