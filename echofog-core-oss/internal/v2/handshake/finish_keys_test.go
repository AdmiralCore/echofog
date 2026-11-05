package handshake

import (
	"bytes"
	"errors"
	"testing"
)

// TestHandleFinishSessionKeysOnCardRejection pins that message 3 is authenticated before the
// card is checked, so a card rejection can still be reported as a sealed REJECT on the control
// stream (decisions.md, T3.1; handshake §8).
func TestHandleFinishSessionKeysOnCardRejection(t *testing.T) {
	w := newWorld(t)
	w.ic.Card = newTestIdentity(t, 8).card(t, CardOptions{Scope: 1}) // hs_key is not the initiator's
	ini, err := NewInitiator(w.ic, testCB)
	if err != nil {
		t.Fatal(err)
	}
	res := NewResponder(w.rc)
	hello, m1, err := ini.Start()
	if err != nil {
		t.Fatal(err)
	}
	rep := res.HandleFirst(testCB, hello, m1, w.now)
	m3, si, err := ini.HandleMsg2(rep.Payload)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := res.HandleFinish(m3)
	if !errors.Is(err, ErrPeerCard) {
		t.Fatalf("initiator card mismatch: %v", err)
	}
	if sr == nil {
		t.Fatal("card rejection discarded the session keys")
	}
	if sr.Peer != nil {
		t.Fatal("peer card set on a rejected card")
	}
	if !bytes.Equal(sr.SID, si.SID) || !bytes.Equal(sr.SessionSecret, si.SessionSecret) ||
		!bytes.Equal(sr.KI2R.Key(), si.KI2R.Key()) || !bytes.Equal(sr.KR2I.Key(), si.KR2I.Key()) {
		t.Fatal("rejected-card session keys differ from the initiator's")
	}
}
