package discovery

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"

	"log"
	"strings"

	"github.com/echofog/echofog-core/pkg/api/types"

	"github.com/grandcat/zeroconf"
)

const (
	ServiceInstance = "EchoFogNode"
	ServiceType     = "_echo-for-p2p._udp"
	Domain          = "local."
)

type MDNSFoundedPeer struct {
	ID    string `json:"ID"`
	Addr  string `json:"addr"`
	Alias string `json:"alias"`
}

type PeerNotifier interface {
	AddPotentialPeer(id types.PeerID, addr string)
}

type MDNSService struct {
	server *zeroconf.Server
	myID   types.PeerID
	port   int
	notif  PeerNotifier

	mu          sync.RWMutex
	onPeerFound func(id types.PeerID, addr string, alias string)

	foundedPeers map[types.PeerID]MDNSFoundedPeer
}

func NewMDNS(id types.PeerID, port int, notifier PeerNotifier) *MDNSService {
	return &MDNSService{
		myID:         id,
		port:         port,
		notif:        notifier,
		foundedPeers: make(map[types.PeerID]MDNSFoundedPeer),
	}
}

func (s *MDNSService) Start(ctx context.Context, myAlias string) error {
	idHex := hex.EncodeToString(s.myID[:])
	txtRecords := []string{fmt.Sprintf("id=%s", idHex), fmt.Sprintf("alias=%s", myAlias), "v=1.0.0"}

	server, err := zeroconf.Register(
		fmt.Sprintf("EchoFog-%s", idHex[:6]),
		ServiceType,
		Domain,
		s.port,
		txtRecords,
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to register mDNS service: %w", err)
	}
	s.server = server

	go s.browseLoop(ctx)

	return nil
}

func (s *MDNSService) browseLoop(ctx context.Context) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		log.Printf("[mDNS] Failed to initialize resolver: %v", err)
		return
	}

	entries := make(chan *zeroconf.ServiceEntry, 100)

	go func() {
		if err = resolver.Browse(ctx, ServiceType, Domain, entries); err != nil {
			log.Printf("[mDNS] Browse failed: %v", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.server.Shutdown()
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			if entry != nil {
				s.handleEntry(entry)
			}
		}
	}
}

func (s *MDNSService) handleEntry(entry *zeroconf.ServiceEntry) {

	var remoteID types.PeerID
	var remoteAlias string
	foundID := false

	for _, record := range entry.Text {
		if strings.HasPrefix(record, "id=") {
			idBytes, err := hex.DecodeString(strings.TrimPrefix(record, "id="))
			if err == nil {
				remoteID, _ = types.ToPeerID(idBytes)
				foundID = true
			}
		}
		if strings.HasPrefix(record, "alias=") {
			remoteAlias = strings.TrimPrefix(record, "alias=")
		}
	}

	if !foundID || remoteID == s.myID {
		return
	}

	bestAddr := ""
	if len(entry.AddrIPv4) > 0 {
		bestAddr = fmt.Sprintf("%s:%d", entry.AddrIPv4[0].String(), entry.Port)
	} else {
		return
	}

	if remoteAlias == "" {
		remoteAlias = fmt.Sprintf("Unknown (%x)", remoteID[:4])
	}

	log.Printf("[Discovery] Found peer %s at %s", remoteAlias, bestAddr)

	s.mu.Lock()
	s.foundedPeers[remoteID] = MDNSFoundedPeer{
		Addr:  bestAddr,
		Alias: remoteAlias,
	}
	s.mu.Unlock()

	if s.onPeerFound != nil {
		s.onPeerFound(remoteID, bestAddr, remoteAlias)
	}
}

func (s *MDNSService) SetOnPeerFound(cb func(id types.PeerID, addr string, alias string)) {
	s.onPeerFound = cb
}

func (s *MDNSService) GetFoundedPeers() []MDNSFoundedPeer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]MDNSFoundedPeer, 0, len(s.foundedPeers))

	for id, peer := range s.foundedPeers {
		hexID := hex.EncodeToString(id[:])
		peer.ID = hexID
		res = append(res, peer)
	}

	return res
}

func (s *MDNSService) ConnectToMDNSPeer(id string, addr string) error {
	decodedID, err := hex.DecodeString(id)
	if err != nil {
		return err
	}

	peerID, err := types.ToPeerID(decodedID)
	if err != nil {
		return err
	}

	s.notif.AddPotentialPeer(peerID, addr)
	return nil
}

func (s *MDNSService) Stop() {
	if s.server != nil {
		s.server.Shutdown()
	}
}
