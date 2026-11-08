package node

import "time"

// IncomingCall is a call invitation of a direct chat (T11.4b): the event the desktop shows as
// "call:incoming", or as "call:missed" when the invitation came too late to ring.
type IncomingCall struct {
	CallID      string `json:"callId"`
	Coordinator string `json:"coordinator"`
	From        string `json:"from"`      // the node ID of the caller
	Timestamp   int64  `json:"timestamp"` // when the invitation was written, ms
	Missed      bool   `json:"missed"`
}

// v2Now is the clock of the ring decision; a test replaces it.
var v2Now = time.Now

// OnIncomingCall is not built yet.
func (n *NodeApi) OnIncomingCall(cb func(IncomingCall)) {}
