package swarm

import (
	"context"
	"errors"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/natcheck"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/network/router"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Peers tell each other which address they see a connection coming from. That is exactly how the
// outside world sees us through the NAT, it works without any STUN server (which censors block),
// and answers from peers in different places also reveal whether the NAT hands out the same
// public address for every destination. The idea is libp2p's "identify observed address".

const observeTimeout = 4 * time.Second

// registerObservedAddr answers TypeObservedAddr requests: it tells the caller the address it
// connects from. Only QUIC connections count: over TCP the source port is a throwaway one.
func (s *Swarm) registerObservedAddr() {
	s.RegisterStreamHandler(net_types.TypeObservedAddr, func(st *network.Stream, _ []byte, peerID types.PeerID) {
		defer st.Close()
		p := s.GetPeer(peerID)
		if p == nil || p.ActiveProtocol() == router.ProtoTCPTLS {
			return
		}
		_ = st.WriteFrame(net_types.TypeObservedAddr, []byte(p.RemoteAddr()))
	})
}

// ObservedAddr asks one connected peer which address it sees us at.
func (s *Swarm) ObservedAddr(ctx context.Context, peer types.PeerID) (netip.AddrPort, error) {
	streams, err := s.OpenNewStreams(ctx, peer, 1)
	if err != nil {
		return netip.AddrPort{}, err
	}
	st := streams[0]
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(observeTimeout))
	if err := st.WriteFrame(net_types.TypeObservedAddr, nil); err != nil {
		return netip.AddrPort{}, err
	}
	t, raw, err := st.ReadFrame()
	if err != nil {
		return netip.AddrPort{}, err
	}
	if t != net_types.TypeObservedAddr || len(raw) == 0 || len(raw) > 64 {
		return netip.AddrPort{}, errors.New("bad observed-address reply")
	}
	ap, err := netip.ParseAddrPort(string(raw))
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// ObserveFromPeers asks up to max connected QUIC peers, preferring different hosts so their
// answers are independent, and returns what they saw.
func (s *Swarm) ObserveFromPeers(ctx context.Context, max int) []natcheck.Observation {
	return s.ObserveFromPeersOn(ctx, max, 0)
}

// ObserveFromPeersOn is ObserveFromPeers for the peers connected through our socket on port (0:
// any). A connection from another socket (a REALITY dial opens its own, so does a birthday punch)
// goes through the NAT with a mapping of its own, and its port says nothing about the one peers
// reach the node at; behind a cone NAT it would even make the NAT look symmetric.
//
// Nothing proves a peer's answer, so who is asked matters (see pickObservers): the servers the
// node was told about (bootstrap, invitations) and its guards first, then peers it dialed itself,
// and peers that dialed in last; and peers in different networks before a second one from the same
// network. What a peer announces about itself (relay, signaling) does not move it up: anybody can
// announce that.
func (s *Swarm) ObserveFromPeersOn(ctx context.Context, max, port int) []natcheck.Observation {
	var cands []observerCand
	for _, p := range s.GetAllPeers() {
		if p.ActiveProtocol() == router.ProtoTCPTLS {
			continue
		}
		if port != 0 {
			la, err := netip.ParseAddrPort(p.LocalAddr())
			if err != nil || int(la.Port()) != port {
				continue
			}
		}
		ra, err := netip.ParseAddrPort(p.RemoteAddr())
		if err != nil {
			continue
		}
		tier := tierInbound
		switch {
		case s.isExplicitPeer(p.ID()) || s.IsGuard(p.ID()):
			tier = tierTold
		case p.Outbound():
			tier = tierDialed
		}
		cands = append(cands, observerCand{id: p.ID(), host: ra.Addr().Unmap(), tier: tier})
	}
	ids := pickObservers(cands, max)

	var mu sync.Mutex
	got := make([]*natcheck.Observation, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, observeTimeout)
			defer cancel()
			if ap, err := s.ObservedAddr(cctx, id); err == nil {
				mu.Lock()
				got[i] = &natcheck.Observation{From: id.String(), Addr: ap}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// in the order they were picked, the most trusted first: Classify gives them the edge when
	// two ports are equally backed
	var out []natcheck.Observation
	for _, o := range got {
		if o != nil {
			out = append(out, *o)
		}
	}
	return out
}

// How far a peer's answer about our address is trusted, best first.
const (
	tierTold    = iota // a bootstrap server, a peer from an invitation, a guard
	tierDialed         // a peer this node dialed itself, at an address it chose
	tierInbound        // a peer that dialed in: anybody can
)

type observerCand struct {
	id   types.PeerID
	host netip.Addr
	tier int
}

// pickObservers chooses up to max observers: by tier, and within the picks one host per address
// and one per network (an IPv4 /16, an IPv6 /32) before a second from the same network, so a
// handful of hosts one party rents side by side do not answer for the whole Internet.
func pickObservers(cands []observerCand, max int) []types.PeerID {
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].tier < cands[j].tier })
	var ids []types.PeerID
	seenHost := map[netip.Addr]bool{}
	seenNet := map[netip.Prefix]bool{}
	taken := make([]bool, len(cands))
	for pass := 0; pass < 2 && len(ids) < max; pass++ {
		for i, c := range cands {
			if len(ids) == max {
				break
			}
			if taken[i] || seenHost[c.host] {
				continue
			}
			nw := observerNet(c.host)
			if pass == 0 && seenNet[nw] {
				continue // a second one from this network only if nothing else is left
			}
			taken[i], seenHost[c.host], seenNet[nw] = true, true, true
			ids = append(ids, c.id)
		}
	}
	return ids
}

// observerNet is the network a host counts toward: an IPv4 /16, an IPv6 /32 (about one provider).
func observerNet(a netip.Addr) netip.Prefix {
	bits := 16
	if a.Is6() {
		bits = 32
	}
	p, _ := a.Prefix(bits)
	return p
}
