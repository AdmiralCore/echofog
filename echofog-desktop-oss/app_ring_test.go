package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// T11.4b: the events chats.js and the call store listen to, with the fields they read.
func TestCallEventOfNamesTheEventsTheFrontendKnows(t *testing.T) {
	ev, p := callEventOf(api.IncomingCall{CallID: "call_1", Coordinator: "co", From: "bob", Timestamp: 7})
	require.Equal(t, "call:incoming", ev)
	require.Equal(t, map[string]interface{}{"callId": "call_1", "coordinator": "co", "from": "bob", "timestamp": int64(7)}, p)
	ev, p = callEventOf(api.IncomingCall{CallID: "call_2", Missed: true, From: "bob"})
	require.Equal(t, "call:missed", ev)
	require.Equal(t, "bob", p["from"], "chats.js names the caller from `from`")
}
