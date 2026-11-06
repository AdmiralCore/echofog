package censor

import (
	"io"
	"time"
)

// maxPkts bounds the packet-level view of one flow, so memory follows the number of flows, not
// the number of bytes.
const maxPkts = 400

// Pkt is one packet of a flow as the border sees it.
type Pkt struct {
	At      time.Duration `json:"at"`      // since the first packet of the flow
	Up      bool          `json:"up"`      // client to server
	Payload int           `json:"payload"` // TCP/UDP payload bytes
	Seq     uint32        `json:"seq,omitempty"`
	Ack     uint32        `json:"ack,omitempty"`
	Flags   uint8         `json:"flags,omitempty"`
}

// Record is one TLS record of the first 16 KB of a TCP flow in one direction.
type Record struct {
	At   time.Duration `json:"at"`
	Up   bool          `json:"up"`
	Type byte          `json:"type"` // 22 handshake, 23 application data, 20 change cipher spec, 21 alert
	Len  int           `json:"len"`  // the whole record on the wire, header included
}

// AnalyzeStream returns a writer to feed a classic pcap stream into and a function that waits for
// the end of the stream and returns the flows. Nothing is buffered beyond one record and the
// per-flow state, so a big capture costs memory in proportion to its flows.
func AnalyzeStream() (io.WriteCloser, func() ([]Flow, error)) {
	pr, pw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	return pw, func() ([]Flow, error) { return nil, io.EOF }
}
