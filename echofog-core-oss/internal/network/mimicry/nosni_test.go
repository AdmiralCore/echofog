package mimicry

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// helloExtensions lists the extension types of a ClientHello record.
func helloExtensions(t *testing.T, rec []byte) map[uint16]bool {
	t.Helper()
	m := rec[5+4:] // record header, handshake header
	m = m[2+32:]   // version, random
	m = m[1+int(m[0]):]
	m = m[2+int(binary.BigEndian.Uint16(m)):]
	m = m[1+int(m[0]):]
	ext := m[2 : 2+int(binary.BigEndian.Uint16(m))]
	out := map[uint16]bool{}
	for len(ext) >= 4 {
		typ, n := binary.BigEndian.Uint16(ext), int(binary.BigEndian.Uint16(ext[2:]))
		out[typ] = true
		ext = ext[4+n:]
	}
	return out
}

// A dial to a peer whose cover site is unknown names no site at all, the way a browser asks an
// IP address: no made-up www.microsoft.com on a residential or VPS address. The peer, old or
// new, accepts it by the token alone.
func TestNoSNIWithoutAKnownCover(t *testing.T) {
	cfg := DefaultMimicryConfig()
	if cfg.ServerName != "" {
		t.Fatalf("the default config names a cover site: %q", cfg.ServerName)
	}
	cfg.AuthKey = NodeStealthKey("", [32]byte{7})
	cfg.InsecureSkipVerify = true

	srv, cli := net.Pipe()
	go func() { _, _, _ = ClientHandshake(cli, cfg) }()
	rec, ok := readClientHelloRecord(srv)
	_ = srv.Close()
	_ = cli.Close()
	if !ok {
		t.Fatal("did not read a ClientHello record")
	}
	if bytes.Contains(rec, []byte("microsoft")) {
		t.Fatal("the ClientHello names www.microsoft.com")
	}
	if helloExtensions(t, rec)[0] {
		t.Fatal("the ClientHello carries a server_name extension though no cover site is known")
	}

	// the parser above does see the extension when a cover is known
	named := *cfg
	named.ServerName = "cover.example"
	srv2, cli2 := net.Pipe()
	go func() { _, _, _ = ClientHandshake(cli2, &named) }()
	rec2, ok := readClientHelloRecord(srv2)
	_ = srv2.Close()
	_ = cli2.Close()
	if !ok || !helloExtensions(t, rec2)[0] {
		t.Fatal("a hello to a known cover has no server_name extension")
	}

	// and a full handshake still goes through
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan bool, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			done <- false
			return
		}
		defer raw.Close()
		_, isPeer, _, err := ServerHandshake(raw, BuildServerTLSConfig(generateTestCertificate(t)), cfg)
		done <- err == nil && isPeer
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ClientHandshake(raw, cfg); err != nil {
		t.Fatalf("handshake without SNI: %v", err)
	}
	if !<-done {
		t.Fatal("the server did not take the SNI-less hello as a peer")
	}
}
