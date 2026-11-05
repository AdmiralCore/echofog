package selfcover

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier/reality2/profile"
)

// Report is the result of one re-measurement: for each transport the profile of the website,
// the profile of a selected (token-carrying) connection, and what differs between them. The same
// code and certificate make the list empty; a non-empty list means something broke the
// equivalence (a library update, a different certificate chain) and the suite must not stay
// enabled on this card.
type Report struct {
	At time.Time
	// Reason is "start", "interval" or "certificate".
	Reason string
	// Site and Ours are keyed by transport; Diffs holds the Compare result.
	Site  map[profile.Transport]*profile.CoverProfile
	Ours  map[profile.Transport]*profile.CoverProfile
	Diffs map[profile.Transport][]profile.Diff
	// Unstable lists, per transport, the items the website itself varies between connections
	// (the layout of QUIC packets depends on timing). The own server is the same stack, so such
	// an item is no divergence of ours; it is reported, not counted against OK.
	Unstable map[profile.Transport][]string
	// Serial is the serial number of the leaf the measurement saw.
	Serial string
}

// OK reports that every measured transport matched.
func (r *Report) OK() bool {
	for _, d := range r.Diffs {
		if len(d) > 0 {
			return false
		}
	}
	return len(r.Site) > 0
}

// variableSlack is how far a variable length may lie outside the range the website showed. An
// ECDSA signature has 3 to 4 possible DER lengths, and the extreme ones are rare: 60 samples of
// the website can miss one that 5 samples of our own connection hit. The own server runs the same
// code with the same key, so the distribution is the same; slots that are constant on both sides
// are still compared exactly.
const variableSlack = 4

// CompareOwn compares the profile of the website with that of a selected connection for the own
// cover. It is profile.Compare with three allowances that hold only because both sides are one
// stack with one certificate: an item the website itself varies is returned as unstable instead
// of as a difference, the packing of a flight may vary on either side, and a length that varies
// between samples may exceed the observed range by variableSlack bytes.
func CompareOwn(site, ours *profile.CoverProfile) (diffs []profile.Diff, unstable []string) {
	for _, d := range profile.Compare(site, ours) {
		switch {
		case d.Cover == "unstable":
			unstable = append(unstable, d.Item)
		case d.Item == profile.ItemPacking && d.Ours == "unstable":
			// How a flight is cut into segments or packets depends on timing, and the selected
			// connection also runs through the gate: under load it varies in a window where the
			// website's longer measurement did not. One stack serves both, so this is not a
			// difference (T2.6b-2 F2). Every other item unstable on our side only still is one.
			unstable = append(unstable, d.Item)
		case onlyVariableSlack(d, site, ours):
		default:
			diffs = append(diffs, d)
		}
	}
	return diffs, unstable
}

func onlyVariableSlack(d profile.Diff, site, ours *profile.CoverProfile) bool {
	var c, o []profile.Slot
	switch d.Item {
	case profile.ItemFlightLen:
		c, o = site.Flight, ours.Flight
	case profile.ItemMsgLen:
		c, o = site.Inner, ours.Inner
	case profile.ItemTicketLen:
		c, o = site.Tickets, ours.Tickets
	default:
		return false
	}
	if len(d.Slots) == 0 {
		return false
	}
	for _, i := range d.Slots {
		if i >= len(c) || i >= len(o) {
			return false
		}
		varies := c[i].Min != c[i].Max || o[i].Min != o[i].Max
		if !varies || o[i].Max < c[i].Min-variableSlack || o[i].Min > c[i].Max+variableSlack {
			return false
		}
	}
	return true
}

// uniformInterval is the re-measurement period of wire.md §3.4: uniform(5, 7) hours.
func uniformInterval() time.Duration {
	const span = 2 * time.Hour
	n, err := rand.Int(rand.Reader, big.NewInt(int64(span)))
	if err != nil {
		return 6 * time.Hour
	}
	return 5*time.Hour + time.Duration(n.Int64())
}

// remeasureLoop measures once the certificate is in, then again every Interval and whenever the
// certificate changes (an ACME renewal changes the length of the Certificate message, which the
// profile of the cover includes).
func (n *Node) remeasureLoop(ctx context.Context) {
	reason := "start"
	for {
		// The first certificate triggers a change signal too; it is folded into "start".
		select {
		case <-n.changed:
		default:
		}
		rep, err := n.Remeasure(ctx, reason)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.report(fmt.Errorf("selfcover: measuring the cover: %w", err))
		} else if n.cfg.OnReport != nil {
			n.cfg.OnReport(rep)
		}
		timer := make(chan struct{})
		sleepCtx, cancel := context.WithCancel(ctx)
		go func() {
			_ = n.cfg.Sleep(sleepCtx, n.cfg.Interval())
			close(timer)
		}()
		select {
		case <-ctx.Done():
			cancel()
			return
		case <-n.changed:
			reason = "certificate"
		case <-timer:
			reason = "interval"
		}
		cancel()
	}
}

// Remeasure measures the website and a selected connection over every enabled transport and
// compares them (the records after the handshake are left out, see ProbeConfig.IgnorePost: what
// the website and the carrier send there is application data), with the same probe the cover profile of T2.6b-1 uses. The profile of the
// website is stored. Runs are serialized.
func (n *Node) Remeasure(ctx context.Context, reason string) (*Report, error) {
	n.remeasures.Lock()
	defer n.remeasures.Unlock()
	defer n.clearOwn()

	leaf := n.certs.Leaf()
	rep := &Report{At: n.cfg.Now(), Reason: reason,
		Site: map[profile.Transport]*profile.CoverProfile{}, Ours: map[profile.Transport]*profile.CoverProfile{},
		Diffs: map[profile.Transport][]profile.Diff{}, Unstable: map[profile.Transport][]string{}}
	if leaf != nil {
		rep.Serial = leaf.SerialNumber.String()
	}

	siteProber := profile.NewProber(profile.ProbeConfig{Samples: n.cfg.Samples, Settle: n.cfg.Settle, OnLocal: n.registerOwn, IgnorePost: true})
	oursProber := profile.NewProber(profile.ProbeConfig{Samples: 5, Settle: n.cfg.Settle, NoRequest: true, IgnorePost: true, OnLocal: n.registerOwn,
		PrepareTCP: n.minter.tcpPrepare, PrepareQUIC: n.minter.quicHook})

	type job struct {
		tr   profile.Transport
		addr string
		site func(context.Context, profile.Target) (*profile.CoverProfile, error)
		ours func(context.Context, profile.Target) (*profile.CoverProfile, error)
	}
	var jobs []job
	if n.tcpAddr != nil {
		jobs = append(jobs, job{profile.TCP, loopbackAddr(n.tcpAddr), siteProber.MeasureTCP, oursProber.MeasureTCP})
	}
	if n.udpAddr != nil {
		jobs = append(jobs, job{profile.QUIC, loopbackAddr(n.udpAddr), siteProber.MeasureQUIC, oursProber.MeasureQUIC})
	}
	for _, j := range jobs {
		t := profile.Target{Host: n.certs.Domain(), Addr: j.addr, SNI: n.certs.Domain()}
		site, err := j.site(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("%s website: %w", j.tr, err)
		}
		ours, err := j.ours(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("%s selected connection: %w", j.tr, err)
		}
		rep.Site[j.tr], rep.Ours[j.tr] = site, ours
		rep.Diffs[j.tr], rep.Unstable[j.tr] = CompareOwn(site, ours)
		if err := profile.Save(ctx, n.cfg.KV, site); err != nil {
			n.report(fmt.Errorf("selfcover: storing the profile: %w", err))
		}
	}
	return rep, nil
}
