package node

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	coreNode "github.com/echofog/echofog-core/pkg/node"
)

// How a call reaches the other members: over a direct link, or through a relay because no direct
// one could be made, and then why and what would help. The answer depends on this side's NAT:
// behind the provider's carrier-grade NAT no router setting helps and only a public IP or a fast
// relay does, while a router that merely lets nobody in is fixed by UPnP or a port forward.
// Advice that does not fit the case sends people to change settings that cannot help, so each
// case gets its own. Anonymous mode, a proxy, calls set to a relay and guard mode put a call on a
// relay on purpose: that is said, with no advice. Of the other side only what its call profile
// declares is known (see memberReach): not whether it sits behind a carrier's NAT.

// Reasons a call link goes through a relay, as codes the UI translates and maps to advice.
const (
	pathAnonymous = "anonymous" // on purpose: anonymous mode meets every peer at a relay
	pathProxy     = "proxy"     // on purpose: behind a proxy, a direct call would show the address
	pathSettings  = "settings"  // on purpose: calls are set to go through a relay
	pathGuard     = "guard"     // on purpose: guard mode keeps a user across the border on a relay
	pathCarrier   = "cgnat"     // this side is behind the provider's NAT: no router setting helps
	pathRouter    = "router"    // this side's router lets nobody in: UPnP or a port forward helps
	pathChecking  = "checking"  // this side's NAT is not known yet
	pathRemote    = "remote"    // this side is reachable: the other side's network is in the way
	pathTrying    = "trying"    // relayed a moment ago: a direct link is still being looked for
)

// pathOrder ranks the reasons for a call's summary, the one that matters most first: what the
// user chose explains every link, then what the user can act on, then what passes by itself.
var pathOrder = []string{pathAnonymous, pathProxy, pathSettings, pathCarrier, pathRouter, pathChecking, pathRemote, pathTrying, pathGuard}

// pathTryingFor is how long a link counts as new after it was first seen going through a relay: a
// direct attempt runs for up to 40 s (see tryDirectLater).
const pathTryingFor = 45 * time.Second

// When a relay costs the call something people notice, the badge says so.
const (
	pathSlowPing   = 250 // ms: a round trip through the relay that makes talking awkward
	pathCappedPart = 0.6 // own streams going out at less than this part of their preset
)

// pathFacts is what decides why a link to one call member goes through a relay.
type pathFacts struct {
	Anonymous bool   // anonymous mode
	Proxy     bool   // behind a proxy, and calls are not set to go direct
	Forced    bool   // calls are set to go through a relay
	Guard     bool   // guard mode keeps the link to this member on a relay
	NAT       string // this side: public, cone, symmetric, unknown
	Mapped    bool   // the router forwards a port to this side
	Carrier   bool   // this side is behind a carrier-grade (or double) NAT
	New       bool   // first seen relayed less than pathTryingFor ago, and direct links are tried
}

// pathReason is why a relayed link is not direct, as one of the path* codes.
func pathReason(f pathFacts) string {
	switch {
	case f.Anonymous:
		return pathAnonymous
	case f.Proxy:
		return pathProxy
	case f.Forced:
		return pathSettings
	case f.Guard:
		return pathGuard
	case f.New:
		// a hole punch can get through even a carrier-grade NAT that keeps one mapping
		return pathTrying
	case f.Carrier:
		return pathCarrier
	case f.NAT == "public" || f.Mapped:
		return pathRemote
	case f.NAT == "" || f.NAT == "unknown":
		return pathChecking
	default:
		return pathRouter
	}
}

// pathSummary is the reason that matters most among those of the relayed members ("" for none).
func pathSummary(reasons []string) string {
	best := len(pathOrder)
	for _, r := range reasons {
		for i, o := range pathOrder {
			if o == r && i < best {
				best = i
			}
		}
	}
	if best == len(pathOrder) {
		return ""
	}
	return pathOrder[best]
}

// pathHurts says what the relay costs the call that people notice: "ping" (a slow round trip),
// "capped" (own streams held well below their preset), or "".
func pathHurts(slowestPingMs, sendKbps, presetKbps int) string {
	switch {
	case slowestPingMs >= pathSlowPing:
		return "ping"
	case presetKbps > 0 && float64(sendKbps) < pathCappedPart*float64(presetKbps):
		return "capped"
	}
	return ""
}

// memberReach reads a member's NAT from its call profile, where it declares how many members it
// can feed (Node.CallRelayCapacity) and whether it is reached only through relays: "open" (others
// reach it: a public address or a forwarded port), "cone", "symmetric", or "" when the profile
// cannot tell. declared: the member sent this profile itself (this node coordinates the call);
// coordinator: it is the coordinator's own, whose RelayOnly is its own but whose fan-out the voice
// tree raises to DefaultCoordinatorFanout when it declared less than 2.
func memberReach(p tree.PeerNodeProfile, declared, coordinator bool) string {
	switch {
	case p.RelayOnly && (declared || coordinator):
		return "symmetric"
	case coordinator && p.MaxRelayChildren == tree.DefaultCoordinatorFanout:
		return "" // raised: symmetric behind a forward, or not known yet
	case p.MaxRelayChildren == coreNode.CallFanoutReachable:
		return "open"
	case p.MaxRelayChildren == coreNode.CallFanoutCone:
		return "cone"
	}
	return ""
}

// CallPath is how this node's call reaches the other members, for the badge and panel of the call.
type CallPath struct {
	InCall  bool             `json:"inCall"`
	Members []CallPathMember `json:"members"` // the other members
	Relayed int              `json:"relayed"` // members reached through a relay
	Reason  string           `json:"reason"`  // the reason that matters most among them; "" none relayed
	Hurts   string           `json:"hurts"`   // "ping", "capped" or "": what the relay costs
	PingMs  int              `json:"pingMs"`  // the slowest round trip through a relay, 0 not measured yet
	Route   string           `json:"route"`   // the call route in effect: auto, direct, relay
	// This side's NAT, as the relay card shows it.
	NAT        string `json:"nat"` // public, cone, symmetric, unknown
	Mapped     bool   `json:"mapped"`
	CarrierNAT bool   `json:"carrierNat"`
	ListenPort int    `json:"listenPort"` // the fixed listen port to forward, 0 random
	// Own streams: the bitrate they go out at now and the one their presets ask for (0 none).
	SendKbps   int `json:"sendKbps"`
	PresetKbps int `json:"presetKbps"`
}

// CallPathMember is how this node reaches one member.
type CallPathMember struct {
	Peer string `json:"peer"`
	// Link: "direct", "relay", or "none" (no link of its own: in a group the member's voice comes
	// through the others, in a call of two the link is still being made).
	Link      string `json:"link"`
	Relay     string `json:"relay"`     // the relay's peer ID
	RelayAddr string `json:"relayAddr"` // where it is
	PingMs    int    `json:"pingMs"`    // round trip to the member through the relay, 0 not measured yet
	Reason    string `json:"reason"`    // why through a relay (a path* code)
	TheirNAT  string `json:"theirNat"`  // the member's NAT as its profile declares it (memberReach)
}

// callPathSeen remembers when each member of the current call was first seen going through a
// relay, so a new relayed link says a direct one is still being looked for.
type callPathSeen struct {
	mu     sync.Mutex
	callID string
	since  map[types.PeerID]time.Time
}

func (c *callPathSeen) relayedFor(callID string, peer types.PeerID, now time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.callID != callID || c.since == nil {
		c.callID, c.since = callID, map[types.PeerID]time.Time{}
	}
	at, ok := c.since[peer]
	if !ok {
		c.since[peer] = now
		return 0
	}
	return now.Sub(at)
}

// GetCallPath says how the active call reaches each member. Each look also pings every relayed
// link once (at most every half second): the round trip it measures shows in the next look.
func (n *NodeApi) GetCallPath() CallPath {
	if n.v2Unavailable() != nil {
		return CallPath{}
	}

	p := CallPath{Members: []CallPathMember{}}
	nd := n.node
	if nd == nil || nd.CallManager == nil || nd.Swarm == nil || nd.Cfg == nil {
		return p
	}
	s, ok := nd.CallManager.Snapshot()
	if !ok {
		return p
	}
	p.InCall, p.Route = true, nd.CallRoute()
	reach := nd.RelayReachability()
	p.NAT, p.Mapped = reach.NAT, reach.Mapped
	p.CarrierNAT = nd.NAT != nil && nd.NAT.CarrierNAT()
	p.ListenPort = relaySettingsFromConfig(nd.Cfg).ListenPort
	p.SendKbps, p.PresetKbps = nd.CallManager.SendRate()

	base := pathFacts{
		Anonymous: nd.AnonMode() != "",
		Forced:    nd.Cfg.Network.CallRoute == config.CallRelay,
		NAT:       p.NAT,
		Mapped:    p.Mapped,
		Carrier:   p.CarrierNAT,
	}
	base.Proxy = nd.ProxyOn() && p.Route == config.CallRelay && !base.Forced
	guarded := p.Route != config.CallDirect && nd.Swarm.GuardModeActive() && !nd.ProxyOn()

	profiles, coordinator, declared := nd.CallManager.MemberProfiles()
	members := append([]types.PeerID(nil), s.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].String() < members[j].String() })
	now := time.Now()
	var reasons []string
	for _, id := range members {
		if id == nd.ID {
			continue
		}
		m := CallPathMember{Peer: id.String(), Link: "none"}
		if prof, ok := profiles[id]; ok {
			m.TheirNAT = memberReach(prof, declared, id == coordinator)
		}
		if nd.Swarm.GetPeer(id) != nil {
			m.Link = "direct"
		} else if rc := nd.RelayClient; rc != nil {
			if r, ok := rc.RouteTo(id); ok {
				m.Link, m.Relay, m.RelayAddr = "relay", r.Relay.String(), nd.Swarm.GetPeerAddr(r.Relay)
				m.PingMs = int(r.RTT.Milliseconds())
				rc.PingCircuit(id)
				f := base
				if guarded {
					addr := ""
					if nd.DhtEngine != nil {
						addr = nd.DhtEngine.KnownAddr(id)
					}
					f.Guard = nd.Swarm.UserLinkViaRelay(id, addr)
				}
				f.New = nd.HolePunch != nil && n.callPath.relayedFor(s.CallID, id, now) < pathTryingFor
				m.Reason = pathReason(f)
				reasons = append(reasons, m.Reason)
				p.Relayed++
				p.PingMs = max(p.PingMs, m.PingMs)
			}
		}
		p.Members = append(p.Members, m)
	}
	p.Reason = pathSummary(reasons)
	if p.Relayed > 0 {
		p.Hurts = pathHurts(p.PingMs, p.SendKbps, p.PresetKbps)
	}
	return p
}

// CallRelay is a relay this node can put a call through.
type CallRelay struct {
	PeerID     string `json:"peerId"`
	Addr       string `json:"addr"` // "" for a relay known only from its advert
	Connected  bool   `json:"connected"`
	PingMs     int    `json:"pingMs"`     // round trip to the relay, 0 no answer (or not connected)
	PricePerMB int64  `json:"pricePerMb"` // micro-USDT per MB its advert asks; 0 free
	Mine       bool   `json:"mine"`       // one the user added by a link, or an invitation named
	Guard      bool   `json:"guard"`      // one of the node's guards
	Carrying   int    `json:"carrying"`   // members of the current call it carries now
}

const (
	callRelaysMax  = 16
	callRelayPings = 2500 * time.Millisecond // a relay that answers slower is shown without a ping
)

// GetCallRelays lists the relays a call can go through: the connected ones, best first, each
// pinged, then the paid ones known from their adverts. Nothing can be bought yet.
func (n *NodeApi) GetCallRelays() []CallRelay {
	if n.v2Unavailable() != nil {
		return nil
	}

	out := []CallRelay{}
	nd := n.node
	if nd == nil || nd.Swarm == nil || nd.RelayClient == nil {
		return out
	}
	rc := nd.RelayClient
	carrying := map[types.PeerID]int{}
	if s, ok := callSnapshot(n); ok {
		for _, id := range s {
			if r, ok := rc.RouteTo(id); ok && nd.Swarm.GetPeer(id) == nil {
				carrying[r.Relay]++
			}
		}
	}
	seen := map[types.PeerID]bool{}
	var ids []types.PeerID
	for _, p := range rc.GetRankedViableRelays(types.PeerID{}) {
		if len(out) >= callRelaysMax {
			break
		}
		id := p.ID()
		price, _ := rc.RelayPrice(id)
		seen[id] = true
		ids = append(ids, id)
		out = append(out, CallRelay{PeerID: id.String(), Addr: p.Addr(), Connected: true, PricePerMB: price,
			Mine: rc.IsPreferred(id), Guard: nd.Swarm.IsGuard(id), Carrying: carrying[id]})
	}
	pings := n.pingRelays(ids)
	for i := range ids {
		out[i].PingMs = pings[i]
	}
	var paid []CallRelay
	for id, price := range rc.PricedRelays() {
		if !seen[id] && len(out)+len(paid) < callRelaysMax {
			paid = append(paid, CallRelay{PeerID: id.String(), PricePerMB: price})
		}
	}
	sort.Slice(paid, func(i, j int) bool {
		if paid[i].PricePerMB != paid[j].PricePerMB {
			return paid[i].PricePerMB < paid[j].PricePerMB
		}
		return paid[i].PeerID < paid[j].PeerID
	})
	return append(out, paid...)
}

// pingRelays measures the round trip to each relay at once; a relay that does not answer within
// callRelayPings gets 0.
func (n *NodeApi) pingRelays(ids []types.PeerID) []int {
	var mu sync.Mutex
	out := make([]int, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id types.PeerID) {
			defer wg.Done()
			start := time.Now()
			if !n.node.Swarm.Answers(id) {
				return
			}
			mu.Lock()
			out[i] = max(1, int(time.Since(start).Milliseconds()))
			mu.Unlock()
		}(i, id)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(callRelayPings):
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]int(nil), out...)
}

// callSnapshot is the other members of the active call.
func callSnapshot(n *NodeApi) ([]types.PeerID, bool) {
	if n.node == nil || n.node.CallManager == nil {
		return nil, false
	}
	s, ok := n.node.CallManager.Snapshot()
	if !ok {
		return nil, false
	}
	var out []types.PeerID
	for _, id := range s.Members {
		if id != n.node.ID {
			out = append(out, id)
		}
	}
	return out, true
}

var errNothingRelayed = errors.New("no member of the call goes through a relay")

// SwitchCallRelay moves every call member this node reaches through a relay onto another relay:
// via (a relay's peer ID) when given, else the best one other than the current. Members stay
// where they are when no relay can take them; it fails only when none moved.
func (n *NodeApi) SwitchCallRelay(via string) error {
	if err := n.v2Unavailable(); err != nil {
		return ErrV2NotBuilt
	}

	members, ok := callSnapshot(n)
	if !ok {
		return errCallsUnavailable
	}
	nd := n.node
	if nd.RelayClient == nil || nd.Swarm == nil {
		return errNothingRelayed
	}
	var viaID types.PeerID
	if via != "" {
		id, err := types.PeerIDFromString(via)
		if err != nil {
			return err
		}
		viaID = id
	}
	var relayed []types.PeerID
	for _, id := range members {
		if _, ok := nd.RelayClient.RouteTo(id); ok && nd.Swarm.GetPeer(id) == nil {
			relayed = append(relayed, id)
		}
	}
	if len(relayed) == 0 {
		return errNothingRelayed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := make([]error, len(relayed))
	var wg sync.WaitGroup
	for i, id := range relayed {
		wg.Add(1)
		go func(i int, id types.PeerID) {
			defer wg.Done()
			_, errs[i] = nd.RelayClient.SwitchCircuit(ctx, id, viaID)
		}(i, id)
	}
	wg.Wait()
	for _, err := range errs {
		if err == nil {
			return nil
		}
	}
	return errs[0]
}
