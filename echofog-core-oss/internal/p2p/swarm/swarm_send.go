package swarm

import (
	"context"
	"errors"

	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

// SendDataForPeer signs a message and sends it to a connected peer over a reliable stream.
func (s *Swarm) SendDataForPeer(peerID types.PeerID, msgType net_types.MessageType, data *internal_pb.MessageData) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}

	env, err := s.SignMessageData(data)
	if err != nil {
		return err
	}

	return peer.Send(msgType, env)
}

// SendRawFrame sends raw binary bytes directly over the peer transport without Protobuf Envelope overhead (Fast Path).
func (s *Swarm) SendRawFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}
	return peer.SendRaw(msgType, data)
}

// SendUnreliableFrame sends a loss-tolerant frame (real-time media) without retransmission.
func (s *Swarm) SendUnreliableFrame(peerID types.PeerID, msgType net_types.MessageType, data []byte) error {
	peer := s.GetPeer(peerID)
	if peer == nil {
		return errors.New("this peer is not connected")
	}
	return peer.SendRawUnreliable(msgType, data)
}

// OpenNewStreams opens count fresh streams to a peer, e.g. for a call's media fan-out.
func (s *Swarm) OpenNewStreams(ctx context.Context, peerID types.PeerID, count uint32) ([]*network.Stream, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if !ok {
		return nil, errors.New("this peer is inactive")
	}
	var (
		mu                = sync.Mutex{}
		wg                = sync.WaitGroup{}
		streams           = make([]*network.Stream, 0, count)
		openedCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	)
	defer cancel()

	wg.Add(int(count))
	for range count {
		go func() {
			defer wg.Done()
			stream, err := peer.transport.OpenBidirectionalStream(openedCtx)
			if err != nil {
				return
			}
			mu.Lock()
			streams = append(streams, stream)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(streams) == 0 {
		return nil, errors.New("failed to open any streams")
	}

	return streams, nil
}

// SendDataWithRequest sends a raw request to a peer and waits for its reply.
func (s *Swarm) SendDataWithRequest(peerID types.PeerID, msgType net_types.MessageType, data []byte) (net_types.MessageType, []byte, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()
	if ok {
		return peer.transport.SendDataWithRequest(msgType, data)
	}
	return 0, nil, errors.New("peer not connected")
}

// SendMessageWithRequest signs a message, sends it as a request, and returns the peer's reply.
func (s *Swarm) SendMessageWithRequest(peerID types.PeerID, msgType net_types.MessageType, msg *internal_pb.MessageData) (net_types.MessageType, *internal_pb.MessageData, error) {
	s.mu.RLock()
	peer, ok := s.activePeers[peerID]
	s.mu.RUnlock()

	if ok {
		env, err := s.SignMessageData(msg)
		if err != nil {
			return 0, nil, err
		}

		var envBytes []byte
		envBytes, err = proto.Marshal(env)
		if err != nil {
			return 0, nil, err
		}

		var respType net_types.MessageType
		var respMsg []byte
		respType, respMsg, err = peer.transport.SendDataWithRequest(msgType, envBytes)
		if err != nil {
			return 0, nil, err
		}

		var respEnvelope internal_pb.Envelope
		err = proto.Unmarshal(respMsg, &respEnvelope)
		if err != nil {
			return 0, nil, err
		}

		if !s.CheckSign(&respEnvelope) {
			return 0, nil, errors.New("invalid signature")
		}

		var msgResp internal_pb.MessageData
		err = proto.Unmarshal(respEnvelope.Data, &msgResp)
		if err != nil {
			return 0, nil, err
		}

		return respType, &msgResp, nil
	}
	return 0, nil, errors.New("peer not connected")
}
