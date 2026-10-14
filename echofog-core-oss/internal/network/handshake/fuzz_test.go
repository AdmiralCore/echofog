package handshake

import (
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"google.golang.org/protobuf/proto"
)

// FuzzResponder feeds up to three arbitrary frames to the side that accepts connections: it must
// answer with an error, never panic, never wait forever, and never authenticate garbage.
func FuzzResponder(f *testing.F) {
	init, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_HandshakeInit{HandshakeInit: &internal_pb.HandshakeInit{
		PubKey: make([]byte, 32), Nonce: make([]byte, nonceLen), Version: MinProtocolVersion, NetworkId: make([]byte, 32),
	}}})
	puzzle, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_PuzzleSolution{PuzzleSolution: &internal_pb.PuzzleSolution{Nonce: 1}}})
	f.Add(init, puzzle, []byte{})
	f.Add([]byte{}, []byte{}, []byte{})
	f.Add([]byte{0xff, 0xff}, init, init)
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		in := make(chan []byte, 4)
		out := make(chan []byte, 16)
		for _, fr := range [][]byte{a, b, c} {
			in <- fr
		}
		close(in) // after the frames: end of stream, so nothing waits for a timeout
		n := NewNegotiator(newSigner(t), cfg(), chanFrames{in: in, out: out})
		res, err := n.Negotiate(binding(1), false)
		if err == nil && res == nil {
			t.Fatal("no error and no result")
		}
		if err == nil {
			t.Fatalf("garbage authenticated as peer %x", res.PubKey)
		}
	})
}

// FuzzInitiator does the same for the side that connects: whatever the server answers.
func FuzzInitiator(f *testing.F) {
	challenge, _ := proto.Marshal(&internal_pb.MessageData{Payload: &internal_pb.MessageData_PuzzleChallenge{PuzzleChallenge: &internal_pb.PuzzleChallenge{Prefix: []byte{1, 2}, Difficulty: 4}}})
	f.Add(challenge, []byte{})
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, a, b []byte) {
		in := make(chan []byte, 4)
		out := make(chan []byte, 16)
		in <- a
		in <- b
		close(in)
		n := NewNegotiator(newSigner(t), cfg(), chanFrames{in: in, out: out})
		if res, err := n.Negotiate(binding(1), true); err == nil {
			t.Fatalf("garbage authenticated as peer %x", res.PubKey)
		}
	})
}
