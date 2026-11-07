package probe

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

// Scenarios the launchpad probes (T15.0b item 2): an ordinary node with a cover behind it, a node
// whose cover is its own website, a hidden node (which must not answer at all) and a server with
// several addresses (one entry each).
const (
	ScenarioOrdinary = "ordinary"
	ScenarioCover    = "cover"
	ScenarioHidden   = "hidden"
	ScenarioServer   = "server"
)

// Case is one target with the inputs its probes need.
type Case struct {
	Target Target
	Inputs Inputs
}

type specEntry struct {
	Scenario      string `json:"scenario"`
	Network       string `json:"network"`
	Addr          string `json:"addr"`
	Cover         string `json:"cover"`
	SNI           string `json:"sni"`
	FirstFlight   string `json:"first_flight"`
	Valid         string `json:"valid"`
	Foreign       string `json:"foreign"`
	WindowSeconds int    `json:"window_seconds"`
	ListenMillis  int    `json:"listen_ms"`
}

// LoadSpec reads a JSON list of probe entries: scenario, network, addr, cover, sni, the captured
// and fresh flights as hex, the replay window in seconds and the listen time in milliseconds. It
// refuses an entry that cannot run, so a typo costs a message, not a run.
func LoadSpec(r io.Reader) ([]Case, error) { return LoadSpecWith(r, SpecOptions{}) }

// SpecOptions changes what a spec may ask.
type SpecOptions struct {
	// AllowPublic lets the spec name public addresses and host names. Without it the prober asks
	// loopback, private, link-local, CGNAT and documentation addresses only (the lab).
	AllowPublic bool
}

// LoadSpecWith is LoadSpec with options.
func LoadSpecWith(r io.Reader, o SpecOptions) ([]Case, error) {
	var entries []specEntry
	if err := json.NewDecoder(r).Decode(&entries); err != nil {
		return nil, fmt.Errorf("probe spec: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("probe spec: no entries")
	}
	var out []Case
	for i, e := range entries {
		switch e.Scenario {
		case ScenarioOrdinary, ScenarioCover, ScenarioServer:
			if e.Cover == "" {
				return nil, fmt.Errorf("probe spec: entry %d (%s) needs a cover to compare with", i, e.Scenario)
			}
		case ScenarioHidden:
			if e.Cover != "" {
				return nil, fmt.Errorf("probe spec: entry %d: a hidden node has no cover; its reference is silence", i)
			}
		default:
			return nil, fmt.Errorf("probe spec: entry %d: unknown scenario %q", i, e.Scenario)
		}
		if e.Addr == "" {
			return nil, fmt.Errorf("probe spec: entry %d has no address", i)
		}
		for _, a := range []string{e.Addr, e.Cover} {
			if err := checkTarget(a, o.AllowPublic); err != nil {
				return nil, fmt.Errorf("probe spec: entry %d: %w", i, err)
			}
		}
		network := e.Network
		if network == "" {
			network = "tcp"
		}
		if network != "tcp" && network != "udp" {
			return nil, fmt.Errorf("probe spec: entry %d: network %q is not tcp or udp", i, e.Network)
		}
		var in Inputs
		for _, f := range []struct {
			name string
			src  string
			dst  *[]byte
		}{{"first_flight", e.FirstFlight, &in.FirstFlight}, {"valid", e.Valid, &in.Valid}, {"foreign", e.Foreign, &in.Foreign}} {
			b, err := hex.DecodeString(f.src)
			if err != nil {
				return nil, fmt.Errorf("probe spec: entry %d: %s is not hex: %w", i, f.name, err)
			}
			*f.dst = b
		}
		in.Window = time.Duration(e.WindowSeconds) * time.Second
		in.Listen = time.Duration(e.ListenMillis) * time.Millisecond
		out = append(out, Case{
			Target: Target{Name: e.Scenario, Network: network, Addr: e.Addr, Cover: e.Cover, SNI: e.SNI},
			Inputs: in,
		})
	}
	return out, nil
}

// RunSpec reads a spec, runs every case from this machine (the border), writes the reports as JSON
// to w and returns how many answers of a node's own were not expected.
func RunSpec(ctx context.Context, r io.Reader, w io.Writer) (own int, err error) {
	return RunSpecWith(ctx, r, w, SpecOptions{})
}

// RunSpecWith is RunSpec with options.
func RunSpecWith(ctx context.Context, r io.Reader, w io.Writer, o SpecOptions) (own int, err error) {
	cases, err := LoadSpecWith(r, o)
	if err != nil {
		return 0, err
	}
	reps := make([]Report, 0, len(cases))
	for _, c := range cases {
		rep := Run(ctx, c.Target, c.Inputs)
		own += rep.OwnAnswers()
		reps = append(reps, rep)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return own, enc.Encode(reps)
}

// labNets are the addresses a lab prober may ask without being told otherwise: the documentation
// ranges the stand uses, CGNAT, and everything netip calls private, loopback or link-local.
var labNets = []netip.Prefix{
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("100.64.0.0/10"),
}

// checkTarget refuses an address that is not host:port with an IP literal inside the lab ranges,
// unless public addresses are allowed (then any host:port will do). An empty address is the cover
// of a hidden node and is not checked.
func checkTarget(addr string, allowPublic bool) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if allowPublic {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%q: a host name is not accepted without -allow-public (the prober asks IP literals of the lab)", addr)
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	for _, p := range labNets {
		if p.Contains(ip) {
			return nil
		}
	}
	return fmt.Errorf("%q is a public address: the prober asks the lab only unless -allow-public says that a public node is meant", addr)
}
