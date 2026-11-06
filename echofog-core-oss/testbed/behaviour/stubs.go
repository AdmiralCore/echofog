// Package behaviour is the launchpad's behaviour, network and resilience module (T15.0c): the
// censor rules D16–D25 as a deterministic model on a virtual clock, the product's own code driven
// against them (routepolicy, carrier.Plan and carrier.Dial, session.PathPolicy), a netem for the
// real noise3 carrier, and a soak that counts goroutines, descriptors and heap.
//
// Nothing here needs Docker. What does need a live stand (NAT, IPv6, DNS, cut routers, 20 to 5
// nodes, a two-hour soak) is listed in Report.NotChecked and never reported as passed.
package behaviour

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

var errTodo = errors.New("not implemented")

// ---------------------------------------------------------------------------- virtual clock

// VClock is a manually advanced carrier.Clock.
type VClock struct{}

func NewVClock() *VClock                                          { return &VClock{} }
func (*VClock) Now() time.Time                                    { return time.Time{} }
func (*VClock) AfterFunc(d time.Duration, f func()) carrier.Timer { return stubTimer{} }

type stubTimer struct{}

func (stubTimer) Stop() bool            { return false }
func (*VClock) Advance(d time.Duration) {}

var _ carrier.Clock = (*VClock)(nil)

// ---------------------------------------------------------------------------- censor rules

type VerdictKind int

const (
	Pass VerdictKind = iota
	Drop
	Reset
	Freeze
	Throttle
)

func (k VerdictKind) String() string { return "?" }

type Verdict struct {
	Kind    VerdictKind
	RateBps int
	Until   time.Time
}

// Attempt is one flow as the censor sees it.
type Attempt struct {
	Flow        int
	Proto       string // tcp or udp
	Dst         netip.AddrPort
	SNI         string
	JA4         string
	ASN         string
	ServerBytes int
}

type AndRule struct {
	ASNs     map[string]bool
	JA4s     map[string]bool
	Parallel int
	Penalty  time.Duration
}

type CensorConfig struct {
	FreezeBytes  int
	UDPDrop      bool
	AllowSNI     map[string]bool
	AllowCIDR    []netip.Prefix
	Only443      bool
	ThrottleBps  int
	ShutdownFrom time.Time
	ShutdownTo   time.Time
	And          *AndRule
	Port443Down  bool
}

type Censor struct{}

func NewCensor(cfg CensorConfig, clock carrier.Clock) *Censor { return &Censor{} }
func (c *Censor) Begin(a Attempt) Verdict                     { return Verdict{} }
func (c *Censor) Judge(a Attempt) Verdict                     { return Verdict{} }
func (c *Censor) End(a Attempt)                               {}

// LoadCensorConfig reads a censor described as JSON.
func LoadCensorConfig(r io.Reader) (CensorConfig, error) { return CensorConfig{}, errTodo }

// ---------------------------------------------------------------------------- the product against it

type Product struct {
	Country string
	Net     routepolicy.NetClass
}

type FreezeResult struct {
	Completed   bool
	Via         string
	Connections int
	FrozenAt    int
	Rule        string
}

func MeasureFreeze(p Product, c CensorConfig, total int) FreezeResult { return FreezeResult{} }

type DialResult struct {
	OK       bool
	Suite    uint32
	Elapsed  time.Duration
	Attempts int
	Blocked  int
	Err      string
}

// MeasureDial runs the product's dial race (carrier.Plan, carrier.Dial) over a simulated network
// whose censor is c, on a virtual clock.
func MeasureDial(card *objects.Card, p Product, c CensorConfig, rtt time.Duration) DialResult {
	return DialResult{}
}

type ParallelResult struct {
	MaxOpen int
	Fired   bool
}

func MeasureParallel(c CensorConfig, want int, naive bool) ParallelResult { return ParallelResult{} }

type PenaltyResult struct {
	Attempts     []time.Duration
	FirstBackoff time.Duration
	InWindow120  int
	InWindow600  int
	Recovery     time.Duration
	Grows        bool
}

func MeasurePenalty(lift, horizon time.Duration) PenaltyResult { return PenaltyResult{} }

type CrowdResult struct {
	Clients  int
	Distinct int
	Top      string
	TopShare float64
}

func MeasureCrowd(n int) (CrowdResult, error) { return CrowdResult{}, errTodo }

type ECHResult struct {
	Present bool
	JA4     string
	Bytes   int
}

func MeasureECH() (ECHResult, error) { return ECHResult{}, errTodo }

// ---------------------------------------------------------------------------- netem and noise3

type NetemConfig struct {
	Loss  float64
	Delay time.Duration
	MTU   int
	Seed  int64
}

type NetemStats struct{ Forwarded, Dropped, Oversize int64 }

type Netem struct{}

func StartNetem(target string, cfg NetemConfig) (*Netem, error) { return nil, errTodo }
func (n *Netem) Addr() string                                   { return "" }
func (n *Netem) Cut(d time.Duration)                            {}
func (n *Netem) Rebind()                                        {}
func (n *Netem) Stats() NetemStats                              { return NetemStats{} }
func (n *Netem) Close()                                         {}

type TransferResult struct {
	Connected   bool
	ConnectTime time.Duration
	Total       time.Duration
	Bytes       int64
	Echoed      int64
	MaxGap      time.Duration
	Err         string
}

// StartEcho runs a noise3 echo server on loopback and returns its address.
func StartEcho(seed int64) (addr string, stop func(), err error) { return "", func() {}, errTodo }

// RunTransfer dials addr with noise3, sends n bytes and counts the echo. hook is called with the
// bytes sent so far.
func RunTransfer(ctx context.Context, addr string, seed int64, n int64, hook func(sent int64)) TransferResult {
	return TransferResult{}
}

type SoakConfig struct {
	Duration             time.Duration
	ListenerRestartEvery int
}

type SoakResult struct {
	Cycles          int
	Errors          int
	GoroutinesStart int
	GoroutinesEnd   int
	HeapStart       uint64
	HeapEnd         uint64
	FDsStart        int
	FDsEnd          int
}

func RunSoak(ctx context.Context, cfg SoakConfig) SoakResult { return SoakResult{} }

// ---------------------------------------------------------------------------- the report

type Options struct {
	Short bool
}

type Report struct {
	Freeze     []FreezeResult
	Dial       map[string]DialResult
	Parallel   map[string]ParallelResult
	Penalty    PenaltyResult
	Crowd      CrowdResult
	ECH        ECHResult
	Net        map[string]TransferResult
	Soak       SoakResult
	NotChecked []string
	Findings   []string
}

func RunAll(ctx context.Context, o Options) Report { return Report{} }
