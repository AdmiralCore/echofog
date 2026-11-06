package behaviour

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
	"github.com/echofog/echofog-core/internal/v2/session"
	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/probe/r2flight"
)

// Product is the client being measured: where it is and what kind of network it is on. The
// product's own routepolicy table, dial plan, dial race and path policy do the rest.
type Product struct {
	Country string
	Net     routepolicy.NetClass
}

// ---------------------------------------------------------------------------- D16 freeze

// FreezeResult is what a transfer of the product to a foreign data centre does under a freeze.
type FreezeResult struct {
	Completed   bool
	Via         string // direct, entry-in-country, udp, tcp-chunked or frozen
	Connections int
	FrozenAt    int    // bytes after which a TCP flow stopped
	Rule        string // the row of the policy table the product used
}

// MeasureFreeze reads the product's routepolicy row for a foreign destination and plays a transfer
// of total bytes against the censor. The freeze is a TCP rule: it is avoided by a home entry (the
// long leg stays inside the country), by UDP, or by carriers that each stay under the budget.
// What is left, a TCP flow with no byte limit, stops at the budget.
func MeasureFreeze(p Product, c CensorConfig, total int) FreezeResult {
	d := routepolicy.Default().Decide(p.Country, p.Net, routepolicy.DestForeign)
	r := FreezeResult{Rule: d.RuleID, Connections: 1}
	switch {
	case c.FreezeBytes == 0 || total <= c.FreezeBytes:
		r.Completed, r.Via = true, "direct"
	case d.EntryInCountry:
		r.Completed, r.Via = true, "entry-in-country"
	case d.UDP && !c.UDPDrop && (d.AllowsSuite(objects.SuiteReality2QUIC) || d.AllowsSuite(objects.SuiteNoise3)):
		r.Completed, r.Via = true, "udp"
	case d.TCPLimit > 0 && d.TCPLimit < c.FreezeBytes:
		r.Completed, r.Via = true, "tcp-chunked"
		r.Connections = (total + d.TCPLimit - 1) / d.TCPLimit
	default:
		r.Via, r.FrozenAt = "frozen", c.FreezeBytes
	}
	return r
}

// ---------------------------------------------------------------------------- D17, D18, D23, D25: the dial race

// DialResult is one run of the product's dial race over a simulated network.
type DialResult struct {
	OK       bool
	Suite    uint32
	Elapsed  time.Duration // virtual time
	Attempts int
	Blocked  int
	Err      string
}

type tally struct {
	mu       sync.Mutex
	attempts int
	blocked  int
	flow     int
}

func (t *tally) next() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempts++
	t.flow++
	return t.flow
}

func (t *tally) block() {
	t.mu.Lock()
	t.blocked++
	t.mu.Unlock()
}

// simDialer is a carrier.Dialer for one suite whose network is the censor: a flow that passes
// connects after one round trip, a dropped one never answers (the product's own dial timeout ends
// it), a reset fails after one round trip.
type simDialer struct {
	clk *VClock
	cen *Censor
	rtt time.Duration
	t   *tally
}

func (d *simDialer) Dial(ctx context.Context, req carrier.DialRequest) (mux.Carrier, error) {
	a := req.Addr
	ip, err := netip.ParseAddr(a.GetHost())
	if err != nil {
		return nil, err
	}
	proto := "tcp"
	if routepolicy.IsUDP(a.GetSuite()) {
		proto = "udp"
	}
	flow := d.t.next()
	att := Attempt{Flow: flow, Proto: proto, Dst: netip.AddrPortFrom(ip, uint16(a.GetPort())),
		SNI: objects.SuiteParams(a).GetSni(), JA4: "ja4-product", ASN: "AS64500"}
	v := d.cen.Begin(att)
	wait := func(after time.Duration) error {
		done := make(chan struct{})
		d.clk.AfterFunc(after, func() { close(done) })
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	switch v.Kind {
	case Drop:
		d.t.block()
		<-ctx.Done() // silence: only the product's own timeout ends this
		return nil, ctx.Err()
	case Reset:
		d.t.block()
		if err := wait(d.rtt); err != nil {
			return nil, err
		}
		return nil, errors.New("connection reset")
	}
	if err := wait(d.rtt); err != nil {
		return nil, err
	}
	return memcarrier.NewQUIC().Client, nil
}

// MeasureDial runs carrier.Plan and carrier.Dial of the product for card, as the product p, over a
// network with the censor c and the round trip rtt, on a virtual clock. The product's routepolicy
// row filters the plan the way the node does. The second attempt's jitter is fixed at 100 ms.
func MeasureDial(card *objects.Card, p Product, c CensorConfig, rtt time.Duration) DialResult {
	clk := NewVClock()
	cen := NewCensor(c, clk)
	t := &tally{}
	reg := carrier.NewRegistry()
	for _, s := range []uint32{objects.SuiteNoise3, objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2} {
		reg.RegisterDialer(s, &simDialer{clk: clk, cen: cen, rtt: rtt, t: t})
	}

	client := netip.MustParseAddr("192.0.2.10")
	self := carrier.LocalInfo{
		Addrs: []netip.Addr{client},
		CountryOf: func(a netip.Addr) (string, bool) {
			if a == client {
				return p.Country, true
			}
			return "DE", true
		},
	}
	dec := routepolicy.Default().Decide(p.Country, p.Net, routepolicy.DestForeign)
	var plan []carrier.Attempt
	for _, a := range carrier.Plan(card, self) {
		if a.Kind == carrier.AttemptCarrier && (!dec.Direct || !dec.AllowsSuite(a.Suite)) {
			continue
		}
		plan = append(plan, a)
	}
	res := DialResult{}
	if len(plan) == 0 {
		res.Err = "no direct attempt: the product goes through a relay or an entry"
		return res
	}

	start := clk.Now()
	type out struct {
		a   carrier.Attempt
		err error
	}
	done := make(chan out, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	go func() {
		conn, att, err := carrier.Dial(ctx, carrier.DialRequest{Card: card}, plan, carrier.DialConfig{
			Registry: reg, Clock: clk, Jitter: func() time.Duration { return 100 * time.Millisecond },
		})
		if conn != nil {
			_ = conn.Close()
		}
		done <- out{att, err}
	}()
	deadline := start.Add(40 * time.Second)
	for {
		select {
		case o := <-done:
			res.Elapsed = clk.Now().Sub(start)
			t.mu.Lock()
			res.Attempts, res.Blocked = t.attempts, t.blocked
			t.mu.Unlock()
			if o.err != nil {
				res.Err = o.err.Error()
				return res
			}
			res.OK, res.Suite = true, o.a.Suite
			return res
		default:
		}
		if clk.Now().After(deadline) {
			res.Err = "the race did not end in 40 s of virtual time"
			return res
		}
		time.Sleep(time.Millisecond)
		clk.Advance(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------- D21 on PathPolicy

// ParallelResult is how many carriers to one (address, SNI) a client opened and whether the AND
// rule fired on them.
type ParallelResult struct {
	MaxOpen int
	Fired   bool
}

// MeasureParallel asks for want parallel carriers to one pair. The product goes through
// session.PathPolicy, which allows two and multiplexes the rest on them (H11); the naive client of
// the control opens one carrier per request.
func MeasureParallel(c CensorConfig, want int, naive bool) ParallelResult {
	clk := NewVClock()
	cen := NewCensor(c, clk)
	pol := session.NewPathPolicy(session.PathPolicyConfig{Clock: clk})
	pair := session.Pair{Host: "203.0.113.15", SNI: "cover.lab"}
	dst := netip.MustParseAddrPort("203.0.113.15:443")
	var r ParallelResult
	open := 0
	for i := 1; i <= want; i++ {
		if !naive {
			if err := pol.Acquire(pair); err != nil {
				continue // at the limit: the request rides an existing carrier
			}
		}
		open++
		r.MaxOpen = max(r.MaxOpen, open)
		v := cen.Begin(Attempt{Flow: i, Proto: "tcp", Dst: dst, SNI: pair.SNI, JA4: "j", ASN: "AS64500"})
		if v.Kind == Drop {
			r.Fired = true
		}
	}
	return r
}

// ---------------------------------------------------------------------------- D24 on PathPolicy

// PenaltyResult is how the product retries a pair that a censor keeps dropping.
type PenaltyResult struct {
	Attempts     []time.Duration // offsets of the handshakes from the first
	FirstBackoff time.Duration
	InWindow120  int // handshakes in the first 120 s
	InWindow600  int // handshakes in the first 600 s
	Recovery     time.Duration
	Grows        bool
}

// MeasurePenalty plays a pair that is dropped until lift (never when lift is 0) against
// session.PathPolicy on a virtual clock for horizon, retrying whenever the policy allows.
func MeasurePenalty(lift, horizon time.Duration) PenaltyResult {
	clk := NewVClock()
	pol := session.NewPathPolicy(session.PathPolicyConfig{Clock: clk})
	pair := session.Pair{Host: "203.0.113.15", SNI: "cover.lab"}
	start := clk.Now()
	var r PenaltyResult
	var backoffs []time.Duration
	for clk.Now().Sub(start) <= horizon {
		if until, ok := pol.Backoff(pair); ok {
			clk.Advance(until.Sub(clk.Now()))
			continue
		}
		at := clk.Now().Sub(start)
		r.Attempts = append(r.Attempts, at)
		if err := pol.Acquire(pair); err != nil {
			break
		}
		if lift > 0 && at >= lift {
			pol.Succeeded(pair)
			pol.Release(pair)
			r.Recovery = at - lift
			break
		}
		pol.Penalize(pair)
		until, _ := pol.Backoff(pair)
		backoffs = append(backoffs, until.Sub(clk.Now()))
	}
	if len(backoffs) > 0 {
		r.FirstBackoff = backoffs[0]
	}
	r.Grows = len(backoffs) > 1 && backoffs[1] > backoffs[0]
	for _, a := range r.Attempts {
		if a < 120*time.Second {
			r.InWindow120++
		}
		if a < 600*time.Second {
			r.InWindow600++
		}
	}
	return r
}

// ---------------------------------------------------------------------------- D22, H16: the product's hello

// CrowdResult is the spread of ClientHello fingerprints among n clients of the product.
type CrowdResult struct {
	Clients  int
	Distinct int
	Top      string
	TopShare float64
}

func helloOf(sni string) (censor.Fingerprint, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return censor.Fingerprint{}, err
	}
	flight, err := r2flight.Flight(key.PublicKey().Bytes(), "198.51.100.20:443", sni, time.Now())
	if err != nil {
		return censor.Fingerprint{}, err
	}
	fp, ok := censor.ParseTLSClientHello(flight)
	if !ok {
		return censor.Fingerprint{}, errors.New("the first flight is not a ClientHello")
	}
	return fp, nil
}

// MeasureCrowd builds n first flights of the reality2-tcp client and counts their fingerprints. It
// says what the product sends; the comparison with a browser population needs real browsers
// (T15.2), and the report says so.
func MeasureCrowd(n int) (CrowdResult, error) {
	count := map[string]int{}
	for i := 0; i < n; i++ {
		fp, err := helloOf("cover.lab")
		if err != nil {
			return CrowdResult{}, err
		}
		count[fp.JA4]++
	}
	r := CrowdResult{Clients: n, Distinct: len(count)}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if count[k] > count[r.Top] || r.Top == "" {
			r.Top = k
		}
	}
	if n > 0 {
		r.TopShare = float64(count[r.Top]) / float64(n)
	}
	return r, nil
}

// ECHResult is whether the product's hello carries an encrypted-client-hello extension.
type ECHResult struct {
	Present bool
	JA4     string
	Bytes   int
}

// MeasureECH reads the hello the product sends. H16: GREASE-ECH stays in, real ECH is not used,
// so the extension must be there and must be the browser's GREASE one.
func MeasureECH() (ECHResult, error) {
	fp, err := helloOf("cover.lab")
	if err != nil {
		return ECHResult{}, fmt.Errorf("measure ECH: %w", err)
	}
	return ECHResult{Present: fp.ECH, JA4: fp.JA4, Bytes: fp.Bytes}, nil
}
