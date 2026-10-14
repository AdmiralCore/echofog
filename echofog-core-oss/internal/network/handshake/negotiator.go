package handshake

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/network/net_types"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

type FrameReadWriter interface {
	ReadFrame() (net_types.MessageType, []byte, error)
	WriteFrame(msgType net_types.MessageType, data []byte) error
}

type NegotiatorConfig struct {
	MyNetworkID [32]byte // if array all in zero - global echo fog network
	MyVersion   uint32
	MyCaps      Capability
	RequirePoW  bool
	// AntiDDoS is shared by all handshakes of a node so that it sees the arrival rate; nil gives
	// this negotiator a counter of its own, which stays idle.
	AntiDDoS *AntiDDoS
}

type SignerGetterKey interface {
	GetPublicKey() ([]byte, error)
	SignData(data []byte) ([]byte, error)
}

type Negotiator struct {
	//privKey  types.PeerPrivateKey
	signer SignerGetterKey

	antiDDoS *AntiDDoS
	config   *NegotiatorConfig
	frame    FrameReadWriter
}

func NewNegotiator(signer SignerGetterKey, cfg *NegotiatorConfig, frame FrameReadWriter) *Negotiator {
	dd := cfg.AntiDDoS
	if dd == nil {
		dd = NewAntiDDoS()
	}
	return &Negotiator{
		signer:   signer,
		antiDDoS: dd,
		config:   cfg,
		frame:    frame,
	}
}

// MinProtocolVersion is the oldest peer protocol this node talks to: version 101 introduced
// mutual authentication bound to the transport session.
const MinProtocolVersion = 101

const (
	labelResponder = "echofog/handshake/v2/responder"
	labelInitiator = "echofog/handshake/v2/initiator"
	nonceLen       = 32
)

var (
	ErrBadHandshake    = errors.New("malformed handshake")
	ErrOldPeerProtocol = errors.New("peer runs an outdated protocol version")
)

// transcript is what each side signs: the role, the transport's channel binding (TLS exporter,
// unique to this connection, so a relay in the middle cannot forward the signature to another
// connection), the initiator's nonce and both public keys.
func transcript(label string, binding, nonce, initiatorPub, responderPub []byte) []byte {
	out := make([]byte, 0, len(label)+len(binding)+len(nonce)+64)
	out = append(out, label...)
	out = append(out, binding...)
	out = append(out, nonce...)
	out = append(out, initiatorPub...)
	return append(out, responderPub...)
}

func pubKeyOf(b []byte) (types.PeerPublicKey, error) {
	var k types.PeerPublicKey
	if len(b) != len(k) {
		return k, ErrBadHandshake
	}
	copy(k[:], b)
	return k, nil
}

// Negotiate runs the peer handshake over the frame stream. binding must be the channel binding
// of the underlying connection; both sides must pass the same value. Deadlines are the
// caller's: the stream is owned by the authenticator.
func (n *Negotiator) Negotiate(binding []byte, isInitiator bool) (*HandshakeResult, error) {
	if len(binding) < 16 {
		return nil, errors.New("handshake: missing channel binding")
	}
	if isInitiator {
		return n.runInitiatorFlow(binding)
	}
	return n.runResponderFlow(binding)
}

func (n *Negotiator) readMessage() (*internal_pb.MessageData, error) {
	_, data, err := n.frame.ReadFrame()
	if err != nil {
		return nil, err
	}
	var msg internal_pb.MessageData
	if err := proto.Unmarshal(data, &msg); err != nil {
		return nil, ErrBadHandshake
	}
	return &msg, nil
}

func (n *Negotiator) runInitiatorFlow(binding []byte) (*HandshakeResult, error) {
	myNonce := make([]byte, nonceLen)
	if _, err := rand.Read(myNonce); err != nil {
		return nil, err
	}
	myPub, err := n.signer.GetPublicKey()
	if err != nil {
		return nil, err
	}

	hello := &internal_pb.HandshakeInit{
		Nonce:        myNonce,
		Version:      n.config.MyVersion,
		NetworkId:    n.config.MyNetworkID[:],
		Capabilities: uint32(n.config.MyCaps),
		PubKey:       myPub,
	}
	if err = n.sendProto(net_types.TypeHandshake, &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_HandshakeInit{HandshakeInit: hello},
	}); err != nil {
		return nil, err
	}

	resp, err := n.readMessage()
	if err != nil {
		return nil, err
	}
	if challenge := resp.GetPuzzleChallenge(); challenge != nil {
		if challenge.Difficulty > maxPuzzleDifficulty {
			return nil, errors.New("handshake: unreasonable puzzle difficulty")
		}
		nonce := SolvePuzzle(challenge.Prefix, challenge.Difficulty)
		if err = n.sendProto(net_types.TypeHandshake, &internal_pb.MessageData{
			Payload: &internal_pb.MessageData_PuzzleSolution{PuzzleSolution: &internal_pb.PuzzleSolution{Nonce: nonce}},
		}); err != nil {
			return nil, err
		}
		if resp, err = n.readMessage(); err != nil {
			return nil, err
		}
	}

	hsResp := resp.GetHandshakeResponse()
	if hsResp == nil {
		return nil, errors.New("expected handshake response")
	}
	if string(hsResp.NetworkId) != string(n.config.MyNetworkID[:]) {
		return nil, fmt.Errorf("wrong network ID: %x", hsResp.NetworkId)
	}
	if hsResp.Version < MinProtocolVersion {
		return nil, ErrOldPeerProtocol
	}
	peerPub, err := pubKeyOf(hsResp.PubKey)
	if err != nil {
		return nil, err
	}
	if !crypto.VerifySignature(peerPub, transcript(labelResponder, binding, myNonce, myPub, peerPub[:]), hsResp.Signature) {
		return nil, errors.New("invalid signature")
	}

	// prove our own identity the same way
	mySig, err := n.signer.SignData(transcript(labelInitiator, binding, myNonce, myPub, peerPub[:]))
	if err != nil {
		return nil, err
	}
	if err = n.sendProto(net_types.TypeHandshake, &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_HandshakeResponse{HandshakeResponse: &internal_pb.HandshakeResponse{
			PubKey:    myPub,
			Signature: mySig,
		}},
	}); err != nil {
		return nil, err
	}

	return &HandshakeResult{
		PeerID:    types.PeerPubKeyToID(peerPub),
		PubKey:    peerPub,
		NetworkID: n.config.MyNetworkID,
		Caps:      Capability(hsResp.Capabilities),
		Version:   hsResp.Version,
	}, nil
}

func (n *Negotiator) runResponderFlow(binding []byte) (*HandshakeResult, error) {
	msg, err := n.readMessage()
	if err != nil {
		return nil, err
	}
	init := msg.GetHandshakeInit()
	if init == nil {
		return nil, errors.New("expected init")
	}
	peerPub, err := pubKeyOf(init.PubKey)
	if err != nil || len(init.Nonce) != nonceLen {
		return nil, ErrBadHandshake
	}
	if string(init.NetworkId) != string(n.config.MyNetworkID[:]) {
		return nil, fmt.Errorf("wrong network ID: %x", init.NetworkId)
	}
	if init.Version < MinProtocolVersion {
		return nil, ErrOldPeerProtocol
	}

	difficulty := n.antiDDoS.GetDifficulty()
	if n.config.RequirePoW && difficulty < minPoWBits {
		difficulty = minPoWBits
	}
	if difficulty > 0 {
		prefix, _ := n.IssueChallenge()
		if err = n.sendProto(net_types.TypeHandshake, &internal_pb.MessageData{
			Payload: &internal_pb.MessageData_PuzzleChallenge{
				PuzzleChallenge: &internal_pb.PuzzleChallenge{Prefix: prefix, Difficulty: difficulty},
			},
		}); err != nil {
			return nil, err
		}
		solMsg, err := n.readMessage()
		if err != nil {
			return nil, err
		}
		solution := solMsg.GetPuzzleSolution()
		if solution == nil || !VerifyPuzzle(prefix, difficulty, solution.Nonce) {
			return nil, errors.New("puzzle failed, go away bot")
		}
	}

	myPub, err := n.signer.GetPublicKey()
	if err != nil {
		return nil, err
	}
	signature, err := n.signer.SignData(transcript(labelResponder, binding, init.Nonce, peerPub[:], myPub))
	if err != nil {
		return nil, err
	}
	if err = n.sendProto(net_types.TypeHandshake, &internal_pb.MessageData{
		Payload: &internal_pb.MessageData_HandshakeResponse{HandshakeResponse: &internal_pb.HandshakeResponse{
			PubKey:       myPub,
			Signature:    signature,
			Version:      n.config.MyVersion,
			NetworkId:    n.config.MyNetworkID[:],
			Capabilities: uint32(n.config.MyCaps),
		}},
	}); err != nil {
		return nil, err
	}

	// the initiator must prove it holds the key it claimed
	proofMsg, err := n.readMessage()
	if err != nil {
		return nil, err
	}
	proof := proofMsg.GetHandshakeResponse()
	if proof == nil || string(proof.PubKey) != string(peerPub[:]) ||
		!crypto.VerifySignature(peerPub, transcript(labelInitiator, binding, init.Nonce, peerPub[:], myPub), proof.Signature) {
		return nil, errors.New("initiator failed to prove its identity")
	}

	return &HandshakeResult{
		PeerID:    types.PeerPubKeyToID(peerPub),
		PubKey:    peerPub,
		NetworkID: n.config.MyNetworkID,
		Caps:      Capability(init.Capabilities),
		Version:   init.Version,
	}, nil
}

func (n *Negotiator) IssueChallenge() (prefix []byte, difficulty uint32) {
	prefix = make([]byte, 16)
	rand.Read(prefix)

	difficulty = n.antiDDoS.GetDifficulty()
	return prefix, difficulty
}

func (n *Negotiator) VerifyChallenge(prefix []byte, difficulty uint32, nonce uint64) bool {
	return VerifyPuzzle(prefix, difficulty, nonce)
}

func (n *Negotiator) sendProto(msgType net_types.MessageType, msg *internal_pb.MessageData) error {
	data, _ := proto.Marshal(msg)
	return n.frame.WriteFrame(msgType, data)
}
