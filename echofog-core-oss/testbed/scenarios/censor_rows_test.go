package scenarios

import (
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/censor"
	"github.com/echofog/echofog-core/testbed/gate"
	"github.com/echofog/echofog-core/testbed/probe"
)

func controlConfig() censor.DetectorConfig {
	return censor.DetectorConfig{
		BlockedSNI: map[string]bool{"blocked.example": true},
		Hosted:     map[netip.Addr][]string{fakeSrv.Addr(): {"cover.example"}},
	}
}

// tcpFlowsThrough runs client against a TCP server on loopback through the tap and returns the
// flows the censor read.
func tcpFlowsThrough(t *testing.T, serve func(net.Conn), client func(addr string)) []censor.Flow {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); serve(c) }()
		}
	}()
	pw, wait := newCapture()
	tap, err := NewTCPTap(ln.Addr().String(), fakeSrv, pw)
	require.NoError(t, err)
	client(tap.Addr())
	require.NoError(t, tap.Close())
	flows, err := wait()
	require.NoError(t, err)
	return flows
}

// The scenario must be able to fail. A carrier that sends random bytes first is what the GFW
// blocks (D1): the classifier row for it is red, with the detector named, and says it is a finding.
func TestCensorClassifierRowIsRedForRandomBytes(t *testing.T) {
	flows := tcpFlowsThrough(t,
		func(c net.Conn) { _, _ = io.Copy(c, c) },
		func(addr string) {
			c, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			defer c.Close()
			b := make([]byte, 3000)
			_, _ = rand.Read(b)
			_, err = c.Write(b)
			require.NoError(t, err)
			_, err = io.ReadFull(c, make([]byte, len(b)))
			require.NoError(t, err)
		})
	row, finding := classifierRow("random", flows, nil, controlConfig())
	assert.Equal(t, gate.Red, row.Status, "%s: %s", row.Value, row.Note)
	assert.Contains(t, row.Value, "D1")
	assert.NotEmpty(t, finding)
	assert.Equal(t, "v2-censor/random/classifier", row.ID)
}

// A plain Go TLS client that names a site the address does not host: D3 (SNI against the address)
// and D4 (a library's ClientHello, no GREASE) flag it.
func TestCensorClassifierRowIsRedForAForeignSNIWithAnOrdinaryTLSStack(t *testing.T) {
	target := tlsEcho(t, "foreign.example")
	_ = target
	flows := tcpFlowsThrough(t,
		func(c net.Conn) {
			s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "foreign.example")}})
			if s.Handshake() == nil {
				_, _ = io.Copy(s, s)
			}
		},
		func(addr string) {
			c, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			tc := tls.Client(c, &tls.Config{ServerName: "foreign.example", InsecureSkipVerify: true}) // #nosec G402 -- test certificate
			require.NoError(t, tc.Handshake())
			_, _ = tc.Write([]byte("hello"))
			_, _ = io.ReadFull(tc, make([]byte, 5))
			_ = tc.Close()
		})
	row, finding := classifierRow("tls-go", flows, nil, controlConfig())
	assert.Equal(t, gate.Red, row.Status, "%s: %s", row.Value, row.Note)
	assert.Contains(t, row.Value, "D3")
	assert.NotEmpty(t, finding)
}

// No flow of the carrier: nothing was measured, and an empty capture is never green.
func TestCensorClassifierRowWithNoFlowsIsNotMeasured(t *testing.T) {
	row, finding := classifierRow("empty", nil, nil, controlConfig())
	assert.Equal(t, gate.NotMeasured, row.Status)
	assert.NotEmpty(t, row.Note)
	assert.Empty(t, finding)
}

func TestCensorControlRowWithNoBrowserFlowsIsNotMeasured(t *testing.T) {
	row := controlRow(nil, controlConfig())
	assert.Equal(t, gate.NotMeasured, row.Status)
	assert.NotEmpty(t, row.Note)
}

func TestCensorProbeRowReadsTheReport(t *testing.T) {
	quiet := probe.Answer{Class: "silence"}
	mk := func(outs ...probe.Outcome) probe.Report { return probe.Report{Target: "x", Outcomes: outs} }
	ok := probe.Outcome{Detector: "D12", Case: "garbage", Node: quiet, Cover: quiet}
	own := probe.Outcome{Detector: "D11", Case: "at once", Node: probe.Answer{Class: "tls"}, Cover: quiet, Own: true}
	skip := probe.Outcome{Detector: "D13", Case: "page", Skipped: "no cover"}

	row, finding := probeRow("c", mk(ok, ok, skip))
	assert.Equal(t, gate.Green, row.Status, "%s %s", row.Value, row.Note)
	assert.Empty(t, finding)
	assert.Contains(t, row.Note, "page", "a skipped case is named")

	row, finding = probeRow("c", mk(ok, own))
	assert.Equal(t, gate.Red, row.Status)
	assert.NotEmpty(t, finding)
	assert.Contains(t, row.Value, "1")

	row, _ = probeRow("c", mk(skip))
	assert.Equal(t, gate.NotMeasured, row.Status, "nothing ran")
	assert.NotEmpty(t, row.Note)
}

// A plain Go TLS client naming the site its address hosts: only D4 (a library's ClientHello) flags it.
func libraryHelloFlows(t *testing.T, sni string) []censor.Flow {
	t.Helper()
	return tcpFlowsThrough(t,
		func(c net.Conn) {
			s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{selfSigned(t, sni)}})
			if s.Handshake() == nil {
				_, _ = io.Copy(s, s)
			}
		},
		func(addr string) {
			c, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) // #nosec G402 -- test certificate
			require.NoError(t, tc.Handshake())
			_, _ = tc.Write([]byte("hello"))
			_, _ = io.ReadFull(tc, make([]byte, 5))
			_ = tc.Close()
		})
}

// A detector whose reference is missing (the browser profiles hold no Chrome hello for this wire) is
// not measured: the row is neither red because of it nor green in spite of it.
func TestCensorClassifierRowDoesNotCountADetectorWithoutAReference(t *testing.T) {
	flows := libraryHelloFlows(t, "cover.example")
	red, _ := classifierRow("tls-go", flows, nil, controlConfig())
	require.Equal(t, gate.Red, red.Status, "without the exclusion D4 flags the library hello: %s", red.Value)
	require.Contains(t, red.Value, "D4")

	row, finding := classifierRowWith("tls-go", flows, nil, controlConfig(), map[string]string{"D4": "no Chrome http/1.1 JA4 in browserprofiles"})
	assert.Equal(t, gate.NotMeasured, row.Status, "%s: %s", row.Value, row.Note)
	assert.Contains(t, row.Note, "D4")
	assert.Contains(t, row.Note, "no Chrome http/1.1 JA4")
	assert.Empty(t, finding, "an artifact of the references is not a finding of the product")
}

// The exclusion is for the named detector only: another one that flags still makes the row red.
func TestCensorClassifierRowStaysRedForADetectorThatIsNotExcluded(t *testing.T) {
	flows := libraryHelloFlows(t, "foreign.example") // D3 flags the SNI too
	row, finding := classifierRowWith("tls-go", flows, nil, controlConfig(), map[string]string{"D4": "no reference"})
	assert.Equal(t, gate.Red, row.Status, "%s: %s", row.Value, row.Note)
	assert.Contains(t, row.Value, "D3")
	assert.NotEmpty(t, finding)
	assert.NotContains(t, finding, "D4")
}
