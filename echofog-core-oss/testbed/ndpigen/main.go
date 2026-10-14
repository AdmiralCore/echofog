// Command ndpigen produces traffic for a DPI engine (nDPI) to classify, to check that an EchoFog
// TCP session looks like an ordinary HTTPS visit to the mimicked site and does not reveal the
// inner TLS of a proxied connection ("TLS in TLS").
//
// It runs three flows on loopback, each on its own port so they are separate in the capture:
//   - echofog: the production TCP fallback (REALITY token in the ClientHello, smux, Vision
//     padding) carrying a real inner TLS 1.3 session, as the proxy would carry a user's HTTPS.
//   - control-naive: a plain TLS 1.3 session carrying another TLS 1.3 session inside, with no
//     padding — the textbook "TLS in TLS" an engine should be able to flag.
//   - control-https: a single ordinary TLS 1.3 session (a normal HTTPS visit) for reference.
//
// Capture loopback while this runs (tcpdump -i lo -w out.pcap) and classify with ndpiReader.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/network/mimicry"
	"github.com/echofog/echofog-core/pkg/api/types"
)

const serverName = "www.microsoft.com"

type signer struct{ priv ed25519.PrivateKey }

func newSigner() signer {
	_, p, _ := ed25519.GenerateKey(rand.Reader)
	return signer{p}
}
func (s signer) GetPublicKey() ([]byte, error)     { return s.priv.Public().(ed25519.PublicKey), nil }
func (s signer) SignData(d []byte) ([]byte, error) { return ed25519.Sign(s.priv, d), nil }
func (s signer) id() types.PeerID {
	var k types.PeerPublicKey
	copy(k[:], s.priv.Public().(ed25519.PublicKey))
	return types.PeerPubKeyToID(k)
}

// innerCert / serverTLS for the inner (proxied) TLS session the engine must not see.
func ecdsaCert() tls.Certificate {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "inner.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"inner.example"},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &priv.PublicKey, priv)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// streamConn adapts a MuxStream to net.Conn so a tls.Client/Server can run over it.
type streamConn struct {
	network.MuxStream
	l, r net.Addr
}

func (c streamConn) LocalAddr() net.Addr  { return c.l }
func (c streamConn) RemoteAddr() net.Addr { return c.r }

// pump writes a burst and reads the echo, to produce application records after the handshake.
func pump(conn net.Conn, tag string) {
	msg := []byte("GET / HTTP/1.1\r\nHost: " + serverName + "\r\nUser-Agent: Mozilla/5.0\r\n\r\n")
	for i := 0; i < 8; i++ {
		if _, err := conn.Write(msg); err != nil {
			return
		}
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Read(buf); err != nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	_ = tag
}

func innerServerLoop(ln net.Listener, cfg *tls.Config) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				_, _ = c.Write(buf[:n]) // echo
			}
		}()
	}
}

// echofogFlow: production TCP fallback carrying a real inner TLS session.
func echofogFlow() {
	cert, err := mimicry.GenerateCamouflageCert(serverName)
	if err != nil {
		log.Fatal(err)
	}
	mcfg := network.MimicryConfigFrom(network.MimicryOptions{ServerName: serverName})
	srvSigner, cliSigner := newSigner(), newSigner()

	server := network.NewTCPTransport(cert, mcfg, network.NewDefaultPeerAuthenticator(srvSigner, 101, 5*time.Second))
	server.SetNodeKeys("", srvSigner.id())
	if err := server.Listen("127.0.0.1:0"); err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	addr := server.Addr().String()

	client := network.NewTCPTransport(cert, mcfg, network.NewDefaultPeerAuthenticator(cliSigner, 101, 5*time.Second))
	client.SetNodeKeys("", cliSigner.id())
	defer client.Close()
	client.ExpectPeer(addr, srvSigner.id())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Dial(ctx, addr); err != nil {
		log.Fatalf("echofog dial: %v", err)
	}

	srvEv := <-server.ConnChan()
	cliEv := <-client.ConnChan()

	innerSrvTLS := &tls.Config{Certificates: []tls.Certificate{ecdsaCert()}, MinVersion: tls.VersionTLS13}
	done := make(chan struct{})
	go func() {
		st, err := srvEv.Conn.AcceptStream(ctx)
		if err != nil {
			close(done)
			return
		}
		sc := streamConn{st, srvEv.Conn.LocalAddr(), srvEv.Conn.RemoteAddr()}
		s := tls.Server(sc, innerSrvTLS)
		if err := s.Handshake(); err != nil {
			close(done)
			return
		}
		buf := make([]byte, 4096)
		for {
			n, e := s.Read(buf)
			if e != nil {
				break
			}
			_, _ = s.Write(buf[:n])
		}
		close(done)
	}()

	st, err := cliEv.Conn.OpenStreamSync(ctx)
	if err != nil {
		log.Fatalf("echofog open stream: %v", err)
	}
	cc := streamConn{st, cliEv.Conn.LocalAddr(), cliEv.Conn.RemoteAddr()}
	inner := tls.Client(cc, &tls.Config{ServerName: "inner.example", InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err := inner.Handshake(); err != nil {
		log.Fatalf("echofog inner handshake: %v", err)
	}
	pump(inner, "echofog")
	inner.Close()
	<-done
	log.Println("echofog flow done")
}

// controlNaiveFlow: plain TLS carrying a TLS session inside, no padding — textbook TLS in TLS.
func controlNaiveFlow() {
	outerCert := ecdsaCert()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{outerCert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	innerSrvTLS := &tls.Config{Certificates: []tls.Certificate{ecdsaCert()}, MinVersion: tls.VersionTLS13}
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		s := tls.Server(c, innerSrvTLS)
		if err := s.Handshake(); err != nil {
			return
		}
		buf := make([]byte, 4096)
		for {
			n, e := s.Read(buf)
			if e != nil {
				return
			}
			_, _ = s.Write(buf[:n])
		}
	}()

	outer, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		log.Fatalf("control-naive outer: %v", err)
	}
	defer outer.Close()
	inner := tls.Client(outer, &tls.Config{ServerName: "inner.example", InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err := inner.Handshake(); err != nil {
		log.Fatalf("control-naive inner: %v", err)
	}
	pump(inner, "control-naive")
	log.Println("control-naive flow done")
}

// controlHTTPSFlow: one ordinary TLS session, for reference.
func controlHTTPSFlow() {
	cert := ecdsaCert()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	go innerServerLoop(ln, nil)
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		log.Fatalf("control-https: %v", err)
	}
	defer c.Close()
	pump(c, "control-https")
	log.Println("control-https flow done")
}

func main() {
	fmt.Println("generating traffic flows on loopback...")
	controlHTTPSFlow()
	controlNaiveFlow()
	echofogFlow()
	time.Sleep(500 * time.Millisecond) // let the capture flush
	io.Discard.Write(nil)
	fmt.Println("done")
}
