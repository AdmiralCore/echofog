package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestRealCarriersCarryPages starts the lab servers of the three real carriers on loopback and
// loads pages through each with the lab client: the modes the live run uses work end to end.
func TestRealCarriersCarryPages(t *testing.T) {
	cover, err := labCert("cover.lab")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cover}})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	go func() {
		for {
			c, err := cl.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = c.Write([]byte("cover")) }()
		}
	}()

	for _, suite := range []string{suiteNoise3, suiteR2TCP, suiteWS2, suiteR2QUIC} {
		t.Run(suite, func(t *testing.T) {
			dir := t.TempDir()
			network := "tcp"
			if suite == suiteR2QUIC || suite == suiteNoise3 {
				network = "udp"
			}
			port := freePort(t, network)
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			advertise := addr
			if suite == suiteWS2 {
				advertise = ""
			}
			ca := filepath.Join(dir, "ca.pem")
			ready := filepath.Join(dir, "ready")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- serveReal(ctx, realServe{suite: suite, listen: addr, advertise: advertise, cover: cl.Addr().String(), seed: 7, ready: ready, caOut: ca, refListen: fmt.Sprintf("127.0.0.1:%d", freePort(t, "tcp"))})
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the server did not become ready")
				}
				select {
				case err := <-done:
					t.Fatalf("the server stopped: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
			}
			res, err := realClient(suite, addr, ca, 7, 2, 3*time.Second)
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			if res.Connections != 3 || res.Bytes == 0 {
				t.Fatalf("result %+v: want 3 connections and some bytes", res)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the server did not stop")
			}
		})
	}
}
