package main

import (
	"context"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The D1 control must be exactly the traffic GFW's FET rule catches: four set bits per byte on
// average (not 3.3, which OR-ing four random positions gives), no printable runs, no TLS/HTTP.
func TestEncryptedPayloadHasFourDistinctBits(t *testing.T) {
	p := encryptedPayload(600, seeded(42))
	ones, printable, run, longest := 0, 0, 0, 0
	for _, c := range p {
		ones += bits.OnesCount8(c)
		if c >= 0x20 && c <= 0x7e {
			printable++
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	avg := float64(ones) / float64(len(p))
	if avg <= 3.4 || avg >= 4.6 {
		t.Errorf("bits per byte %.2f: the FET rule exempts it", avg)
	}
	if printable > 0 {
		t.Errorf("%d printable bytes: the FET rule exempts text", printable)
	}
	if longest > 20 {
		t.Errorf("a printable run of %d", longest)
	}
}

func TestNoise3KeyIsDeterministicPerSeed(t *testing.T) {
	a, pa := noise3Key(7)
	b, pb := noise3Key(7)
	c, pc := noise3Key(8)
	if a != b || string(pa) != string(pb) {
		t.Error("the same seed must give the same key on both ends")
	}
	if a == c || string(pa) == string(pc) {
		t.Error("another seed must give another key")
	}
	if len(pa) != 32 {
		t.Errorf("public key is %d bytes", len(pa))
	}
}

// The noise3 mode runs the real carrier: a UDP listener and dialer from internal/v2/carrier/noise3,
// not a stand-in. The result says so, and bytes cross in both directions.
func TestNoise3ModeRunsTheRealCarrierBothWays(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := filepath.Join(t.TempDir(), "ready")
	done := make(chan error, 1)
	go func() { done <- noise3Serve(ctx, pc, 42, ready) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the listener did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, err := noise3Client(pc.LocalAddr().String(), 42, 700*time.Millisecond)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if res.Synthetic {
		t.Error("the real carrier is not synthetic")
	}
	if res.Carrier != "noise3" || res.Mode != "noise3" {
		t.Errorf("result names %q / %q", res.Mode, res.Carrier)
	}
	if res.Bytes < 1000 || res.Packets < 1 || res.Connections != 1 {
		t.Errorf("nothing crossed: %+v", res)
	}
	if res.EchoedBytes != res.Bytes {
		t.Errorf("sent %d, echoed %d", res.Bytes, res.EchoedBytes)
	}
}

func TestNoise3ClientWithTheWrongSeedDoesNotConnect(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- noise3Serve(ctx, pc, 1, "") }()
	defer func() {
		cancel()
		<-done
	}()
	_, err = noise3Client(pc.LocalAddr().String(), 2, 300*time.Millisecond)
	if err == nil {
		t.Error("a client with another key got a session: the listener must stay silent")
	}
}
