package gossip

import (
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestSeenCacheIsBounded(t *testing.T) {
	g := NewManager(nil)
	// hop limit 1: each message is marked seen and goes nowhere, so no swarm is needed
	first := types.NewMessageID()
	g.Broadcast(0, &internal_pb.MessageData{MessageId: first, HopLimit: 1})
	if !g.seenCache.Exists(types.MessageID(first)) {
		t.Fatal("a broadcast message is remembered")
	}
	for i := 0; i < seenCacheSize+5000; i++ {
		id := types.NewMessageID()
		g.Broadcast(0, &internal_pb.MessageData{MessageId: id, HopLimit: 1})
	}
	if n := g.seenCache.Len(); n > seenCacheSize {
		t.Fatalf("seen cache holds %d IDs, more than its bound %d", n, seenCacheSize)
	}
	if g.seenCache.Exists(types.MessageID(first)) {
		t.Fatal("the oldest IDs make room for new ones")
	}
}
