package messenger

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// outboxBroker is a broker with nothing but its outbox: deliver stands in for the swarm.
func outboxBroker(t *testing.T, deliver func(types.PeerID, *internal_pb.MessageData) (bool, bool)) *MessageBroker {
	t.Helper()
	db := testutil.NewTestingBadgerStorage(t)
	t.Cleanup(db.CloseAndClear)
	return &MessageBroker{kv: db.St, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deliverFn: deliver}
}

func outboxItemOf(t *testing.T, s *MessageBroker, target types.PeerID) (key []byte, it outboxItem) {
	t.Helper()
	entries := s.outboxEntries(outboxFolder(s.kv, target))
	if len(entries) != 1 {
		t.Fatalf("%d items wait for the peer, want 1", len(entries))
	}
	if err := json.Unmarshal(entries[0].Value, &it); err != nil {
		t.Fatal(err)
	}
	return entries[0].Key, it
}
