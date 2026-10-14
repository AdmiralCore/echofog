package swarm

import (
	"time"

	"github.com/echofog/echofog-core/internal/crypto"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"

	"google.golang.org/protobuf/proto"
)

// SignMessageData wraps a message in an envelope signed with this node's identity key.
func (s *Swarm) SignMessageData(mesData *internal_pb.MessageData) (*internal_pb.Envelope, error) {
	if len(mesData.MessageId) == 0 {
		mesData.MessageId = types.NewMessageID()[:]
	}

	if len(mesData.OriginId) == 0 {
		mesData.OriginId = s.selfID[:]
	}

	if mesData.Timestamp == 0 {
		mesData.Timestamp = uint64(time.Now().Unix())
	}

	if mesData.HopLimit == 0 {
		mesData.HopLimit = types.HOPLIMIT
	}

	Data, err := proto.Marshal(mesData)
	if err != nil {
		return nil, err
	}

	var pubKey []byte
	pubKey, err = s.identity.GetPublicKey()
	if err != nil {
		return nil, err
	}

	var signature []byte
	signature, err = s.identity.SignData(Data)
	if err != nil {
		return nil, err
	}

	return &internal_pb.Envelope{
		Data:      Data,
		Signature: signature,
		PubKey:    pubKey[:],
	}, nil
}

// CheckSign verifies that an envelope's signature matches its declared sender.
func (s *Swarm) CheckSign(envelope *internal_pb.Envelope) bool {
	if len(envelope.PubKey) != 32 {
		return false
	}
	pubKey := types.PeerPublicKey(envelope.PubKey)
	return crypto.VerifySignature(pubKey, envelope.Data, envelope.Signature)
}

// CreateSignature signs arbitrary bytes with this node's identity key.
func (s *Swarm) CreateSignature(dataForSign []byte) ([]byte, error) {
	return s.identity.SignData(dataForSign)
}
