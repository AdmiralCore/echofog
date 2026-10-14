package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type mockNotifier struct{}

func (m *mockNotifier) AddPotentialPeer(id types.PeerID, addr string) {}

func TestMDNS_PeerDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var id1, id2 types.PeerID
	id1[0], id2[0] = 1, 2

	notifier := &mockNotifier{}

	alice := NewMDNS(id1, 9001, notifier)
	aliceFoundBob := make(chan bool, 1)
	alice.SetOnPeerFound(func(id types.PeerID, addr string, alias string) {
		if id == id2 {
			t.Logf("Alice found Bob! Addr: %s, Alias: %s", addr, alias)
			aliceFoundBob <- true
		}
	})
	if err := alice.Start(ctx, "Alice"); err != nil {
		t.Fatalf("Alice failed to start: %v", err)
	}

	bob := NewMDNS(id2, 9002, notifier)
	bobFoundAlice := make(chan bool, 1)
	bob.SetOnPeerFound(func(id types.PeerID, addr string, alias string) {
		if id == id1 {
			t.Logf("Bob found Alice! Addr: %s, Alias: %s", addr, alias)
			bobFoundAlice <- true
		}
	})
	if err := bob.Start(ctx, "Bob"); err != nil {
		t.Fatalf("Bob failed to start: %v", err)
	}

	timeout := time.After(5 * time.Second)
	aliceSuccess, bobSuccess := false, false

	for {
		select {
		case <-aliceFoundBob:
			aliceSuccess = true
		case <-bobFoundAlice:
			bobSuccess = true
		case <-timeout:
			t.Fatalf("Timeout waiting for mDNS discovery. Alice success: %v, Bob success: %v", aliceSuccess, bobSuccess)
		}

		if aliceSuccess && bobSuccess {
			t.Log("✅ mDNS bi-directional discovery successful!")
			return
		}
	}
}
