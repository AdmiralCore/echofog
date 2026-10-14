package dht

import (
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

func (d *EchoFogDHT) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	slog.Info("handle message", "peerID", hex.EncodeToString(peerID[:]))

	switch msg.Payload.(type) {
	case *internal_pb.MessageData_FindNodeReq:
		d.HandleFindNode(msg, peerID)
	case *internal_pb.MessageData_FindNodeRes:
		d.HandleFindNodeResponse(msg)
	case *internal_pb.MessageData_StoreReq:
		d.HandleStore(msg, peerID)
	case *internal_pb.MessageData_FindValueReq:
		d.HandleFindValue(msg, peerID)
	case *internal_pb.MessageData_FindValueRes:
		d.HandleFindValueResponse(msg)
	}
}

func (d *EchoFogDHT) HandleFindNode(msg *internal_pb.MessageData, peerID types.PeerID) {
	req := msg.GetFindNodeReq()
	if req == nil {
		return
	}

	var targetID types.PeerID
	copy(targetID[:], req.TargetId)

	p := d.swarm.GetPeer(peerID)
	if p != nil && p.Addr() != "" {
		d.routingTable.AddOrUpdatePeer(p.PubKey(), p.Addr())
	}

	closestPeers := d.routingTable.GetClosestPeers(targetID, 20)

	peerInfos := make([]*internal_pb.PeerInfo, 0, len(closestPeers))
	for _, p := range closestPeers {
		peerInfos = append(peerInfos, &internal_pb.PeerInfo{
			PubKey:  p.PubKey[:],
			Address: p.Addr,
		})
	}

	resMsg := &internal_pb.MessageData{
		MessageId: msg.MessageId,
		OriginId:  d.routingTable.myID[:],
		TargetId:  peerID[:],
		Payload: &internal_pb.MessageData_FindNodeRes{
			FindNodeRes: &internal_pb.FindNodeResponse{
				ClosestPeers: peerInfos,
			},
		},
	}

	slog.Info("[STEP 3: OUT] Sending FindNodeRes", "to", peerID, "foundPeers", len(peerInfos))

	err := d.swarm.SendDataForPeer(peerID, net_types.TypeFindNodeRes, resMsg)
	if err != nil {
		slog.Warn("DHT: Failed to send FindNodeRes", "peer", peerID, "err", err)
	}
}

func (d *EchoFogDHT) HandleFindNodeResponse(msg *internal_pb.MessageData) {
	res := msg.GetFindNodeRes()
	if res == nil {
		return
	}

	msgIDStr := string(msg.MessageId)

	_, ok := d.pending.Load(msgIDStr)
	slog.Info("[STEP 4: IN] Handling FindNodeRes", "msgID", hex.EncodeToString(msg.MessageId), "foundInPending", ok)

	if chPtr, ok := d.pending.LoadAndDelete(msgIDStr); ok {
		ch := chPtr.(chan *internal_pb.FindNodeResponse)
		select {
		case ch <- res:
		default:
		}
	}
}

func (d *EchoFogDHT) HandleStore(msg *internal_pb.MessageData, peerID types.PeerID) {
	req := msg.GetStoreReq()
	if req == nil || req.Record == nil {
		return
	}

	if err := CheckRecord(req.Record, nil); err != nil {
		slog.Warn("DHT: Rejected fake or malicious record", "from", peerID, "key", fmt.Sprintf("%.24q", req.Record.Key), "err", err)
		d.emit("security:poisoning_detected", map[string]any{"peer": peerID.String(), "reason": err.Error()})
		return
	}

	if d.countRecordFromPeer(peerID) > 1000 {
		slog.Info("Spam from peer", "from", peerID)
		return
	}

	if held, ok := d.localRecord(req.Record.Key); ok && !newerThan(req.Record, held) {
		slog.Debug("DHT: kept the newer record", "from", peerID)
		return
	}

	recBytes, err := proto.Marshal(req.Record)
	if err == nil {
		err = d.swarm.GetStorage().Set(RecordKey(req.Record.Key), recBytes)
	}
	if err != nil {
		slog.Error("DHT: Failed to save record", "err", err)
	}
}

func (d *EchoFogDHT) HandleFindValue(msg *internal_pb.MessageData, peerID types.PeerID) {
	req := msg.GetFindValueReq()
	if req == nil {
		return
	}

	resPayload := &internal_pb.FindValueRes{}

	// only DHT records are given out, never anything else of our database
	if rec, ok := d.localRecord(req.TargetKey); ok {
		resPayload.Record = rec
	}

	if resPayload.Record == nil {
		targetID, _ := types.ToPeerID(req.TargetKey)
		closestPeers := d.routingTable.GetClosestPeers(targetID, 20)

		peerInfos := make([]*internal_pb.PeerInfo, 0, len(closestPeers))
		for _, p := range closestPeers {
			peerInfos = append(peerInfos, &internal_pb.PeerInfo{
				PubKey:  p.PubKey[:],
				Address: p.Addr,
			})
		}
		resPayload.ClosestPeers = peerInfos
	}

	resMsg := &internal_pb.MessageData{
		MessageId: msg.MessageId,
		TargetId:  peerID[:],
		Payload: &internal_pb.MessageData_FindValueRes{
			FindValueRes: resPayload,
		},
	}

	err := d.swarm.SendDataForPeer(peerID, net_types.TypeFindValueRes, resMsg)
	if err != nil {
		slog.Error("DHT: Failed to send FindValueRes", "peer", peerID, "err", err)
	}
}

func (d *EchoFogDHT) HandleFindValueResponse(msg *internal_pb.MessageData) {
	res := msg.GetFindValueRes()
	if res == nil {
		return
	}

	msgIDStr := string(msg.MessageId)
	if chPtr, ok := d.pending.LoadAndDelete(msgIDStr); ok {
		ch := chPtr.(chan *internal_pb.FindValueRes)
		select {
		case ch <- res:
		default:
		}
	}
}

func (d *EchoFogDHT) GetSubscribedTypes() []interface{} {
	return []interface{}{
		(*internal_pb.MessageData_FindNodeReq)(nil),
		(*internal_pb.MessageData_FindNodeRes)(nil),
		(*internal_pb.MessageData_StoreReq)(nil),
		(*internal_pb.MessageData_FindValueReq)(nil),
		(*internal_pb.MessageData_FindValueRes)(nil),
	}
}
