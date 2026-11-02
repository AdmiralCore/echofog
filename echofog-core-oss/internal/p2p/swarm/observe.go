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
// reach the node at; behind a cone NAT it would even make the NAT look symmetric. Relays and
// signaling nodes are asked first: they sit at public addresses and see us as the Internet does.
func (s *Swarm) ObserveFromPeersOn(ctx context.Context, max, port int) []natcheck.Observation {
	var ids []types.PeerID
	seenHost := map[string]bool{}
	peers := s.GetAllPeers()
	sort.SliceStable(peers, func(i, j int) bool { return peers[i].Signals() && !peers[j].Signals() })
	for _, p := range peers {
		if p.ActiveProtocol() == router.ProtoTCPTLS {
			continue
		}
		if port != 0 {
			la, err := netip.ParseAddrPort(p.LocalAddr())
			if err != nil || int(la.Port()) != port {
				continue
			}
		}
		host := p.RemoteAddr()
		if ap, err := netip.ParseAddrPort(host); err == nil {
			host = ap.Addr().String()
		}
		if seenHost[host] {
			continue
		}
		seenHost[host] = true
		ids = append(ids, p.ID())
		if len(ids) == max {
			break
		}
	}

	var mu sync.Mutex
	var out []natcheck.Observation
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, observeTimeout)
			defer cancel()
			if ap, err := s.ObservedAddr(cctx, id); err == nil {
				mu.Lock()
				out = append(out, natcheck.Observation{From: id.String(), Addr: ap})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}
