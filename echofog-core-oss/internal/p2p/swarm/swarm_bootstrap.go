package swarm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// findAndConnectToPeers dials the peers this node was connected to before, freshest first,
// skipping those whose last tries failed until their retry is due.
func (s *Swarm) findAndConnectToPeers() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.dialCandidates(s.historyCandidates(startupHistoryDials))
	}()
}

// expectPeer tells the transport whose address addr is: with obfuscation on, the packets to a node
// are keyed by its peer ID, so every dial has to name it.
// WSTransport is the optional WebSocket transport, used to reach peers at ws:// or wss://
// addresses (for going behind a CDN).
type WSTransport interface {
	Dial(ctx context.Context, url string) error
	ExpectPeer(addr string, id types.PeerID)
	ExpectPeerSNI(addr string, id types.PeerID, sni string)
	ConnChan() <-chan network.NewConnEvent
}

// SetWSTransport attaches a WebSocket transport: its inbound peers register with the swarm, and
// addresses starting with ws:// or wss:// are dialed through it.
func (s *Swarm) SetWSTransport(w WSTransport) {
	s.wsTransport = w
	s.shareLimits(w)
	s.wg.Add(1)
	go s.registrationLoop(w.ConnChan())
}

func isWSAddr(addr string) bool {
	return strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://")
}

func (s *Swarm) expectPeer(addr string, id types.PeerID) {
	sni := s.GetPeerSNI(id)
	if sni == "" {
		_, _, extSNI, _ := network.SplitPeerAddrSNI(addr)
		sni = extSNI
	}
	if isWSAddr(addr) {
		if s.wsTransport != nil {
			if sni != "" {
				s.wsTransport.ExpectPeerSNI(addr, id, sni)
			} else {
				s.wsTransport.ExpectPeer(addr, id)
			}
		}
		return
	}
	if e, ok := s.netTransport.(network.PeerExpecter); ok {
		if sni != "" {
			e.ExpectPeerSNI(addr, id, sni)
		} else {
			e.ExpectPeer(addr, id)
		}
	}
}

// ErrIsolated: this node is isolated and dials nobody.
var ErrIsolated = errors.New("this node is isolated: it does not connect to other nodes")

// ErrSelfAddr: the address is this node's own; it is not dialed again.
var ErrSelfAddr = errors.New("this address is this node's own")

// SetNoDial stops (or allows again) every outbound connection: an isolated node only accepts.
func (s *Swarm) SetNoDial(on bool) { s.noDial.Store(on) }

// dialEarly dials addr on the transport its scheme selects: WebSocket for ws:// and wss://,
// otherwise the QUIC/TCP transport.
func (s *Swarm) dialEarly(ctx context.Context, addr string) error {
	if s.noDial.Load() {
		return ErrIsolated
	}
	if _, self := s.selfAddrs.Load(addr); self {
		return ErrSelfAddr
	}
	var err error
	if isWSAddr(addr) {
		if s.wsTransport == nil {
			return errors.New("no WebSocket transport to dial " + addr)
		}
		err = s.wsTransport.Dial(ctx, addr)
	} else {
		err = s.netTransport.DialEarly(ctx, addr)
	}
	// a stale record (an old ID of ours at our address) leads back here: remember the address
	var mismatch *network.PeerMismatchError
	if errors.As(err, &mismatch) && mismatch.Got == s.selfID {
		if _, seen := s.selfAddrs.LoadOrStore(addr, true); !seen {
			s.log().Info("Swarm: an address announced for another peer is this node's own; not dialing it again", "addr", addr)
		}
		return ErrSelfAddr
	}
	return err
}

func (s *Swarm) connect(addr string) {
	ctx, cancel := context.WithTimeout(s.ctx, time.Second*3)
	defer cancel()
	err := s.dialEarly(ctx, addr)
	if err != nil {
		s.log().Warn("Failed to connect to swarm", "addr", addr, "err", err)
		return
	}
}

// GetHistoryConnected Set to 0 to get all peers
func (s *Swarm) GetHistoryConnected(count uint) []*storage.PeerStoreEntry {
	if s.peerStore != nil {
		return s.peerStore.GetHistoryConnected(count)
	}
	return nil
}

// AddPotentialPeer dials a peer in the background if it is not already connected.
func (s *Swarm) AddPotentialPeer(id types.PeerID, addr string) {
	s.mu.Lock()
	_, connected := s.activePeers[id]
	s.mu.Unlock()

	if connected {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debug("Swarm: Attempting automatic local connection", "addr", addr)
			ctx, cancel := context.WithTimeout(s.ctx, time.Second*5)
			defer cancel()
			s.expectPeer(addr, id)
			err := s.dialEarly(ctx, addr)
			if err != nil {
				s.logger.Debug("Swarm: Failed to dial discovered peer", "addr", addr, "err", err)
			}
		}
	}()
}

// AddPotentialPeerWithCtx dials a peer and waits for the attempt to finish or ctx to end.
func (s *Swarm) AddPotentialPeerWithCtx(ctx context.Context, id types.PeerID, addr string) error {
	s.mu.Lock()
	_, connected := s.activePeers[id]
	s.mu.Unlock()

	if connected {
		return nil
	}

	slog.Debug("Swarm: Dialing peer...", "id", id, "addr", addr)
	s.expectPeer(addr, id)
	err := s.dialEarly(ctx, addr)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting for peer registration in swarm")
		case <-ticker.C:
			s.mu.RLock()
			_, active := s.activePeers[id]
			s.mu.RUnlock()
			if active {
				return nil
			}
		}
	}
}

// PingPeer measures the round trip to a peer, dialling it first if it is not connected. A
// connected peer whose connection is alive counts as alive even when it does not answer the
// ping (a node of an older version has no ping handler): the connection proves it is there.
func (s *Swarm) PingPeer(id types.PeerID, lastAddr string) (time.Duration, error) {
	start := time.Now()

	if p := s.GetPeer(id); p != nil {
		if !p.Alive() {
			return 0, errors.New("peer connection is closed")
		}
		ping := &internal_pb.MessageData{
			MessageId: types.NewMessageID(),
			OriginId:  s.selfID[:],
			Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: start.UnixNano()}},
		}
		if respType, resp, err := s.SendMessageWithRequest(id, net_types.TypePing, ping); err == nil &&
			respType == net_types.TypePong && resp.GetPong().GetNonce() == start.UnixNano() {
			return time.Since(start), nil
		}
		if p.Alive() {
			return time.Since(start), nil
		}
		return 0, errors.New("peer connection closed during ping")
	}

	if lastAddr == "" {
		return 0, errors.New("peer not connected and no address provided")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()

	s.expectPeer(lastAddr, id)
	err := s.dialEarly(ctx, lastAddr)
	if err != nil {
		return 0, fmt.Errorf("peer unreachable at %s: %w", lastAddr, err)
	}

	return time.Since(start), nil
}

// Bootstrap dials the given nodes. With obfuscation on each entry must name the node:
// "host:port/<peer id>" (network.JoinPeerAddr).
// Bootstrap dials the given nodes ("host:port/peerid"; the host may be a name, so a seed node can
// move to another IP without a new release).
func (s *Swarm) Bootstrap(addrs []string) {
	for _, entry := range addrs {
		if entry == "" {
			continue
		}
		addr, id, named := network.SplitPeerAddr(entry)
		if named && id == s.selfID {
			continue // a seed node finds itself in the built-in list
		}
		if named && s.ThisIsActivePeer(id) {
			// already connected: a second connection replaced the first, and every relay circuit
			// riding on the first (a call, a VPN through a relay, a file) died with it. A node
			// short of peers dialed its bootstrap nodes every 15 s to a few minutes, and did that.
			continue
		}

		s.wg.Add(1)
		go func(a string) {
			defer s.wg.Done()
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			// obfuscation keys are bound to the address packets come from: an IP, not a name
			a = resolveHostPort(s.ctx, a)
			if named {
				s.expectPeer(a, id)
			}
			s.connect(a)
		}(addr)
	}
}

// resolveHostPort turns "name:port" into "ip:port" (IPv4 first); an IP or an unresolvable name is
// returned as it is.
func resolveHostPort(ctx context.Context, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || net.ParseIP(host) != nil {
		return addr
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return addr
	}
	pick := ips[0].IP
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			pick = ip.IP
			break
		}
	}
	return net.JoinHostPort(pick.String(), port)
}
