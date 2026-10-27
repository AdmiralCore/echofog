package swarm

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

func TestMailboxHintsAreSignedByTheirOwner(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var pk types.PeerPublicKey
	copy(pk[:], pub)
	hints := []MailboxHint{{Peer: strings.Repeat("ab", 32), Addr: "203.0.113.5:4242", Until: time.Now().Add(time.Hour).Unix()}}
	sig := ed25519.Sign(priv, mailboxSigned(hints))
	if !verifyMailboxes(pk, hints, sig) {
		t.Fatal("the owner's signature does not verify")
	}
	// somebody passing the record on points the mail elsewhere: refused
	forged := []MailboxHint{{Peer: strings.Repeat("cd", 32), Until: hints[0].Until}}
	if verifyMailboxes(pk, forged, sig) {
		t.Fatal("changed hints verified")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	var opk types.PeerPublicKey
	copy(opk[:], other)
	if verifyMailboxes(opk, hints, sig) {
		t.Fatal("another key verified the hints")
	}
}

func TestStaleAndBadHintsAreDropped(t *testing.T) {
	now := time.Now()
	good := strings.Repeat("ab", 32)
	in := []MailboxHint{
		{Peer: good, Until: now.Add(time.Hour).Unix()},
		{Peer: good, Until: now.Add(-time.Minute).Unix()},        // stale
		{Peer: "not-hex", Until: now.Add(time.Hour).Unix()},      // bad id
		{Peer: good, Until: now.Add(30 * 24 * time.Hour).Unix()}, // too far out
		{Peer: good, Addr: strings.Repeat("x", 300), Until: now.Add(time.Hour).Unix()},
	}
	if out := cleanHints(in, now); len(out) != 1 {
		t.Fatalf("kept %d hints: %+v", len(out), out)
	}
}
