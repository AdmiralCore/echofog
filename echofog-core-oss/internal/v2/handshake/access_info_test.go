package handshake

import (
	"bytes"
	"testing"
)

// TestResponderReportsAccessPass covers the integration point of T3.6: the responder tells the
// caller which invitation or personal pass admitted the session, so the runtime can redeem an
// invitation or renew a contact's pass.
func TestResponderReportsAccessPass(t *testing.T) {
	w, secret := hiddenWorld(t)
	_, sr := w.handshake(t)
	if sr.Access == nil {
		t.Fatal("no access info on an invitation session")
	}
	if sr.Access.Personal {
		t.Fatal("an invitation pass is marked personal")
	}
	if !bytes.Equal(sr.Access.PassID[:], PassID(secret)) {
		t.Fatalf("pass id %x", sr.Access.PassID)
	}

	w, secret = hiddenWorld(t)
	w.rc.Passes = personalPasses{string(PassID(secret)): secret}
	_, sr = w.handshake(t)
	if sr.Access == nil || !sr.Access.Personal || !bytes.Equal(sr.Access.PassID[:], PassID(secret)) {
		t.Fatalf("personal access info %+v", sr.Access)
	}

	// A public node never reports access: there is no access proof on the wire.
	w = newWorld(t)
	_, sr = w.handshake(t)
	if sr.Access != nil {
		t.Fatalf("public session reports %+v", sr.Access)
	}
}
