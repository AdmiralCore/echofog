package dpi

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	"golang.org/x/net/ipv4"
)

// spyConn counts what quic-go does with an OOB-capable connection: GSO trains written, and
// datagrams read that arrived ECN-marked.
type spyConn struct {
	OOBPacketConn
	writes, gsoWrites, reads, ecnReads atomic.Int64
}

func (s *spyConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (int, int, error) {
	s.writes.Add(1)
	if seg, _ := gsoSegment(oob); seg > 0 && len(b) > seg {
		s.gsoWrites.Add(1)
	}
	return s.OOBPacketConn.WriteMsgUDP(b, oob, addr)
}

func (s *spyConn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	n, err := s.OOBPacketConn.ReadBatch(ms, flags)
	for i := 0; i < n; i++ {
		s.reads.Add(1)
		if ecnMarked(ms[i].OOB[:ms[i].NN]) {
			s.ecnReads.Add(1)
		}
	}
	return n, err
}

func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"test"}},
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"test"}, ServerName: "test"}
}

// quicDownload runs a quic-go server on serverConn and a quic-go client on clientConn; the client
// asks for size bytes and checks what arrives. It returns once the download is complete.
func quicDownload(t *testing.T, serverConn, clientConn net.PacketConn, size int) {
	t.Helper()
	srvTLS, cliTLS := testTLS(t)
	str := &quic.Transport{Conn: serverConn}
	t.Cleanup(func() { str.Close() })
	ln, err := str.Listen(srvTLS, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, size)
	_, _ = rand.Read(payload)
	want := sha256.Sum256(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() {
		c, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		s, err := c.AcceptStream(ctx)
		if err != nil {
			return
		}
		var req [1]byte
		if _, err := io.ReadFull(s, req[:]); err != nil {
			return
		}
		_, _ = s.Write(payload)
		_ = s.Close()
		<-ctx.Done()
	}()

	ctr := &quic.Transport{Conn: clientConn}
	t.Cleanup(func() { ctr.Close() })
	c, err := ctr.Dial(ctx, serverConn.LocalAddr(), cliTLS, &quic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseWithError(0, "")
	s, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	n, err := io.Copy(h, s)
	if err != nil {
		t.Fatalf("download after %d bytes: %v", n, err)
	}
	if n != int64(size) || !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatalf("downloaded %d bytes, want %d, intact=%v", n, size, bytes.Equal(h.Sum(nil), want[:]))
	}
}

// fastPathOS: where quic-go uses GSO and reads ECN marks.
var fastPathOS = runtime.GOOS == "linux"

// quic-go behind the STUN multiplexer keeps GSO (it writes trains of packets with one sendmsg)
// and reads the ECN marks of what arrives. Before, the multiplexer was not OOB-capable and quic-go
// fell back to one plain write per packet with ECN off.
func TestMuxQUICConnKeepsGSOAndECN(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	oc, ok := mux.GetQUICConn().(OOBPacketConn)
	if !ok {
		t.Fatal("the QUIC side of the multiplexer must be OOB-capable")
	}
	var _ quic.OOBCapablePacketConn = oc
	spy := &spyConn{OOBPacketConn: oc}
	quicDownload(t, spy, clientConn, 8<<20)
	t.Logf("server: %d writes, %d GSO trains; %d datagrams read, %d ECN-marked", spy.writes.Load(), spy.gsoWrites.Load(), spy.reads.Load(), spy.ecnReads.Load())
	checkFastPath(t, spy)
}

// checkFastPath: on Linux quic-go must have read through ReadBatch, written GSO trains and seen
// ECN marks; on Windows it must not have touched the OOB methods at all (it reads and writes one
// packet at a time there, as it always did).
func checkFastPath(t *testing.T, spy *spyConn) {
	t.Helper()
	if runtime.GOOS == "windows" && spy.writes.Load()+spy.reads.Load() != 0 {
		t.Fatal("on Windows quic-go must keep using ReadFrom/WriteTo")
	}
	if fastPathOS {
		if spy.reads.Load() == 0 {
			t.Fatal("quic-go must read through ReadBatch")
		}
		if spy.gsoWrites.Load() == 0 {
			t.Fatal("quic-go must write GSO trains through the multiplexer")
		}
		if spy.ecnReads.Load() == 0 {
			t.Fatal("the ECN marks of the client's packets must reach quic-go")
		}
	}
}

// ReadBatch hands over several queued datagrams at once, each with its sender, in order.
func TestFilteredConnReadBatch(t *testing.T) {
	serverConn, clientConn := udpPair(t)
	mux, err := NewMuxedConn(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()
	var sent [][]byte
	for i := 0; i < 5; i++ {
		p := make([]byte, 100+i)
		_, _ = rand.Read(p)
		p[0] = 0x40
		sent = append(sent, p)
		if _, err := clientConn.WriteTo(p, serverConn.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	q := mux.GetQUICConn().(OOBPacketConn)
	q.SetReadDeadline(time.Now().Add(2 * time.Second))
	ms := newBatch(8, 1500)
	var got [][]byte
	for len(got) < len(sent) {
		n, err := q.ReadBatch(ms, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if ms[i].Addr.String() != clientConn.LocalAddr().String() {
				t.Fatalf("sender %v, want %v", ms[i].Addr, clientConn.LocalAddr())
			}
			got = append(got, append([]byte(nil), ms[i].Buffers[0][:ms[i].N]...))
		}
	}
	for i := range sent {
		if !bytes.Equal(got[i], sent[i]) {
			t.Fatalf("datagram %d differs", i)
		}
	}
}
