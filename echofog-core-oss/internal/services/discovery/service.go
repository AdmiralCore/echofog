package discovery

import (
	"context"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/gossip"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	guuid "github.com/google/uuid"
)

type DiscoveryService struct {
	storage  storage.Storage
	gsp      *gossip.Manager
	swarm    *swarm.Swarm
	getter   *peerGetter
	giver    *peerGiver
	myPubKey types.PeerPublicKey
}

func NewDiscoveryService(storage storage.Storage, gsp *gossip.Manager, swarm *swarm.Swarm, myPubKey types.PeerPublicKey) *DiscoveryService {
	ds := &DiscoveryService{
		storage:  storage,
		gsp:      gsp,
		swarm:    swarm,
		myPubKey: myPubKey,
	}
	var getter = newPeerGetter(ds)
	giver := newPeerGiver(ds)
	ds.getter = getter
	ds.giver = giver

	return ds
}

func (d *DiscoveryService) Name() string {
	return "DiscoveryService"
}

func (d *DiscoveryService) Start(ctx context.Context) error {
	return nil
}

func (d *DiscoveryService) Stop() error {
	return nil
}

func (d *DiscoveryService) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	switch msg.Payload.(type) {
	case *internal_pb.MessageData_PeerRes:
		d.getter.Handle(msg, peerID)
	case *internal_pb.MessageData_PeerReq:
		d.giver.Handle(msg, peerID)
	}
}

func (d *DiscoveryService) GetSubscribedTypes() []interface{} {
	return []interface{}{
		(*internal_pb.MessageData_PeerReq)(nil),
		(*internal_pb.MessageData_PeerRes)(nil),
	}
}

type peerGetter struct {
	service *DiscoveryService
}

func newPeerGetter(service *DiscoveryService) *peerGetter {
	return &peerGetter{service: service}
}

func (g *peerGetter) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	msgPeer, ok := msg.Payload.(*internal_pb.MessageData_PeerRes)
	if !ok || msgPeer.PeerRes == nil {
		return
	}

	peerStore := g.service.swarm.GetPeerStore()
	for _, p := range msgPeer.PeerRes.Peers {
		if len(p.PubKey) != 32 || p.Address == "" {
			continue
		}
		var pubKey types.PeerPublicKey
		copy(pubKey[:], p.PubKey)

		discoveredID := types.PeerPubKeyToID(pubKey)
		if discoveredID == g.service.swarm.GetSelfID() {
			continue
		}

		if peerStore != nil {
			peerStore.SavePeerAsync(pubKey, p.Address, 50)
		}
	}
}

type peerGiver struct {
	service *DiscoveryService
}

func newPeerGiver(service *DiscoveryService) *peerGiver {
	return &peerGiver{service: service}
}

func (g *peerGiver) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	msgPeer, ok := msg.Payload.(*internal_pb.MessageData_PeerReq)
	if !ok {
		return
	}

	peers := g.service.swarm.GetMyRandomPeers(uint(msgPeer.PeerReq.Count))

	peersInfo := make([]*internal_pb.PeerInfo, 0, len(peers))
	for _, p := range peers {
		if p.Addr() == "" {
			continue // reachable only through its own connection to us
		}
		pubKey := p.PubKey()
		peersInfo = append(peersInfo, &internal_pb.PeerInfo{
			PubKey:  pubKey[:],
			Address: p.Addr(),
		})
	}

	mesID := guuid.New()
	myID := g.service.swarm.GetSelfID()
	peerResponse := &internal_pb.MessageData{
		MessageId: mesID[:],
		OriginId:  myID[:],
		TargetId:  msg.GetOriginId(),
		Timestamp: uint64(time.Now().UnixNano()),
		HopLimit:  20,
		Payload: &internal_pb.MessageData_PeerRes{
			PeerRes: &internal_pb.PeerResponse{
				Peers: peersInfo,
			},
		},
	}

	originID := types.PeerID(msg.OriginId)

	if g.service.swarm.ThisIsActivePeer(originID) {
		err := g.service.swarm.SendDataForPeer(originID, net_types.TypeGetPeerResponse, peerResponse)
		if err == nil {
			return
		}
	}

	g.service.gsp.Broadcast(net_types.TypeGetPeerResponse, peerResponse)
}
