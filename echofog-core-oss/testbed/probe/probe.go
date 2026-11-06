// Package probe is the active prober of the launchpad (detectors D11–D15). It runs from the border
// (the gateway), never from a node, and asks one question of everything it sends: does the node
// answer differently from the cover site it hides behind? An answer of the node's own is a leak.
//
// Every probe sends the same bytes to the node and to the cover and compares what comes back.
// What comes back is reduced to an Answer (a class and a few stable details), so the comparison
// does not depend on timestamps, random values or the order of TCP segments.
package probe

import (
	"context"
	"net"
	"time"
)

// Dialer opens a connection; the runner passes one bound to the gateway's source address.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Target is a node under test and the cover it hides behind.
type Target struct {
	Name    string // the scenario: "ordinary", "cover", "hidden", "server"
	Network string // "tcp" or "udp"
	Addr    string // the node
	Cover   string // the cover site; the reference answers come from here
	SNI     string // the name the cover site serves
	Dial    Dialer // nil means net.Dialer
	AltDial Dialer // a second source address, for the replay from elsewhere; nil skips that case
}

// Inputs are what the probes need from a legitimate client that the harness ran.
type Inputs struct {
	// FirstFlight is the first flight of a client with a valid token, as the border saw it.
	FirstFlight []byte
	// Valid and Foreign are fresh first flights: one with a token the node accepts, one made for
	// another node's key. Used by the token probe.
	Valid, Foreign []byte
	// Window is how long after the legitimate flight the late replay waits; Wait does the waiting
	// (the harness advances a clock, a test does not sleep).
	Window time.Duration
	Wait   func(time.Duration)
	// Listen is how long to wait for an answer before it counts as silence.
	Listen time.Duration
}

// Answer is what came back, reduced to what two honest answers share.
type Answer struct {
	Class  string `json:"class"` // silence, closed, reset, alert, tls, http, datagram, bytes
	Detail string `json:"detail,omitempty"`
}

// Outcome is one probe case.
type Outcome struct {
	Detector string `json:"detector"` // D11…D15
	Case     string `json:"case"`
	Node     Answer `json:"node"`
	Cover    Answer `json:"cover"`
	Own      bool   `json:"own"`      // the node answered differently from the cover
	Expected bool   `json:"expected"` // the node is meant to answer on its own here (a valid token)
	Skipped  string `json:"skipped,omitempty"`
}

// Report is one target's run.
type Report struct {
	Target   string    `json:"target"`
	Outcomes []Outcome `json:"outcomes"`
}

// OwnAnswers counts the answers of the node's own that were not expected: the gate wants zero.
func (r Report) OwnAnswers() int { return -1 }

// Reduce turns the bytes read from a connection (and how the read ended) into an Answer.
func Reduce(network string, got []byte, end error) Answer { return Answer{} }

// Equivalent says whether two answers are the same to an observer.
func Equivalent(a, b Answer) bool { return false }

// Replay is D11: the first flight of a legitimate client again, at once, after the window and
// from another address.
func Replay(ctx context.Context, t Target, in Inputs) []Outcome { return nil }

// Malformed is D12: a wrong version, a truncated first record, a short header, garbage, an
// oversized length.
func Malformed(ctx context.Context, t Target, in Inputs) []Outcome { return nil }

// CoverCheck is D13: the certificate, ALPN, protocol version and the page of a stranger's visit.
func CoverCheck(ctx context.Context, t Target, in Inputs) []Outcome { return nil }

// PostSuspicion is D14: after a first suspicious flight, a series of 16, 64 and 256 bytes.
func PostSuspicion(ctx context.Context, t Target, in Inputs) []Outcome { return nil }

// TokenProbe is D15: a valid token is answered (expected), a foreign one gets the cover.
func TokenProbe(ctx context.Context, t Target, in Inputs) []Outcome { return nil }

// Run runs D11–D15 against one target.
func Run(ctx context.Context, t Target, in Inputs) Report { return Report{Target: t.Name} }
