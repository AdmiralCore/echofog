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
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
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
	// (the harness advances a clock, a test does not sleep). A nil Wait sleeps.
	Window time.Duration
	Wait   func(time.Duration)
	// Listen is how long to wait for an answer before it counts as silence; zero means one second.
	Listen time.Duration
}

func (in Inputs) listen() time.Duration {
	if in.Listen <= 0 {
		return time.Second
	}
	return in.Listen
}

func (in Inputs) wait(d time.Duration) {
	if in.Wait != nil {
		in.Wait(d)
		return
	}
	time.Sleep(d)
}

// Answer is what came back, reduced to what two honest answers share.
type Answer struct {
	Class  string `json:"class"` // silence, closed, reset, refused, alert, tls, http, datagram, bytes
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
	Note     string `json:"note,omitempty"`
}

// Report is one target's run.
type Report struct {
	Target   string    `json:"target"`
	Outcomes []Outcome `json:"outcomes"`
}

// OwnAnswers counts the answers of the node's own that were not expected: the gate wants zero.
func (r Report) OwnAnswers() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Own && !o.Expected && o.Skipped == "" {
			n++
		}
	}
	return n
}

// Equivalent says whether two answers are the same to an observer.
func Equivalent(a, b Answer) bool { return a == b }

// Reduce turns the bytes read from a connection (and how the read ended) into an Answer.
func Reduce(network string, got []byte, end error) Answer {
	if network == "udp" {
		if len(got) == 0 {
			if end != nil && !isTimeout(end) {
				return Answer{Class: "refused"}
			}
			return Answer{Class: "silence"}
		}
		if got[0]&0x80 != 0 && len(got) >= 5 {
			return Answer{Class: "datagram", Detail: fmt.Sprintf("long/%08x", uint32(got[1])<<24|uint32(got[2])<<16|uint32(got[3])<<8|uint32(got[4]))}
		}
		return Answer{Class: "datagram", Detail: "short"}
	}
	if len(got) == 0 {
		switch {
		case end == nil, errors.Is(end, io.EOF):
			return Answer{Class: "closed"}
		case errors.Is(end, syscall.ECONNRESET) || strings.Contains(end.Error(), "reset"):
			return Answer{Class: "reset"}
		case errors.Is(end, syscall.ECONNREFUSED) || strings.Contains(end.Error(), "refused"):
			return Answer{Class: "refused"}
		case isTimeout(end):
			return Answer{Class: "silence"}
		}
		return Answer{Class: "closed"}
	}
	if len(got) >= 7 && got[0] == 0x15 && got[1] == 3 {
		return Answer{Class: "alert", Detail: fmt.Sprint(got[6])}
	}
	if len(got) >= 5 && got[1] == 3 && (got[0] == 0x14 || got[0] == 0x16 || got[0] == 0x17) {
		return Answer{Class: "tls", Detail: fmt.Sprintf("%d/%02x%02x/%d", got[0], got[1], got[2], int(got[3])<<8|int(got[4]))}
	}
	if strings.HasPrefix(string(got), "HTTP/") {
		f := strings.Fields(string(got))
		if len(f) >= 2 {
			return Answer{Class: "http", Detail: f[1]}
		}
	}
	return Answer{Class: "bytes"}
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "timeout") || strings.Contains(s, "deadline")
}

// ---------------------------------------------------------------------------- the wire

func (t Target) network() string {
	if t.Network == "" {
		return "tcp"
	}
	return t.Network
}

func dialWith(d Dialer, ctx context.Context, network, addr string) (net.Conn, error) {
	if d == nil {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
	}
	return d(ctx, network, addr)
}

// readAnswer reads what comes back for up to listen: it stops as soon as there is enough to tell
// the answer apart (16 bytes), at the end of the stream, or at the deadline.
func readAnswer(c net.Conn, network string, listen time.Duration) Answer {
	_ = c.SetReadDeadline(time.Now().Add(listen))
	buf := make([]byte, 2048)
	var got []byte
	var err error
	for len(got) < 16 {
		var n int
		n, err = c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil || network == "udp" {
			break
		}
	}
	if len(got) >= 16 {
		err = nil
	}
	return Reduce(network, got, err)
}

// one sends payload on a fresh connection to addr and returns the answer.
func one(ctx context.Context, d Dialer, network, addr string, payload []byte, listen time.Duration) Answer {
	c, err := dialWith(d, ctx, network, addr)
	if err != nil {
		return Reduce(network, nil, err)
	}
	defer c.Close()
	if len(payload) > 0 {
		if _, err := c.Write(payload); err != nil {
			return Reduce(network, nil, err)
		}
	}
	return readAnswer(c, network, listen)
}

// compare sends payload to the node and to the cover and returns the outcome.
func compare(ctx context.Context, t Target, d Dialer, det, name string, payload []byte, listen time.Duration) Outcome {
	o := Outcome{Detector: det, Case: name}
	o.Node = one(ctx, d, t.network(), t.Addr, payload, listen)
	o.Cover = one(ctx, t.Dial, t.network(), t.Cover, payload, listen)
	o.Own = !Equivalent(o.Node, o.Cover)
	return o
}

func skipped(det, name, why string) Outcome {
	return Outcome{Detector: det, Case: name, Skipped: why}
}

// ---------------------------------------------------------------------------- D11

// Replay is D11: the first flight of a legitimate client again, at once, after the window and
// from another address.
func Replay(ctx context.Context, t Target, in Inputs) []Outcome {
	names := []string{"at once", "after the window", "other address"}
	if len(in.FirstFlight) == 0 {
		out := make([]Outcome, len(names))
		for i, n := range names {
			out[i] = skipped("D11", n, "no first flight of a legitimate client was captured")
		}
		return out
	}
	out := []Outcome{compare(ctx, t, t.Dial, "D11", names[0], in.FirstFlight, in.listen())}
	in.wait(in.Window)
	out = append(out, compare(ctx, t, t.Dial, "D11", names[1], in.FirstFlight, in.listen()))
	if t.AltDial == nil {
		out = append(out, skipped("D11", names[2], "no second source address is configured"))
	} else {
		out = append(out, compare(ctx, t, t.AltDial, "D11", names[2], in.FirstFlight, in.listen()))
	}
	return out
}

// ---------------------------------------------------------------------------- D12

func filler(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// Malformed is D12: a wrong version, a truncated first record, a short header, garbage, an
// oversized length.
func Malformed(ctx context.Context, t Target, in Inputs) []Outcome {
	type probe struct {
		name string
		b    []byte
	}
	var cases []probe
	if t.network() == "udp" {
		long := func(version uint32, n int) []byte {
			b := filler(n, 7)
			b[0] = 0xc3
			b[1], b[2], b[3], b[4] = byte(version>>24), byte(version>>16), byte(version>>8), byte(version)
			b[5] = 8
			return b
		}
		cases = []probe{
			{"unknown version", long(0xdeadbeef, 1200)},
			{"truncated Initial", long(1, 40)},
			{"short header", []byte{0x40}},
			{"garbage", filler(1200, 3)},
			{"oversized", long(1, 1452)},
		}
	} else {
		hello := in.FirstFlight
		if len(hello) < 5 {
			hello = append([]byte{0x16, 0x03, 0x01, 0x02, 0x00}, filler(512, 1)...)
		}
		wrong := append([]byte{0x16, 0x01, 0x00, 0x00, 0x14}, filler(20, 2)...)
		huge := append([]byte{0x16, 0x03, 0x01, 0xff, 0xff}, filler(100, 4)...)
		cases = []probe{
			{"wrong version", wrong},
			{"truncated hello", hello[:min(60, len(hello))]},
			{"short header", []byte{0x16, 0x03, 0x03}},
			{"garbage", filler(64, 5)},
			{"oversized length", huge},
		}
	}
	out := make([]Outcome, 0, len(cases))
	for _, c := range cases {
		out = append(out, compare(ctx, t, t.Dial, "D12", c.name, c.b, in.listen()))
	}
	return out
}

// ---------------------------------------------------------------------------- D13

// CoverCheck is D13: the certificate, ALPN, protocol version and the page of a stranger's visit.
func CoverCheck(ctx context.Context, t Target, in Inputs) []Outcome {
	names := []string{"certificate", "alpn", "page"}
	if t.network() != "tcp" {
		out := make([]Outcome, len(names))
		for i, n := range names {
			out[i] = skipped("D13", n, "the cover check reads a TLS site over TCP; HTTP/3 covers are not read here")
		}
		return out
	}
	node := visit(ctx, t.Dial, t.Addr, t.SNI)
	cover := visit(ctx, t.Dial, t.Cover, t.SNI)
	return []Outcome{
		pair("D13", names[0], node.cert, cover.cert),
		pair("D13", names[1], node.alpn, cover.alpn),
		pair("D13", names[2], node.page, cover.page),
	}
}

func pair(det, name string, node, cover Answer) Outcome {
	return Outcome{Detector: det, Case: name, Node: node, Cover: cover, Own: !Equivalent(node, cover)}
}

// PostSuspicion is D14: a source that has sent a suspicious first flight then sends 16, 64 and 256
// bytes of its own. A node that has marked the source must still answer as the cover does.
func PostSuspicion(ctx context.Context, t Target, in Inputs) []Outcome {
	suspicious := append([]byte{0x16, 0x03, 0x01, 0x00, 0x30}, filler(48, 9)...)
	var out []Outcome
	for _, n := range []int{16, 64, 256} {
		name := fmt.Sprintf("%d bytes", n)
		o := Outcome{Detector: "D14", Case: name}
		o.Node = afterSuspicion(ctx, t.Dial, t.network(), t.Addr, suspicious, filler(n, byte(n)), in.listen())
		o.Cover = afterSuspicion(ctx, t.Dial, t.network(), t.Cover, suspicious, filler(n, byte(n)), in.listen())
		o.Own = !Equivalent(o.Node, o.Cover)
		out = append(out, o)
	}
	return out
}

// afterSuspicion sends the suspicious flight on one connection, lets it end, then sends the probe
// on another connection from the same source and returns that answer. A reset and a plain close
// are one answer here: which of the two a kernel gives depends on a race the observer cannot use.
func afterSuspicion(ctx context.Context, d Dialer, network, addr string, suspicious, probe []byte, listen time.Duration) Answer {
	_ = one(ctx, d, network, addr, suspicious, listen)
	time.Sleep(50 * time.Millisecond)
	a := one(ctx, d, network, addr, probe, listen)
	if a.Class == "reset" {
		a.Class = "closed"
	}
	return a
}

// ---------------------------------------------------------------------------- D15

// TokenProbe is D15: a valid token is answered (expected), a foreign one gets the cover.
func TokenProbe(ctx context.Context, t Target, in Inputs) []Outcome {
	if len(in.Valid) == 0 || len(in.Foreign) == 0 {
		return []Outcome{
			skipped("D15", "valid token", "no fresh flights were prepared"),
			skipped("D15", "foreign token", "no fresh flights were prepared"),
		}
	}
	valid := compare(ctx, t, t.Dial, "D15", "valid token", in.Valid, in.listen())
	valid.Expected = true
	if !valid.Own {
		valid.Note = "the first record of the answer is the cover's: reality2 reproduces the cover's flight, so this probe cannot tell an admitted token from a forwarded one"
	}
	foreign := compare(ctx, t, t.Dial, "D15", "foreign token", in.Foreign, in.listen())
	return []Outcome{valid, foreign}
}

// ---------------------------------------------------------------------------- the run

// Run runs D11–D15 against one target.
func Run(ctx context.Context, t Target, in Inputs) Report {
	r := Report{Target: t.Name}
	r.Outcomes = append(r.Outcomes, Replay(ctx, t, in)...)
	r.Outcomes = append(r.Outcomes, Malformed(ctx, t, in)...)
	r.Outcomes = append(r.Outcomes, CoverCheck(ctx, t, in)...)
	r.Outcomes = append(r.Outcomes, PostSuspicion(ctx, t, in)...)
	r.Outcomes = append(r.Outcomes, TokenProbe(ctx, t, in)...)
	return r
}
