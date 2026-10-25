package messenger

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/testutil"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKnockTextRoundTrip(t *testing.T) {
	card := []byte(`{"peer":"x","bundle":"y"}`)
	flag, got, ok := parseKnock(knockText(knockAsk, card))
	require.True(t, ok)
	assert.Equal(t, knockAsk, flag)
	assert.Equal(t, card, got)

	flag, _, ok = parseKnock(knockText(knockAnswer, card))
	require.True(t, ok)
	assert.Equal(t, knockAnswer, flag)

	for _, bad := range []string{"hello", knockPrefix, knockPrefix + "q:", knockPrefix + "z:" + "YWJj", knockPrefix + "q:not base64!"} {
		_, _, ok := parseKnock(bad)
		assert.False(t, ok, bad)
	}
}

// What a message waits for follows the outbox: keys until it is encrypted, a way to the peer until
// somebody took it, a receipt after that; the receipt removes it and is reported.
func TestPendingStatesFollowTheOutbox(t *testing.T) {
	st := testutil.NewTestingBadgerStorage(t)
	defer st.CloseAndClear()
	s := &MessageBroker{kv: st.St, log: slog.Default()}
	peer := types.PeerID{7}

	var mu sync.Mutex
	var events []MessageState
	s.OnMessageState(func(ev MessageState) { mu.Lock(); events = append(events, ev); mu.Unlock() })

	require.NoError(t, s.trackIntent(peer, "keys", []byte("hi")))
	enc := &internal_pb.MessageData{TargetId: peer[:]} // something encrypted, as far as the outbox cares
	require.NoError(t, s.track(peer, "offline", enc, []byte("hi"), false))
	require.NoError(t, s.track(peer, "sent", enc, []byte("hi"), true))
	assert.Equal(t, map[string]string{"keys": StateKeys, "offline": StateOffline, "sent": StateSent}, s.PendingStates(peer))

	s.confirm(peer, "sent")
	assert.Equal(t, map[string]string{"keys": StateKeys, "offline": StateOffline}, s.PendingStates(peer))
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, MessageState{Target: peer, ID: "sent", State: StateDelivered}, events[0])

	// an ask goes once per pause per peer
	assert.True(t, s.knockDue(peer))
	assert.False(t, s.knockDue(peer))
	assert.True(t, s.knockDue(types.PeerID{8}))
}
