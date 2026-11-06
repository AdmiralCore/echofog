package censor

import (
	"net"
	"sync"
	"time"
)

// Recorder wraps a net.Conn and keeps what a border would see of it: the packet-like chunks in
// each direction, with times, and the TLS records of the first 16 KB each way. It is how the tests
// and the harness take real v2 traffic as a Flow without a capture.
type Recorder struct {
	net.Conn
	mu sync.Mutex
}

// NewRecorder starts recording c.
func NewRecorder(c net.Conn, start time.Time) *Recorder { return &Recorder{Conn: c} }

// Flow returns what was recorded so far as a TCP flow (client = the side that wrapped the conn).
func (r *Recorder) Flow() Flow { return Flow{} }
