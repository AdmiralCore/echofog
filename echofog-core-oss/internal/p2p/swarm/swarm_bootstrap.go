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

// expectPeer tells the transport whose address addr is, and its camouflage SNI if one is known, so a
// dial to it uses the right obfuscation. It returns ErrNoiseDialRefused, and the dial must not be
// made, when noise is all that is left for reaching a server across a border (see noiseDialRefused).
func (s *Swarm) expectPeer(addr string, id types.PeerID) error {
	info := network.ParseAddrInfo(addr)
	if info.Server {
		s.SetPeerServer(id, true)
	}
	sni := s.GetPeerSNI(id)
	if sni == "" {
		sni = info.SNI
		if sni != "" {
			s.SetPeerSNI(id, sni)
		}
	}
	if sni == "" && s.noiseDialRefused(id, addr) {
		return ErrNoiseDialRefused
	}
	addr = info.Addr
	if isWSAddr(addr) {
		if s.wsTransport != nil {
			if sni != "" {
				s.wsTransport.ExpectPeerSNI(addr, id, sni)
			} else {
				s.wsTransport.ExpectPeer(addr, id)
			}
		}
		return nil
	}
	if e, ok := s.netTransport.(network.PeerExpecter); ok {
		if sni != "" {
			e.ExpectPeerSNI(addr, id, sni)
		} else {
			e.ExpectPeer(addr, id)
		}
	}
	return nil
}

// ExpectPeerAddr registers, with the transport, that the node at addr is peer id and (if known) its
// camouflage SNI, so a dial to addr uses the right obfuscation: REALITY for a camouflage node across
// a censoring border, noise otherwise. The exit gateway calls it before dialing a camouflage exit so
// the tunnel runs over a REALITY connection rather than a noise connection reused from the mesh.
func (s *Swarm) ExpectPeerAddr(addr string, id types.PeerID) {
	_ = s.expectPeer(addr, id)
}

// IsCamouflagePeer reports whether the swarm knows a camouflage SNI for id (a node that mimics a
// cover site). The exit gateway uses it to force REALITY on an exit tunnel to such a node.
func (s *Swarm) IsCamouflagePeer(id types.PeerID) bool {
	return s.GetPeerSNI(id) != ""
}

// EnsureExitReality makes sure the connection to a camouflage exit (a node with a known SNI) is a
// REALITY connection, replacing any noise connection left to it by the mesh bootstrap or by an
// earlier market/account probe (those are dialed by peer ID before the SNI is registered, and a noise
// connection to a camouflage node does not serve the tunnel: the exit goes silent and the client
// switches away). Call it once at VPN start, after the SNI is registered, before opening exit
// streams. It always dials a fresh connection, even when a camouflaged one is up: that one may be
// stale (a QUIC link whose UDP was cut since looks alive until its idle timeout), and the dial races
// QUIC against TCP, so a network that now blocks UDP gets a working TCP link at once. It returns once
// a camouflaged link is registered or ctx ends. A no-op for a peer with no known SNI (an ordinary
// exit keeps its noise connection). OpenExitStream makes sure of a camouflaged link before every
// tunnel stream, so one that dies later is redialed rather than failed over.
func (s *Swarm) EnsureExitReality(ctx context.Context, exit types.PeerID) {
	if s.GetPeerSNI(exit) == "" {
		return
	}
	if p := s.GetPeer(exit); p != nil {
		addr := p.Addr()
		if addr == "" {
			addr = p.RemoteAddr()
		}
		if addr != "" {
			s.dialCamouflaged(ctx, exit, addr)
		}
	}
	if err := s.ensureCamouflagedLink(ctx, exit); err != nil {
		s.log().Warn("Swarm: no camouflaged link to the camouflage exit yet", "peer", exit.String()[:12], "err", err)
	}
}

// ErrIsolated: this node is isolated and dials nobody.
var ErrIsolated = errors.New("this node is isolated: it does not connect to other nodes")

// ErrSelfAddr: the address is this node's own; it is not dialed again.
var ErrSelfAddr = errors.New("this address is this node's own")

// SetNoDial stops (or allows again) every outbound connection: an isolated node only accepts.
func (s *Swarm) SetNoDial(on bool) { s.noDial.Store(on) }

// DialEarly dials addr on the transport its scheme selects.
func (s *Swarm) DialEarly(ctx context.Context, addr string) error {
	return s.dialEarly(ctx, addr)
}

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
			if err := s.expectPeer(addr, id); err != nil {
				s.logger.Debug("Swarm: not dialing discovered peer", "addr", addr, "err", err)
				return
			}
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
	if err := s.expectPeer(addr, id); err != nil {
		return err
	}
	// an annotated address ("host:port?sni=...&srv=1", as mailbox hints carry) is dialed at its
	// host:port; expectPeer took what the annotations say
	err := s.dialEarly(ctx, network.ParseAddrInfo(addr).Addr)
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

// Answers reports whether a connected peer answers a ping (within the request timeout, 5s). Unlike
// PingPeer it does not take an open connection for an answer: it is how a link that died without
// either side noticing is told from one that is merely quiet.
func (s *Swarm) Answers(id types.PeerID) bool {
	p := s.GetPeer(id)
	if p == nil || !p.Alive() {
		return false
	}
	nonce := time.Now().UnixNano()
	ping := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  s.selfID[:],
		Payload:   &internal_pb.MessageData_Ping{Ping: &internal_pb.Ping{Nonce: nonce}},
	}
	respType, resp, err := s.SendMessageWithRequest(id, net_types.TypePing, ping)
	if err != nil || respType != net_types.TypePong || resp.GetPong().GetNonce() != nonce {
		return false
	}
	p.heard()
	return true
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

	if err := s.expectPeer(lastAddr, id); err != nil {
		return 0, err
	}
	err := s.dialEarly(ctx, lastAddr)
	if err != nil {
		return 0, fmt.Errorf("peer unreachable at %s: %w", lastAddr, err)
	}

	return time.Since(start), nil
}

// Bootstrap dials the given nodes. With obfuscation on each entry must name the node:
// "host:port/<peer id>" (network.JoinPeerAddr).
// Bootstrap dials the given nodes ("host:port/peerid", optionally "?sni=" for a camouflage node, as
// echofog-node prints its join address; the host may be a name, so a seed node can move to another
// IP without a new release).
func (s *Swarm) Bootstrap(addrs []string) {
	for _, entry := range addrs {
		if entry == "" {
			continue
		}
		info := network.ParseAddrInfo(entry)
		addr, id, sni, named := info.Addr, info.ID, info.SNI, info.Named
		if named && id == s.selfID {
			continue // a seed node finds itself in the built-in list
		}
		if named && sni != "" {
			s.SetPeerSNI(id, sni) // as if the DHT had announced it: dials to it use REALITY
		}
		if named && info.Server {
			s.SetPeerServer(id, true)
		}
		if named {
			s.MarkExplicitPeer(id)
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
				if err := s.expectPeer(a, id); err != nil {
					s.log().Warn("Swarm: not dialing a bootstrap node", "addr", a, "err", err)
					return
				}
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
