package messenger

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/echofog/echofog-core/internal/crypto/x3dh"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// A replayed ratchet message (its key used up, its header not among the recent ones) is dropped
// and the session stays; only a message that does not belong to the session resets it.
func TestReplayedMessageLeavesTheSession(t *testing.T) {
	var broken []types.PeerID
	s := &MessageBroker{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		brokenFn: func(p types.PeerID) { broken = append(broken, p) }}
	var peer types.PeerID
	peer[0] = 7
	h := &internal_pb.RatchetHeader{RatchetKey: make([]byte, 32), N: 3}

	s.undecryptable(peer, h, fmt.Errorf("decrypt failed: %w", x3dh.ErrOldMessage))
	if len(broken) != 0 {
		t.Fatal("a replayed message reset the session")
	}
	s.undecryptable(peer, h, fmt.Errorf("decrypt failed: %w", errors.New("chacha20poly1305: message authentication failed")))
	if len(broken) != 1 {
		t.Fatal("a message of another session did not reset ours")
	}
}
