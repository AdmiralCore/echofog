package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/echofog/echofog-core/testbed/censor"
)

// tinyPcap is a classic pcap (raw IPv4) of n TCP data packets from one client to one server.
func tinyPcap(n int) []byte {
	var b bytes.Buffer
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:], 2)
	binary.LittleEndian.PutUint16(hdr[6:], 4)
	binary.LittleEndian.PutUint32(hdr[16:], 65535)
	binary.LittleEndian.PutUint32(hdr[20:], 101)
	b.Write(hdr)
	pkt := func(i int, flags byte, payload []byte) {
		ip := make([]byte, 40+len(payload))
		ip[0] = 0x45
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		ip[8], ip[9] = 64, 6
		copy(ip[12:], []byte{203, 0, 113, 21})
		copy(ip[16:], []byte{203, 0, 113, 15})
		binary.BigEndian.PutUint16(ip[20:], 40000)
		binary.BigEndian.PutUint16(ip[22:], 443)
		binary.BigEndian.PutUint32(ip[24:], uint32(100+i*len(payload)))
		ip[32] = 5 << 4
		ip[33] = flags
		copy(ip[40:], payload)
		rec := make([]byte, 16)
		binary.LittleEndian.PutUint32(rec[0:], 1_700_000_000)
		binary.LittleEndian.PutUint32(rec[4:], uint32(i*1000))
		binary.LittleEndian.PutUint32(rec[8:], uint32(len(ip)))
		binary.LittleEndian.PutUint32(rec[12:], uint32(len(ip)))
		b.Write(rec)
		b.Write(ip)
	}
	pkt(0, 0x02, nil)
	for i := 1; i <= n; i++ {
		pkt(i, 0x18, bytes.Repeat([]byte{0x41}, 100))
	}
	return b.Bytes()
}

func TestStreamingSinkAnalysesWhileItReceivesAndKeepsNothing(t *testing.T) {
	data := tinyPcap(300)
	want, err := censor.AnalyzeReader(bytes.NewReader(data))
	require.NoError(t, err)

	s := newStreamingSink(0, nil)
	for i := 0; i < len(data); i += 1000 {
		n, err := s.Write(data[i:min(i+1000, len(data))])
		require.NoError(t, err)
		require.Equal(t, min(1000, len(data)-i), n)
	}
	flows, err := s.Finish()
	require.NoError(t, err)
	assert.Equal(t, want, flows)
	assert.Equal(t, int64(len(data)), s.Size())
	assert.Zero(t, s.buffered(), "the bytes of the capture are not kept in memory")
	assert.Nil(t, s.Bytes())
}

func TestStreamingSinkStillObeysTheBudgetAndTheFile(t *testing.T) {
	data := tinyPcap(300)
	path := filepath.Join(t.TempDir(), "w.pcap")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	limit := int64(len(data) / 2)
	s := newStreamingSink(limit, f)

	var werr error
	for i := 0; i < len(data) && werr == nil; i += 700 {
		_, werr = s.Write(data[i:min(i+700, len(data))])
	}
	assert.ErrorIs(t, werr, errCaptureLimit)
	assert.Equal(t, limit, s.Size())
	assert.True(t, s.Over())
	flows, err := s.Finish()
	require.NoError(t, err, "a capture cut at the budget is still read up to there")
	require.NotEmpty(t, flows)
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, limit, st.Size(), "the file never grows past the budget")
}

func TestStreamingSinkFinishWithoutDataIsNotAHang(t *testing.T) {
	s := newStreamingSink(0, nil)
	done := make(chan struct{})
	go func() {
		_, _ = s.Finish()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Finish blocks on an empty capture")
	}
}
