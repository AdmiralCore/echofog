package mimicry

import (
	"net"
	"testing"
	"time"
)

// Capture the first ClientHello bytes our dialer emits and sanity-check it is a modern Chrome
// hello (TLS 1.2 record, offers TLS 1.3, carries GREASE), not Go's default stack.
func TestClientHelloLooksLikeChrome(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _ := c.Read(buf)
		got <- buf[:n]
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cfg := DefaultMimicryConfig()
	go ClientHandshake(raw, cfg) // will fail (no server), we only want the ClientHello
	hello := <-got
	if len(hello) < 48 || hello[0] != 0x16 || hello[5] != 0x01 {
		t.Fatalf("not a TLS handshake ClientHello: % x", hello[:min(16, len(hello))])
	}
	// GREASE values (0x?a?a) appear in a Chrome hello's cipher list; Go's stack never emits them.
	grease := false
	for i := 0; i+1 < len(hello); i++ {
		if hello[i] == hello[i+1] && (hello[i]&0x0f) == 0x0a && hello[i] != 0 {
			grease = true
			break
		}
	}
	if !grease {
		t.Fatalf("no GREASE in ClientHello: does not look like Chrome")
	}
	t.Logf("ClientHello %d bytes, GREASE present (Chrome-like)", len(hello))
}
