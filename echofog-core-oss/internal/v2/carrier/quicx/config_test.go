package quicx

import (
	"crypto/ed25519"
	"crypto/tls"
	"testing"
	"time"
)

func TestNoise3TLSConfig(t *testing.T) {
	client, server, err := Noise3TLSConfig()
	if err != nil {
		t.Fatalf("Noise3TLSConfig: %v", err)
	}
	for name, cfg := range map[string]*tls.Config{"client": client, "server": server} {
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("%s MinVersion = %x, want TLS 1.3", name, cfg.MinVersion)
		}
		if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "h3" {
			t.Errorf("%s NextProtos = %v, want [h3]", name, cfg.NextProtos)
		}
		if len(cfg.CurvePreferences) != 1 || cfg.CurvePreferences[0] != tls.X25519MLKEM768 {
			t.Errorf("%s CurvePreferences = %v, want [X25519MLKEM768]", name, cfg.CurvePreferences)
		}
	}
	if !client.InsecureSkipVerify {
		t.Error("client does not skip certificate verification (wire.md §5.1: the Noise handshake authenticates)")
	}
	if len(server.Certificates) != 1 {
		t.Fatalf("server has %d certificates, want 1", len(server.Certificates))
	}

	leaf := server.Certificates[0].Leaf
	if leaf == nil {
		t.Fatal("server certificate has no parsed Leaf")
	}
	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		t.Errorf("public key is %T, want ed25519.PublicKey", leaf.PublicKey)
	}
	if len(leaf.Subject.Names) != 0 || leaf.Subject.CommonName != "" {
		t.Errorf("subject = %v, want empty", leaf.Subject)
	}
	if leaf.SerialNumber.Sign() == 0 {
		t.Error("serial number is zero")
	}

	// "random serial" (wire.md §5.1): a fresh config does not reuse it.
	_, server2, err := Noise3TLSConfig()
	if err != nil {
		t.Fatalf("second Noise3TLSConfig: %v", err)
	}
	if leaf.SerialNumber.Cmp(server2.Certificates[0].Leaf.SerialNumber) == 0 {
		t.Error("two certificates share a serial number")
	}
}

func TestNoise3QUICConfig(t *testing.T) {
	const pad = 48
	cfg, err := Noise3QUICConfig(pad)
	if err != nil {
		t.Fatalf("Noise3QUICConfig: %v", err)
	}
	inner := PathMTU - Noise3DataOverhead - pad

	if cfg.MaxIdleTimeout != 60*time.Second {
		t.Errorf("MaxIdleTimeout = %v, want 60s", cfg.MaxIdleTimeout)
	}
	if cfg.KeepAlivePeriod != 0 {
		t.Errorf("KeepAlivePeriod = %v, want 0 (L2 PING keeps the path alive)", cfg.KeepAlivePeriod)
	}
	if !cfg.EnableDatagrams {
		t.Error("datagrams are disabled")
	}
	if cfg.Allow0RTT {
		t.Error("0-RTT is allowed")
	}
	if cfg.MaxIncomingStreams != 256 {
		t.Errorf("MaxIncomingStreams = %d, want 256", cfg.MaxIncomingStreams)
	}
	if cfg.MaxIncomingUniStreams != -1 {
		t.Errorf("MaxIncomingUniStreams = %d, want -1 (no unidirectional streams)", cfg.MaxIncomingUniStreams)
	}
	if cfg.InitialPacketSize != uint16(inner) {
		t.Errorf("InitialPacketSize = %d, want %d", cfg.InitialPacketSize, inner)
	}
	if cfg.MaxUDPPayloadSize != uint16(inner) {
		t.Errorf("MaxUDPPayloadSize = %d, want %d", cfg.MaxUDPPayloadSize, inner)
	}
	if !cfg.DisablePathMTUDiscovery {
		t.Error("path MTU discovery is enabled: it would grow packets past the L0 budget")
	}
	if cfg.MaxDatagramFrameSize != int64(inner) {
		t.Errorf("MaxDatagramFrameSize = %d, want %d", cfg.MaxDatagramFrameSize, inner)
	}
}

func TestNoise3QUICConfigBounds(t *testing.T) {
	if _, err := Noise3QUICConfig(PathMTU - Noise3DataOverhead - 1200); err != nil {
		t.Errorf("a profile leaving exactly 1200 bytes: %v", err)
	}
	if _, err := Noise3QUICConfig(PathMTU - Noise3DataOverhead - 1200 + 1); err == nil {
		t.Error("a profile leaving less than a QUIC Initial was accepted")
	}
	if _, err := Noise3QUICConfig(-1); err == nil {
		t.Error("a negative padding budget was accepted")
	}
}

func TestReality2TLSConfig(t *testing.T) {
	cert := testServerCert(t)
	for _, group := range []tls.CurveID{tls.X25519, tls.X25519MLKEM768} {
		client, server := Reality2TLSConfig(group, cert)
		for name, cfg := range map[string]*tls.Config{"client": client, "server": server} {
			if cfg.MinVersion != tls.VersionTLS13 {
				t.Errorf("group %d: %s MinVersion = %x, want TLS 1.3", group, name, cfg.MinVersion)
			}
			if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "h3" {
				t.Errorf("group %d: %s NextProtos = %v, want [h3]", group, name, cfg.NextProtos)
			}
			if len(cfg.CurvePreferences) != 1 || cfg.CurvePreferences[0] != group {
				t.Errorf("group %d: %s CurvePreferences = %v", group, name, cfg.CurvePreferences)
			}
		}
		if !client.InsecureSkipVerify {
			t.Errorf("group %d: client verifies the cover's certificate", group)
		}
		if len(server.Certificates) != 1 {
			t.Errorf("group %d: server has %d certificates, want 1", group, len(server.Certificates))
		}
	}
}

func TestReality2TLSConfigWithoutGroup(t *testing.T) {
	client, server := Reality2TLSConfig(0, nil)
	if len(client.CurvePreferences) != 0 || len(server.CurvePreferences) != 0 {
		t.Error("group 0 restricted the curve preferences")
	}
	if len(server.Certificates) != 0 {
		t.Error("a nil server certificate was installed")
	}
}

func TestReality2QUICConfig(t *testing.T) {
	cfg := Reality2QUICConfig()
	if !cfg.ChromeParrot {
		t.Error("ChromeParrot is off: the cover mirrors Chrome")
	}
	if !cfg.EnableDatagrams {
		t.Error("datagrams are disabled")
	}
}
