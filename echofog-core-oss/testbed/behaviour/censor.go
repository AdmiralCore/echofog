package behaviour

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// The rules of a censor, as the launchpad design lists them (D16–D25). Each is a published
// behaviour, and the model reproduces it; it does not guess what a particular operator runs. The
// clock is injected: the same input gives the same verdicts.

// VerdictKind is what the censor does to a flow.
type VerdictKind int

const (
	Pass VerdictKind = iota
	Drop
	Reset
	Freeze
	Throttle
)

func (k VerdictKind) String() string {
	switch k {
	case Pass:
		return "pass"
	case Drop:
		return "drop"
	case Reset:
		return "reset"
	case Freeze:
		return "freeze"
	case Throttle:
		return "throttle"
	}
	return "?"
}

// Verdict is the censor's decision about a flow. Until is set when the decision lasts.
type Verdict struct {
	Kind    VerdictKind
	RateBps int
	Until   time.Time
}

// Attempt is one flow as the censor sees it. Flow numbers it; ServerBytes is what the server has
// sent on it so far.
type Attempt struct {
	Flow        int
	Proto       string // tcp or udp
	Dst         netip.AddrPort
	SNI         string
	JA4         string
	ASN         string
	ServerBytes int
}

// AndRule is the behavioural rule of 2026 (D21): when the destination's ASN is in the list AND the
// client's ClientHello fingerprint is in the profile AND more than Parallel TLS flows are open to
// the address, everything to the address is dropped for Penalty.
type AndRule struct {
	ASNs     map[string]bool
	JA4s     map[string]bool
	Parallel int
	Penalty  time.Duration
}

// CensorConfig switches the rules on. A zero field is a rule that is off.
type CensorConfig struct {
	FreezeBytes  int             // D16: a TCP flow stops once the server has sent this much
	UDPDrop      bool            // D17: all UDP is dropped
	AllowSNI     map[string]bool // D18, D23: only these names pass
	AllowCIDR    []netip.Prefix  // D18, D23: only these ranges pass
	Only443      bool            // D18: only port 443 passes
	ThrottleBps  int             // D19: every flow gets this rate
	ShutdownFrom time.Time       // D20: nothing passes in [From, To)
	ShutdownTo   time.Time
	And          *AndRule // D21
	Port443Down  bool     // D25: 443 is reset
}

// Censor applies a CensorConfig to flows.
type Censor struct {
	cfg CensorConfig
	clk carrier.Clock

	mu       sync.Mutex
	andOpen  map[netip.AddrPort]map[int]bool
	andUntil map[netip.AddrPort]time.Time
}

// NewCensor returns a censor on the given clock.
func NewCensor(cfg CensorConfig, clock carrier.Clock) *Censor {
	if clock == nil {
		clock = NewVClock()
	}
	return &Censor{cfg: cfg, clk: clock, andOpen: map[netip.AddrPort]map[int]bool{}, andUntil: map[netip.AddrPort]time.Time{}}
}

// Begin judges a new flow.
func (c *Censor) Begin(a Attempt) Verdict {
	now := c.clk.Now()
	cfg := c.cfg
	if !cfg.ShutdownFrom.IsZero() && !now.Before(cfg.ShutdownFrom) && now.Before(cfg.ShutdownTo) {
		return Verdict{Kind: Drop, Until: cfg.ShutdownTo}
	}
	if cfg.Port443Down && a.Dst.Port() == 443 {
		if a.Proto == "udp" {
			return Verdict{Kind: Drop}
		}
		return Verdict{Kind: Reset}
	}
	if cfg.Only443 && a.Dst.Port() != 443 {
		return Verdict{Kind: Drop}
	}
	if len(cfg.AllowSNI) > 0 && !cfg.AllowSNI[a.SNI] {
		return Verdict{Kind: Drop}
	}
	if len(cfg.AllowCIDR) > 0 && !c.inAllowedRange(a.Dst.Addr()) {
		return Verdict{Kind: Drop}
	}
	if cfg.UDPDrop && a.Proto == "udp" {
		return Verdict{Kind: Drop}
	}
	if v, hit := c.andRule(a, now); hit {
		return v
	}
	if cfg.ThrottleBps > 0 {
		return Verdict{Kind: Throttle, RateBps: cfg.ThrottleBps}
	}
	return Verdict{Kind: Pass}
}

func (c *Censor) inAllowedRange(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range c.cfg.AllowCIDR {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *Censor) andRule(a Attempt, now time.Time) (Verdict, bool) {
	r := c.cfg.And
	if r == nil {
		return Verdict{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if until, ok := c.andUntil[a.Dst]; ok && now.Before(until) {
		return Verdict{Kind: Drop, Until: until}, true
	}
	if a.Proto != "tcp" || !r.ASNs[a.ASN] || !r.JA4s[a.JA4] {
		return Verdict{}, false
	}
	open := c.andOpen[a.Dst]
	if open == nil {
		open = map[int]bool{}
		c.andOpen[a.Dst] = open
	}
	open[a.Flow] = true
	if len(open) > r.Parallel {
		until := now.Add(r.Penalty)
		c.andUntil[a.Dst] = until
		delete(open, a.Flow)
		return Verdict{Kind: Drop, Until: until}, true
	}
	return Verdict{}, false
}

// Judge is asked again about an open flow as it carries data: the freeze and the throttle.
func (c *Censor) Judge(a Attempt) Verdict {
	// the start of a shutdown cuts the flows that are open (D20)
	if cfg, now := c.cfg, c.clk.Now(); !cfg.ShutdownFrom.IsZero() && !now.Before(cfg.ShutdownFrom) && now.Before(cfg.ShutdownTo) {
		return Verdict{Kind: Drop, Until: cfg.ShutdownTo}
	}
	if c.cfg.FreezeBytes > 0 && a.Proto == "tcp" && a.ServerBytes >= c.cfg.FreezeBytes {
		return Verdict{Kind: Freeze}
	}
	if c.cfg.ThrottleBps > 0 {
		return Verdict{Kind: Throttle, RateBps: c.cfg.ThrottleBps}
	}
	return Verdict{Kind: Pass}
}

// End says a flow is over.
func (c *Censor) End(a Attempt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if open := c.andOpen[a.Dst]; open != nil {
		delete(open, a.Flow)
	}
}

// ---------------------------------------------------------------------------- the file

type andFile struct {
	ASNs           []string `json:"asns"`
	JA4s           []string `json:"ja4s"`
	Parallel       int      `json:"parallel"`
	PenaltySeconds int      `json:"penalty_seconds"`
}

type configFile struct {
	FreezeBytes  int      `json:"freeze_bytes"`
	UDPDrop      bool     `json:"udp_drop"`
	AllowSNI     []string `json:"allow_sni"`
	AllowCIDR    []string `json:"allow_cidr"`
	Only443      bool     `json:"only_443"`
	ThrottleBps  int      `json:"throttle_bps"`
	ShutdownFrom string   `json:"shutdown_from"`
	ShutdownTo   string   `json:"shutdown_to"`
	Port443Down  bool     `json:"port_443_down"`
	And          *andFile `json:"and"`
}

// LoadCensorConfig reads a censor described as JSON, so a run can name the rules it measured
// against. A file that cannot mean anything is refused.
func LoadCensorConfig(r io.Reader) (CensorConfig, error) {
	var f configFile
	dec := json.NewDecoder(r)
	// a key nobody knows is a typo, and a typo is a censor with no rules: refuse it
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return CensorConfig{}, fmt.Errorf("censor config: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return CensorConfig{}, errors.New("censor config: data after the first JSON value")
	}
	if f.FreezeBytes < 0 || f.ThrottleBps < 0 {
		return CensorConfig{}, errors.New("censor config: a negative size or rate")
	}
	c := CensorConfig{FreezeBytes: f.FreezeBytes, UDPDrop: f.UDPDrop, Only443: f.Only443,
		ThrottleBps: f.ThrottleBps, Port443Down: f.Port443Down}
	if len(f.AllowSNI) > 0 {
		c.AllowSNI = map[string]bool{}
		for _, s := range f.AllowSNI {
			c.AllowSNI[s] = true
		}
	}
	for _, s := range f.AllowCIDR {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return CensorConfig{}, fmt.Errorf("censor config: allow_cidr %q: %w", s, err)
		}
		c.AllowCIDR = append(c.AllowCIDR, p)
	}
	for name, dst := range map[string]*time.Time{"shutdown_from": &c.ShutdownFrom, "shutdown_to": &c.ShutdownTo} {
		s := f.ShutdownFrom
		if name == "shutdown_to" {
			s = f.ShutdownTo
		}
		if s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return CensorConfig{}, fmt.Errorf("censor config: %s: %w", name, err)
		}
		*dst = t
	}
	if (f.ShutdownFrom == "") != (f.ShutdownTo == "") {
		return CensorConfig{}, errors.New("censor config: a shutdown window needs both shutdown_from and shutdown_to")
	}
	if !c.ShutdownFrom.IsZero() && !c.ShutdownTo.After(c.ShutdownFrom) {
		return CensorConfig{}, errors.New("censor config: shutdown_to must be after shutdown_from")
	}
	if f.And != nil {
		a := f.And
		if len(a.ASNs) == 0 || len(a.JA4s) == 0 || a.Parallel < 1 || a.PenaltySeconds < 1 {
			return CensorConfig{}, errors.New("censor config: the AND rule needs asns, ja4s, parallel >= 1 and penalty_seconds >= 1")
		}
		rule := &AndRule{ASNs: map[string]bool{}, JA4s: map[string]bool{}, Parallel: a.Parallel,
			Penalty: time.Duration(a.PenaltySeconds) * time.Second}
		for _, s := range a.ASNs {
			rule.ASNs[s] = true
		}
		for _, s := range a.JA4s {
			rule.JA4s[s] = true
		}
		c.And = rule
	}
	return c, nil
}
