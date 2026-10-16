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

	"google.golang.org/protobuf/proto"
)

func (s *Swarm) findAndConnectToPeers() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		history := s.GetHistoryConnected(0)
		for _, peer := range history {
			select {
			case <-s.ctx.Done():
				return
			default:
			}

			pubKey, _ := types.ToPeerPubKey(peer.PubKey)
			peerID := types.PeerPubKeyToID(pubKey)

			s.mu.RLock()
			if len(s.activePeers) > s.cfg.Network.MaxConnections {
				s.mu.RUnlock()
				break
			}
			_, ok := s.activePeers[peerID]
			s.mu.RUnlock()
			if ok {
				continue
			}

			s.wg.Add(1)
			go func(addr string) {
				defer s.wg.Done()
				select {
				case <-s.ctx.Done():
					return
				default:
					s.expectPeer(addr, peerID)
					s.connect(addr)
				}
			}(peer.GetLastKnownAddr())
		}
	}()
}

// expectPeer tells the transport whose address addr is: with obfuscation on, the packets to a node
// are keyed by its peer ID, so every dial has to name it.
// WSTransport is the optional WebSocket transport, used to reach peers at ws:// or wss://
// addresses (for going behind a CDN).
type WSTransport interface {
	Dial(ctx context.Context, url string) error
	ExpectPeer(addr string, id types.PeerID)
	ConnChan() <-chan network.NewConnEvent
}

// SetWSTransport attaches a WebSocket transport: its inbound peers register with the swarm, and
// addresses starting with ws:// or wss:// are dialed through it.
func (s *Swarm) SetWSTransport(w WSTransport) {
	s.wsTransport = w
	s.wg.Add(1)
	go s.registrationLoop(w.ConnChan())
}

func isWSAddr(addr string) bool {
	return strings.HasPrefix(addr, "ws://") || strings.HasPrefix(addr, "wss://")
}

func (s *Swarm) expectPeer(addr string, id types.PeerID) {
	if isWSAddr(addr) {
		if s.wsTransport != nil {
			s.wsTransport.ExpectPeer(addr, id)
		}
		return
	}
	if e, ok := s.netTransport.(network.PeerExpecter); ok {
		e.ExpectPeer(addr, id)
	}
}

// dialEarly dials addr on the transport its scheme selects: WebSocket for ws:// and wss://,
// otherwise the QUIC/TCP transport.
func (s *Swarm) dialEarly(ctx context.Context, addr string) error {
	if isWSAddr(addr) {
		if s.wsTransport == nil {
			return errors.New("no WebSocket transport to dial " + addr)
		}
		return s.wsTransport.Dial(ctx, addr)
	}
	return s.netTransport.DialEarly(ctx, addr)
}

func (s *Swarm) connect(addr string) {
	ctx, cancel := context.WithTimeout(s.ctx, time.Second*3)
	defer cancel()
	err := s.dialEarly(ctx, addr)
	if err != nil {
		s.logger.Error("Failed to connect to swarm", "err: ", err)
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
			s.logger.Info("Swarm: Attempting automatic local connection", "addr", addr)
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

	slog.Info("Swarm: Dialing peer...", "id", id, "addr", addr)
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

// PingPeer measures the round trip to a peer, dialling it first if it is not connected.
func (s *Swarm) PingPeer(id types.PeerID, lastAddr string) (time.Duration, error) {
	s.mu.RLock()
	_, connected := s.activePeers[id]
	s.mu.RUnlock()

	start := time.Now()

	if connected {
		pingData := &internal_pb.MessageData{
			MessageId: types.NewMessageID(),
			OriginId:  s.selfID[:],
			Payload: &internal_pb.MessageData_Ping{
				Ping: &internal_pb.Ping{Nonce: start.UnixNano()},
			},
		}

		env, err := s.SignMessageData(pingData)
		if err != nil {
			return 0, err
		}

		envBytes, err := proto.Marshal(env)
		if err != nil {
			return 0, err
		}

		_, _, err = s.SendDataWithRequest(id, net_types.TypePing, envBytes)
		if err != nil {
			return 0, err
		}
		return time.Since(start), nil
	}

	if lastAddr == "" {
		return 0, errors.New("peer not connected and no address provided")
	}

	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()

	s.expectPeer(lastAddr, id)
	err := s.netTransport.DialEarly(ctx, lastAddr)
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

// Rejoin is for a node that has lost every peer (it started while offline, or its network was
// cut): it dials the bootstrap nodes and the peers it was connected to before. It does nothing
// while any peer is connected. Returns whether there was anybody to dial.
func (s *Swarm) Rejoin(bootstrap []string) bool {
	if len(s.GetAllPeers()) > 0 {
		return false
	}
	s.Bootstrap(bootstrap)
	tried := len(bootstrap) > 0
	if s.peerStore != nil {
		for _, e := range s.peerStore.GetHistoryConnected(16) {
			pub, err := types.ToPeerPubKey(e.GetPubKey())
			if err != nil || e.GetLastKnownAddr() == "" {
				continue
			}
			s.AddPotentialPeer(types.PeerPubKeyToID(pub), e.GetLastKnownAddr())
			tried = true
		}
	}
	return tried
}
