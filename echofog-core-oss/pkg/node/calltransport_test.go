package node

import (
	"testing"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/transport"
)

// The v1 adapter is the only place that knows both vocabularies; a kind that loses its wire type
// would fall through to the default and be dropped silently.
func TestCallWireTypesRoundTrip(t *testing.T) {
	for _, kind := range []transport.Kind{transport.Signal, transport.SignalLossy, transport.MediaAudio, transport.MediaVideo} {
		msgType, ok := callWireType(kind)
		if !ok {
			t.Fatalf("no wire type for call kind %d", kind)
		}
		got, ok := callKindOf(msgType)
		if !ok || got != kind {
			t.Fatalf("call kind %d became %d (%v)", kind, got, ok)
		}
	}
	if _, ok := callWireType(transport.Kind(200)); ok {
		t.Fatal("an unknown call kind has a wire type")
	}
	for _, msgType := range []net_types.MessageType{net_types.TypeUnknown, net_types.TypeChatMessage, net_types.TypeRelayAck} {
		if kind, ok := callKindOf(msgType); ok {
			t.Fatalf("wire type %d mapped to call kind %d", msgType, kind)
		}
	}
}
