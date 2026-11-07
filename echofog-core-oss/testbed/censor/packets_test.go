package censor

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pcapBuilder writes a classic pcap of raw IPv4 packets (link type 101).
type pcapBuilder struct {
	buf bytes.Buffer
	t0  time.Time
}

func newPcap() *pcapBuilder {
	b := &pcapBuilder{t0: time.Unix(1_700_000_000, 0)}
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:], 2)
	binary.LittleEndian.PutUint16(hdr[6:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], 65535)
	binary.LittleEndian.PutUint32(hdr[20:], linkRaw)
	b.buf.Write(hdr)
	return b
}

func (b *pcapBuilder) tcp(at time.Duration, src, dst netip.AddrPort, seq, ack uint32, flags byte, payload []byte) {
	ip := make([]byte, 20+20+len(payload))
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[8], ip[9] = 64, 6
	copy(ip[12:], src.Addr().AsSlice())
	copy(ip[16:], dst.Addr().AsSlice())
	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], src.Port())
	binary.BigEndian.PutUint16(tcp[2:], dst.Port())
	binary.BigEndian.PutUint32(tcp[4:], seq)
	binary.BigEndian.PutUint32(tcp[8:], ack)
	tcp[12] = 5 << 4
	tcp[13] = flags
	copy(tcp[20:], payload)
	rec := make([]byte, 16)
	ts := b.t0.Add(at)
	binary.LittleEndian.PutUint32(rec[0:], uint32(ts.Unix()))
	binary.LittleEndian.PutUint32(rec[4:], uint32(ts.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(rec[8:], uint32(len(ip)))
	binary.LittleEndian.PutUint32(rec[12:], uint32(len(ip)))
	b.buf.Write(rec)
	b.buf.Write(ip)
}

func tlsRecord(typ byte, n int) []byte {
	out := make([]byte, n)
	out[0], out[1], out[2] = typ, 3, 3
	binary.BigEndian.PutUint16(out[3:], uint16(n-5))
	for i := 5; i < n; i++ {
		out[i] = byte(i * 7)
	}
	return out
}

var (
	cliAP = netip.MustParseAddrPort("203.0.113.21:40000")
	srvAP = netip.MustParseAddrPort("203.0.113.15:443")
)

// exchange: SYN, SYN-ACK, ACK, a client record, the server's ACK 20 ms later, a server record
// 50 ms after the client's, the client's ACK.
func exchangePcap() *pcapBuilder {
	b := newPcap()
	ms := time.Millisecond
	b.tcp(0, cliAP, srvAP, 100, 0, 0x02, nil)
	b.tcp(10*ms, srvAP, cliAP, 500, 101, 0x12, nil)
	b.tcp(20*ms, cliAP, srvAP, 101, 501, 0x10, nil)
	req := tlsRecord(22, 517)
	b.tcp(30*ms, cliAP, srvAP, 101, 501, 0x18, req)
	b.tcp(50*ms, srvAP, cliAP, 501, 101+517, 0x10, nil)
	resp := tlsRecord(22, 122)
	b.tcp(80*ms, srvAP, cliAP, 501, 101+517, 0x18, resp)
	b.tcp(90*ms, cliAP, srvAP, 101+517, 501+122, 0x10, nil)
	return b
}

func TestAnalyzeFillsThePacketViewAndTheRecords(t *testing.T) {
	b := exchangePcap()
	flows, err := AnalyzeReader(bytes.NewReader(b.buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, flows, 1)
	f := flows[0]

	require.Len(t, f.Pkts, 7, "empty ACKs and handshake packets are kept: D6 needs them")
	assert.True(t, f.Pkts[3].Up)
	assert.Equal(t, 517, f.Pkts[3].Payload)
	assert.Equal(t, uint32(101), f.Pkts[3].Seq)
	assert.Equal(t, uint32(501), f.Pkts[3].Ack)
	assert.Equal(t, 30*time.Millisecond, f.Pkts[3].At)
	assert.False(t, f.Pkts[4].Up)
	assert.Equal(t, 0, f.Pkts[4].Payload)
	assert.Equal(t, uint32(101+517), f.Pkts[4].Ack)

	require.Len(t, f.Records, 2)
	assert.Equal(t, Record{At: 30 * time.Millisecond, Up: true, Type: 22, Len: 517}, f.Records[0])
	assert.Equal(t, Record{At: 80 * time.Millisecond, Up: false, Type: 22, Len: 122}, f.Records[1])
}

func TestRecordsStopAt16KBAndSurviveARecordSplitOverSegments(t *testing.T) {
	b := newPcap()
	b.tcp(0, cliAP, srvAP, 100, 0, 0x02, nil)
	rec := tlsRecord(23, 3000)
	b.tcp(time.Millisecond, cliAP, srvAP, 101, 1, 0x18, rec[:1448])
	b.tcp(2*time.Millisecond, cliAP, srvAP, 101+1448, 1, 0x18, rec[1448:])
	seq := uint32(101 + 3000)
	for i := 0; i < 8; i++ { // more than 16 KB in all
		b.tcp(time.Duration(3+i)*time.Millisecond, cliAP, srvAP, seq, 1, 0x18, tlsRecord(23, 3000))
		seq += 3000
	}
	flows, err := AnalyzeReader(bytes.NewReader(b.buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, flows, 1)
	require.NotEmpty(t, flows[0].Records)
	assert.Equal(t, 3000, flows[0].Records[0].Len, "a record split over two segments is one record")
	assert.LessOrEqual(t, len(flows[0].Records), 6, "only the first 16 KB are read (5 records of 3000)")
}

func TestPacketViewIsBounded(t *testing.T) {
	b := newPcap()
	b.tcp(0, cliAP, srvAP, 100, 0, 0x02, nil)
	for i := 0; i < 3*maxPkts; i++ {
		b.tcp(time.Duration(i+1)*time.Millisecond, srvAP, cliAP, 500, 101, 0x10, nil)
	}
	flows, err := AnalyzeReader(bytes.NewReader(b.buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, flows, 1)
	assert.Len(t, flows[0].Pkts, maxPkts)
	assert.Equal(t, 3*maxPkts+1, flows[0].Packets, "the counters still see every packet")
}

func TestAnalyzeStreamEqualsAnalyzeAndNeverNeedsTheWholeCapture(t *testing.T) {
	data := exchangePcap().buf.Bytes()
	want, err := AnalyzeReader(bytes.NewReader(data))
	require.NoError(t, err)

	w, wait := AnalyzeStream()
	// a few bytes at a time, the way a pipe from tcpdump delivers them
	for i := 0; i < len(data); i += 7 {
		n, err := w.Write(data[i:min(i+7, len(data))])
		require.NoError(t, err)
		require.Equal(t, min(7, len(data)-i), n)
	}
	require.NoError(t, w.Close())
	got, err := wait()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestAnalyzeStreamSaysWhenThePcapIsBroken(t *testing.T) {
	w, wait := AnalyzeStream()
	go func() {
		_, _ = w.Write([]byte("this is not a pcap file at all, sorry"))
		_ = w.Close()
	}()
	_, err := wait()
	assert.Error(t, err)
	// writing after the reader gave up must not block the capture pump forever
	done := make(chan struct{})
	go func() {
		_, _ = w.Write(bytes.Repeat([]byte{1}, 1<<16))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a write after the analyzer stopped blocks")
	}
}

// ---------------------------------------------------------------------------- H1a

func TestH1aMeasurePassesBrowserLikeRecordsAndFailsFixedOnes(t *testing.T) {
	flowsOf := func(ls []LabeledFlow) []Flow {
		out := make([]Flow, len(ls))
		for i := range ls {
			out[i] = ls[i].Flow
		}
		return out
	}
	browser := flowsOf(synthSet(kBrowser, 9000, 200))
	like := flowsOf(synthSet(kBrowser, 31000, 40))
	fixed := flowsOf(synthSet(kConstSize, 100, 40))

	ok := H1aMeasure(like, browser)
	assert.Equal(t, 40, ok.Flows)
	assert.True(t, ok.Pass, "KS %.3f against %.3f", ok.KS, ok.Threshold)
	bad := H1aMeasure(fixed, browser)
	assert.False(t, bad.Pass, "KS %.3f against %.3f", bad.KS, bad.Threshold)
	assert.Greater(t, bad.KS, ok.KS)
	assert.Equal(t, ok.Threshold, bad.Threshold, "the threshold is a property of the browser control")
}

func TestH1aWithoutABrowserControlIsNotAPass(t *testing.T) {
	r := H1aMeasure([]Flow{synthFlow(kBrowser, 1, 1).Flow}, nil)
	assert.False(t, r.Pass)
}

// ---------------------------------------------------------------------------- Recorder

func TestRecorderTurnsAConnectionIntoAFlow(t *testing.T) {
	a, b := net.Pipe()
	rec := NewRecorder(a, time.Now())
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = b.Write(tlsRecord(23, 300)) // the answer
			}
		}
	}()
	_, err := rec.Write(tlsRecord(22, 517))
	require.NoError(t, err)
	got := make([]byte, 300)
	_, err = io.ReadFull(rec, got)
	require.NoError(t, err)
	_, err = rec.Write(tlsRecord(23, 200))
	require.NoError(t, err)
	_, err = io.ReadFull(rec, got)
	require.NoError(t, err)
	_ = rec.Close()
	_ = b.Close()

	f := rec.Flow()
	assert.Equal(t, "tcp", f.Proto)
	require.Len(t, f.Records, 4)
	assert.True(t, f.Records[0].Up)
	assert.Equal(t, byte(22), f.Records[0].Type)
	assert.Equal(t, 517, f.Records[0].Len)
	assert.False(t, f.Records[1].Up)
	assert.Equal(t, 300, f.Records[1].Len)
	assert.Equal(t, 517+200, f.BytesUp, "a recorded flow counts payload bytes")
	assert.GreaterOrEqual(t, len(f.Pkts), 4)
	assert.Equal(t, 600, f.PayDn)
}

// N2 of the T15.0b acceptance: the number of flows is bounded, and what did not fit is counted.
func TestAnalyzerCapsTheFlowsAndCountsTheRest(t *testing.T) {
	b := newPcap()
	for i := 0; i < 50; i++ {
		src := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.21"), uint16(30000+i))
		b.tcp(time.Duration(i)*time.Millisecond, src, srvAP, 100, 0, 0x02, nil)
		b.tcp(time.Duration(i)*time.Millisecond+500*time.Microsecond, src, srvAP, 101, 1, 0x18, tlsRecord(22, 100))
	}
	an := newAnalyzer()
	an.maxFlows = 10
	require.NoError(t, streamPcap(bytes.NewReader(b.buf.Bytes()), an.add))
	flows := an.finish()
	assert.Len(t, flows, 10, "the first ten flows are kept whole")
	assert.Equal(t, 40, an.dropped, "forty flows past the bound are counted, not analysed")
	for _, f := range flows {
		assert.Equal(t, 2, f.Packets)
	}

	w, wait := AnalyzeStreamStats()
	_, err := w.Write(b.buf.Bytes())
	require.NoError(t, err)
	require.NoError(t, w.Close())
	res, err := wait()
	require.NoError(t, err)
	assert.Len(t, res.Flows, 50, "under the default bound nothing is dropped")
	assert.Zero(t, res.DroppedFlows)
}

func TestTheDefaultFlowBoundIsLarge(t *testing.T) {
	assert.GreaterOrEqual(t, MaxFlows, 100_000)
}
