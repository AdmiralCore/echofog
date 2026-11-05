package noise3

import (
	"errors"
	"net"
	"testing"
	"time"
)

func keyListener(t *testing.T, keys ...MaskKey) (*Listener, *net.UDPConn) {
	t.Helper()
	l, err := Listen("127.0.0.1:0", ListenerConfig{Keys: keys, Profile: mustProfile(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conn, err := net.DialUDP("udp", nil, l.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return l, conn
}

func hasFlow(l *Listener, conn *net.UDPConn) bool {
	deadline := time.Now().Add(300 * time.Millisecond)
	src := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	for time.Now().Before(deadline) {
		if flowAt(l, src) != nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Н-12: SetKeys swaps the live masking keys without recreating the listener.
func TestSetKeysRotates(t *testing.T) {
	oldK, oldPub := newMaskKey(t, time.Now())
	newK, newPub := newMaskKey(t, time.Now())
	l, conn := keyListener(t, oldK)
	if err := l.SetKeys([]MaskKey{newK}); err != nil {
		t.Fatal(err)
	}
	stale, _ := craftInit(t, oldPub, uint32(time.Now().Unix()), 1300)
	_, _ = conn.Write(stale)
	if hasFlow(l, conn) {
		t.Fatal("an Init for a replaced key opened a flow")
	}
	fresh, _ := craftInit(t, newPub, uint32(time.Now().Unix()), 1300)
	_, _ = conn.Write(fresh)
	if !hasFlow(l, conn) {
		t.Fatal("an Init for the new key was not accepted")
	}
}

func TestRevokeStopsAcceptingKey(t *testing.T) {
	k1, pub1 := newMaskKey(t, time.Now())
	k2, pub2 := newMaskKey(t, time.Now())
	l, conn := keyListener(t, k1, k2)
	var p [32]byte
	copy(p[:], pub1)
	if err := l.Revoke(p); err != nil {
		t.Fatal(err)
	}
	gone, _ := craftInit(t, pub1, uint32(time.Now().Unix()), 1300)
	_, _ = conn.Write(gone)
	if hasFlow(l, conn) {
		t.Fatal("an Init for a revoked key opened a flow")
	}
	kept, _ := craftInit(t, pub2, uint32(time.Now().Unix()), 1300)
	_, _ = conn.Write(kept)
	if !hasFlow(l, conn) {
		t.Fatal("the remaining key stopped working")
	}
	copy(p[:], pub2)
	if err := l.Revoke(p); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("revoking the last key = %v, want ErrBadConfig", err)
	}
	var unknown [32]byte
	if err := l.Revoke(unknown); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("revoking an unknown key = %v, want ErrBadConfig", err)
	}
}

func TestSetKeysRejectsBadSets(t *testing.T) {
	k, _ := newMaskKey(t, time.Now())
	l, _ := keyListener(t, k)
	if err := l.SetKeys(nil); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("SetKeys(nil) = %v, want ErrBadConfig", err)
	}
	if err := l.SetKeys(make([]MaskKey, MaxMaskKeys+1)); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("too many keys = %v, want ErrBadConfig", err)
	}
}
