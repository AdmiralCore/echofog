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
// per-flow state, so a big capture costs memory in proportion to its flows. When the stream is
// not a pcap file the analyzer stops, and writes to the writer fail instead of blocking.
func AnalyzeStream() (io.WriteCloser, func() ([]Flow, error)) {
	w, wait := AnalyzeStreamStats()
	return w, func() ([]Flow, error) {
		a, err := wait()
		return a.Flows, err
	}
}

// Analysis is the result of a stream analysis: the flows and how many new flows past
// MaxFlows were not analysed.
type Analysis struct {
	Flows          []Flow
	DroppedFlows int
}

// AnalyzeStreamStats is AnalyzeStream with the count of what did not fit.
func AnalyzeStreamStats() (io.WriteCloser, func() (Analysis, error)) {
	pr, pw := io.Pipe()
	done := make(chan struct {
		a   Analysis
		err error
	}, 1)
	go func() {
		an := newAnalyzer()
		err := streamPcap(pr, an.add)
		if err != nil {
			_ = pr.CloseWithError(err) // a writer that is still writing gets the error, not a hang
			done <- struct {
				a   Analysis
				err error
			}{Analysis{}, err}
			return
		}
		_, _ = io.Copy(io.Discard, pr) // anything after the last record (a cut-off tail)
		done <- struct {
			a   Analysis
			err error
		}{Analysis{Flows: an.finish(), DroppedFlows: an.dropped}, nil}
	}()
	return pw, func() (Analysis, error) {
		r := <-done
		return r.a, r.err
	}
}
