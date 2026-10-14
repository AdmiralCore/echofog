package mimicry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Our server, built the way the node builds it, answers a Chrome-like ClientHello with the same
// TLS version, cipher and key_share group as a real Go TLS site (which is what big sites running
// current TLS stacks do), so the passive ServerHello fingerprint matches.
func TestServerHelloConformanceMatchesARealSite(t *testing.T) {
	site := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	dest := strings.TrimPrefix(site.URL, "https://")

	cert := generateTestCertificate(t)
	ours := BuildServerTLSConfig(cert)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	verdict, ok := CheckServerHelloConformance(ctx, ours, dest, "www.microsoft.com")
	if !ok {
		t.Fatalf("conformance failed: %s", verdict)
	}
	t.Log(verdict)
}

// The aligned server negotiates the post-quantum hybrid group with a Chrome-like client, as modern
// big sites do (so the key_share group in the ServerHello is not a tell).
func TestServerNegotiatesPostQuantumGroup(t *testing.T) {
	cert := generateTestCertificate(t)
	p, err := OwnServerProfile(BuildServerTLSConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	if p.Group != 4588 { // X25519MLKEM768
		t.Fatalf("server chose group %d, want X25519MLKEM768 (4588)", p.Group)
	}
}
