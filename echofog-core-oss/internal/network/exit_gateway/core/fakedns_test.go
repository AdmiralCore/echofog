package core

import (
	"testing"

	"github.com/echofog/echofog-core/internal/network/fakedns"
)

// core does not import fakedns (the TUN package); the duplicated default address must still match
// it, and a typed-nil mapper must switch interception off as the concrete pointer field did.
func TestFakeDNSDefaultsAndTypedNil(t *testing.T) {
	if !DefaultFakeDNSAddr.Equal(fakedns.DNSAddr) {
		t.Fatalf("DefaultFakeDNSAddr %v does not match fakedns.DNSAddr %v", DefaultFakeDNSAddr, fakedns.DNSAddr)
	}
	s := NewSOCKS5Server("127.0.0.1:0", nil)
	s.SetFakeDNS((*fakedns.Mapper)(nil))
	if s.fakeDNS() != nil {
		t.Fatal("a typed-nil mapper must turn fake DNS off")
	}
}
