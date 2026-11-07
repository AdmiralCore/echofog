package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/routepolicy"
)

// errPolicyBlocked marks "the country policy left no direct attempt" (T6.4). Like a failed carrier
// dial it lets the relay path take over, where the entry rule is enforced.
var errPolicyBlocked = errors.New("node: the country policy leaves no direct carrier")

// RouteReport is the rule applied to the last dial of a peer, for the connection report (T11.2).
type RouteReport struct {
	RuleID       string
	TableVersion string
	Net          routepolicy.NetClass
	Dest         routepolicy.DestClass
	// ViaEntry: the first hop was an entry node inside the own country.
	ViaEntry bool
	// Country and CountrySource: the client's own country at that dial and where it came from.
	Country       string
	CountrySource CountrySource
	// Frozen: the network was marked frozen at that dial (T6.4d).
	Frozen bool
}

// SetNetClass changes the kind of the client's network (T6.4).
func (r *Runtime) SetNetClass(c routepolicy.NetClass) { r.netClass.Store(uint32(c)) }

// NetClass returns the current kind of the client's network.
func (r *Runtime) NetClass() routepolicy.NetClass { return routepolicy.NetClass(r.netClass.Load()) }

// RouteReport returns the rule applied to the last dial of peer.
func (r *Runtime) RouteReport(peer DeviceID) (RouteReport, bool) {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	rep, ok := r.routeInfo[peer]
	return rep, ok
}

const maxRouteReports = 256

func (r *Runtime) noteRoute(peer [32]byte, dec routepolicy.Decision, dest routepolicy.DestClass, viaEntry bool) {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	if r.routeInfo == nil {
		r.routeInfo = map[DeviceID]RouteReport{}
	}
	if len(r.routeInfo) >= maxRouteReports {
		for k := range r.routeInfo { // a bounded map: drop an arbitrary old entry
			delete(r.routeInfo, k)
			break
		}
	}
	cc, src := r.OwnCountry()
	r.routeInfo[DeviceID(peer)] = RouteReport{RuleID: dec.RuleID, TableVersion: r.rules.Version,
		Net: r.NetClass().Effective(), Dest: dest, ViaEntry: viaEntry, Country: cc, CountrySource: src}
}

// addrCountry is the country of a card address; a DNS name or an unknown address has none, so it
// counts as foreign.
func (r *Runtime) addrCountry(a *pb.Addr) string {
	host := a.GetHost()
	if host == "" && a.GetUrl() != "" {
		if u, err := url.Parse(a.GetUrl()); err == nil {
			host = u.Hostname()
		}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	c, _ := r.Country(ip)
	return c
}

// cardRoute classifies a peer card by its most domestic address and returns the table row for it.
func (r *Runtime) cardRoute(card *objects.Card) (own string, dest routepolicy.DestClass, dec routepolicy.Decision) {
	own = r.ownCountry()
	dest = routepolicy.DestForeign
	for _, a := range card.Addrs() {
		if d := r.rules.Classify(own, r.addrCountry(a)); d > dest {
			dest = d
		}
	}
	return own, dest, r.rules.Decide(own, r.NetClass(), dest)
}

// applyRoutePolicy drops the attempts that the country policy forbids. A probe may keep TCP
// attempts the policy would otherwise drop, with the byte budget of the row.
func (r *Runtime) applyRoutePolicy(card *objects.Card, attempts []carrier.Attempt, probe bool) (kept []carrier.Attempt, probeBudget int, err error) {
	own := r.ownCountry()
	net := r.NetClass()
	dropped := false
	for _, at := range attempts {
		if at.Kind != carrier.AttemptCarrier || at.Addr == nil {
			kept = append(kept, at)
			continue
		}
		dest := r.rules.Classify(own, r.addrCountry(at.Addr))
		dec := r.rules.Decide(own, net, dest)
		switch {
		case !dec.Direct:
			if probe && dec.Probe > 0 && !routepolicy.IsUDP(at.Suite) {
				kept = append(kept, at)
				if probeBudget == 0 || dec.Probe < probeBudget {
					probeBudget = dec.Probe
				}
				continue
			}
			dropped = true
		case !dec.AllowsSuite(at.Suite):
			dropped = true
		default:
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 && dropped {
		if card == nil { // a bootstrap address
			return nil, 0, errPolicyBlocked
		}
		return nil, 0, fmt.Errorf("%w: %s", errPolicyBlocked, DeviceID(card.DeviceID))
	}
	return kept, probeBudget, nil
}

// udpAllowedTo reports whether the country policy lets UDP go to ip; an unknown address counts as a
// foreign one (the strictest reading). Hole punching is the one UDP use that begins in a relayed
// session, so it asks here.
func (r *Runtime) udpAllowedTo(ip netip.Addr) bool {
	own := r.ownCountry()
	dest := routepolicy.DestForeign
	if ip.IsValid() {
		if c, ok := r.Country(ip.Unmap()); ok {
			dest = r.rules.Classify(own, c)
		}
	}
	return r.rules.Decide(own, r.NetClass(), dest).UDP
}

// errProbeBudget ends a probe connection that used its byte budget.
var errProbeBudget = errors.New("node: probe budget used")

// budgetConn closes a connection when more than its budget of bytes (both directions) has moved.
type budgetConn struct {
	net.Conn
	left atomic.Int64
}

func (c *budgetConn) take(n int) (int, error) {
	for {
		left := c.left.Load()
		if left <= 0 {
			_ = c.Conn.Close()
			return 0, errProbeBudget
		}
		take := int64(n)
		if take > left {
			take = left
		}
		if c.left.CompareAndSwap(left, left-take) {
			return int(take), nil
		}
	}
}

func (c *budgetConn) Write(p []byte) (int, error) {
	n, err := c.take(len(p))
	if err != nil {
		return 0, err
	}
	w, err := c.Conn.Write(p[:n])
	if err == nil && n < len(p) {
		_ = c.Conn.Close()
		return w, errProbeBudget
	}
	return w, err
}

func (c *budgetConn) Read(p []byte) (int, error) {
	n, err := c.take(len(p))
	if err != nil {
		return 0, err
	}
	return c.Conn.Read(p[:n])
}

// probeDialContext wraps the node's socket opener with a byte budget.
func (r *Runtime) probeDialContext(budget int) func(context.Context, string, string) (net.Conn, error) {
	base := r.cfg.DialContext
	if base == nil {
		base = (&net.Dialer{}).DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := base(ctx, network, address)
		if err != nil {
			return nil, err
		}
		b := &budgetConn{Conn: c}
		b.left.Store(int64(budget))
		return b, nil
	}
}

// entryFilter returns the predicate a first hop must satisfy under the row, or nil when any relay
// will do. An entry sits in the client's own country and, on a network that needs one, inside a
// verified range of the rules (H14).
func (r *Runtime) entryFilter(dec routepolicy.Decision, own string) func(*objects.Card) bool {
	if !dec.EntryInCountry {
		return nil
	}
	net := r.NetClass()
	return func(c *objects.Card) bool {
		if c == nil {
			return false
		}
		for _, a := range c.Addrs() {
			ip, err := netip.ParseAddr(a.GetHost())
			if err != nil {
				continue
			}
			if r.rules.Classify(own, r.addrCountry(a)) == routepolicy.DestDomestic &&
				r.rules.EntryAllowed(own, net, ip) {
				return true
			}
		}
		return false
	}
}

// noEntry reports to the application that the policy needs an entry node in the own country and
// returns the error for the caller: unsupported when the rules verify no range at all.
func (r *Runtime) noEntry() error {
	err := ErrNoDomesticEntry
	msg := "Нет входа в своей стране: из этой сети прямое соединение с зарубежными узлами " +
		"недоступно, нужен домашний вход."
	if !r.rules.HasVerifiedEntries() {
		err = ErrRouteUnsupported
		msg = "Версия 2.0 не поддерживает эту сеть: нет проверенного входа в своей стране."
	}
	r.events.Emit(Event{Kind: EventNoDomesticEntry, Message: msg, At: time.Now(), Err: err})
	return err
}
