//go:build windows

package osprotect

import (
	"bytes"
	"testing"
)

// A secret comes back for the same user and entropy, and only for that entropy; a damaged blob
// does not open.
func TestDPAPIRoundTrip(t *testing.T) {
	p := System()
	if p == nil {
		t.Fatal("Windows has a protector")
	}
	secret := bytes.Repeat([]byte{0xA5}, 37)
	entropy := []byte("echofog test entropy")

	blob, err := p.Protect(secret, entropy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, secret) {
		t.Fatal("the blob holds the secret in the clear")
	}
	got, err := p.Unprotect(blob, entropy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("got %x, want %x", got, secret)
	}

	if _, err := p.Unprotect(blob, []byte("another profile")); err == nil {
		t.Fatal("the blob opened with another entropy")
	}
	if _, err := p.Unprotect(blob, nil); err == nil {
		t.Fatal("the blob opened without its entropy")
	}
	damaged := append([]byte(nil), blob...)
	damaged[len(damaged)-5] ^= 0xFF
	if _, err := p.Unprotect(damaged, entropy); err == nil {
		t.Fatal("a damaged blob opened")
	}
	if _, err := p.Unprotect([]byte("not a blob"), entropy); err == nil {
		t.Fatal("garbage opened")
	}
}
