package messenger

import (
	"encoding/hex"
	"io"
	"log/slog"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"google.golang.org/protobuf/proto"
)

// A session reset travels unencrypted in its sender's signed envelope, so every relay that
// carried one can hand it over again. It is acted on once, by the node it names, while fresh: a
// replay later, to the same node or to another, and one from a blocked peer change nothing.
// (Before, every copy wiped the session with its sender.)
func TestResetIsNotReplayable(t *testing.T) {
	db := testutil.NewTestingBadgerStorage(t)
	t.Cleanup(db.CloseAndClear)
	var me, other, alice, mallory types.PeerID
	me[0], other[0], alice[0], mallory[0] = 1, 2, 3, 4
	resets := map[types.PeerID]int{}
	s := &MessageBroker{kv: db.St, log: slog.New(slog.NewTextHandler(io.Discard, nil)), myIDHexed: hex.EncodeToString(me[:]),
		resetFn: func(p types.PeerID) { resets[p]++ }}
	s.SetFilter(func(origin types.PeerID) Verdict {
		if origin == mallory {
			return Drop
		}
		return Accept
	})
	// what a node of either version sends: a plain-tier chat message whose envelope it signed
	send := func(origin, target types.PeerID, at time.Time, id string) {
		inner, _ := proto.Marshal(&PersonalMessageProto{Id: id, SenderId: hex.EncodeToString(origin[:]), Text: resetPrefix})
		s.Handle(&internal_pb.MessageData{OriginId: origin[:], TargetId: target[:], Timestamp: uint64(at.Unix()),
			Payload: &internal_pb.MessageData_ChatMessage{ChatMessage: &internal_pb.ChatMessage{EncryptedPayload: append([]byte{0}, inner...)}}}, origin)
	}
	now := time.Now()
	steps := []struct {
		name   string
		origin types.PeerID
		target types.PeerID
		at     time.Time
		id     string
		want   int // alice's resets so far
	}{
		{"fresh, for us", alice, me, now, "r1", 1},
		{"the same again (replayed)", alice, me, now, "r1", 1},
		{"a later one", alice, me, now.Add(-time.Hour), "r2", 2},
		{"for another node", alice, other, now, "r3", 2},
		{"older than the window", alice, me, now.Add(-controlWindow - time.Minute), "r4", 2},
		{"from the future", alice, me, now.Add(controlWindow + time.Minute), "r5", 2},
		{"no id", alice, me, now, "", 2},
	}
	for _, st := range steps {
		send(st.origin, st.target, st.at, st.id)
		if resets[alice] != st.want {
			t.Fatalf("%s: %d resets, want %d", st.name, resets[alice], st.want)
		}
	}
	send(mallory, me, now, "m1")
	if resets[mallory] != 0 {
		t.Fatal("a blocked peer reset the session")
	}
}
