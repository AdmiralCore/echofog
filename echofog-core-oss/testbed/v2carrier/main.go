// Command v2carrier is the harness endpoint for a v2 carrier at the launchpad. It is a separate
// process and a separate binary, as T15.0a requires: the runner starts it inside the test bed,
// records what crosses a gateway, and feeds the same testbed/censor detectors.
//
// The v2 branch does not have carrier/noise3 yet (T2.4 is not merged, T0.3 is not built), so the
// udp-noise mode is an explicitly synthetic stand-in with the shape noise3 will have. When the real
// carrier lands, it replaces the body of that mode; the capture, budget, labels and detectors do
// not change.
//
//	go run ./testbed/v2carrier -mode serve -listen :4343 -ready /tmp/ready
//	go run ./testbed/v2carrier -mode udp-noise -peer 203.0.113.15:4343 -duration 5s
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	mrand "math/rand"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/apernet/quic-go"
	utls "github.com/refraction-networking/utls"
)

type result struct {
	Mode        string `json:"mode"`
	Carrier     string `json:"carrier"`
	Synthetic   bool   `json:"synthetic"`
	Bytes       int64  `json:"bytes"`
	Packets     int64  `json:"packets"`
	Connections int    `json:"connections"`
	EchoedBytes int64  `json:"echoed_bytes,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
}

func main() {
	mode := flag.String("mode", "serve", "serve, udp-noise, tcp-random, tls-go, tls-chrome, quic-go or quic-chrome")
	peer := flag.String("peer", "", "host:port of the server")
	listen := flag.String("listen", ":4343", "serve: address to listen on")
	ready := flag.String("ready", "", "serve: write this file once both listeners are up")
	sni := flag.String("sni", "cover.lab", "SNI to send")
	duration := flag.Duration("duration", 5*time.Second, "how long a client mode runs")
	conns := flag.Int("conns", 5, "tcp-random: how many connections")
	seed := flag.Int64("seed", 0, "seed for the synthetic payload")
	flag.Parse()

	start := time.Now()
	var res result
	var err error
	switch *mode {
	case "serve":
		err = serve(*listen, *ready)
		res = result{Mode: *mode, Carrier: "v2-harness"}
	case "udp-noise":
		res, err = udpNoise(*peer, *duration, *seed)
	case "tcp-random":
		res, err = tcpRandom(*peer, *conns, *seed)
	case "tls-go":
		res, err = tlsGo(*peer, *sni)
	case "tls-chrome":
		res, err = tlsChrome(*peer, *sni)
	case "quic-go":
		res, err = quicDial(*peer, *sni, false, *duration)
	case "quic-chrome":
		res, err = quicDial(*peer, *sni, true, *duration)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	res.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
	if err != nil {
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------- server

func serve(addr, ready string) error {
	cert, err := selfSigned()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	defer ln.Close()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	defer pc.Close()

	var tcpConns, udpPackets atomic.Int64
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			tcpConns.Add(1)
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 4096)
				_, _ = io.ReadFull(conn, buf)
				_, _ = conn.Write([]byte("ok"))
			}()
		}
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			udpPackets.Add(1)
			_, _ = pc.WriteTo(buf[:min(n, 64)], from)
		}
	}()
	if ready != "" {
		_ = os.WriteFile(ready, []byte(fmt.Sprintf("tcp=%d\n", tcpConns.Load())), 0o644)
	}
	<-ctx.Done()
	return nil
}

// ---------------------------------------------------------------------------- carrier stand-in

// udpNoise sends datagrams with the shape noise3 has on the wire: a 1200-byte first datagram,
// a QUIC v1 long header, and a body that does not open with QUIC's Initial keys. It is synthetic
// until T2.4/T0.3 land; the report says so.
func udpNoise(peer string, d time.Duration, seed int64) (result, error) {
	res := result{Mode: "udp-noise", Carrier: "noise3-synthetic", Synthetic: true}
	addr, err := net.ResolveUDPAddr("udp", peer)
	if err != nil {
		return res, err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	end := time.Now().Add(d)
	rng := seeded(seed)
	buf := make([]byte, 1200)
	for time.Now().Before(end) {
		_, _ = crand.Read(buf)
		buf[0] = 0xc0 | byte(rng.Intn(16)) // long header, random packet number length
		buf[1], buf[2], buf[3], buf[4] = 0, 0, 0, 1
		buf[5] = 8 // DCID length
		if _, err := conn.Write(buf); err != nil {
			return res, err
		}
		res.Packets++
		res.Bytes += int64(len(buf))
		time.Sleep(2 * time.Millisecond)
	}
	res.Connections = 1
	return res, nil
}

// tcpRandom sends the "fully encrypted" first payload that D1 exists for: exactly four set bits
// per byte, no printable runs, no TLS or HTTP header.
func tcpRandom(peer string, conns int, seed int64) (result, error) {
	res := result{Mode: "tcp-random", Carrier: "d1-control", Synthetic: true}
	if conns <= 0 {
		conns = 5
	}
	rng := seeded(seed)
	for i := 0; i < conns; i++ {
		conn, err := net.DialTimeout("tcp", peer, 3*time.Second)
		if err != nil {
			return res, err
		}
		payload := encryptedPayload(600, rng)
		_, _ = conn.Write(payload)
		res.Bytes += int64(len(payload))
		res.Packets++
		res.Connections++
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return res, nil
}

// ---------------------------------------------------------------------------- honest controls

func tlsGo(peer, sni string) (result, error) {
	res := result{Mode: "tls-go", Carrier: "tool-control"}
	conn, err := tls.Dial("tcp", peer, &tls.Config{ // #nosec G402 -- the lab server is self-signed
		ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"h2"},
	})
	if err != nil {
		return res, err
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hello"))
	res.Bytes, res.Connections = 5, 1
	return res, nil
}

func tlsChrome(peer, sni string) (result, error) {
	res := result{Mode: "tls-chrome", Carrier: "utls-chrome"}
	raw, err := net.DialTimeout("tcp", peer, 3*time.Second)
	if err != nil {
		return res, err
	}
	defer raw.Close()
	u := utls.UClient(raw, &utls.Config{ // #nosec G402 -- the lab server is self-signed
		ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"},
	}, utls.HelloChrome_Auto)
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := u.Handshake(); err != nil {
		return res, err
	}
	res.Connections = 1
	return res, nil
}

func quicDial(peer, sni string, chromeParrot bool, d time.Duration) (result, error) {
	res := result{Mode: "quic-go", Carrier: "quic-go"}
	if chromeParrot {
		res.Mode, res.Carrier = "quic-chrome", "quic-go-chromeparrot"
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := quic.DialAddr(ctx, peer, &tls.Config{ // #nosec G402 -- the lab server is self-signed
		ServerName: sni, InsecureSkipVerify: true, NextProtos: []string{"h3"},
	}, &quic.Config{ChromeParrot: chromeParrot})
	if err != nil && ctx.Err() == nil {
		return res, err
	}
	// The handshake does not finish against a sink; the Initial is on the wire, which is all a
	// censor (and this harness) needs.
	res.Connections = 1
	return res, nil
}

// ---------------------------------------------------------------------------- helpers

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cover.lab"},
		DNSNames:  []string{"cover.lab", "foreign.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func encryptedPayload(n int, rng *mrand.Rand) []byte {
	out := make([]byte, n)
	for i := range out {
		var b byte
		for {
			b = 0
			// four *distinct* bits: OR-ing four random positions averages 3.31 set bits,
			// which the FET rule exempts as "not near half ones per byte"
			for _, k := range rng.Perm(8)[:4] {
				b |= 1 << k
			}
			// keep it non-printable: the FET rule exempts text
			if b < 0x20 || b > 0x7e {
				break
			}
		}
		out[i] = b
	}
	return out
}

func seeded(seed int64) *mrand.Rand {
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return mrand.New(mrand.NewSource(seed))
}
