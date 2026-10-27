package chatlogtest

import (
	"sync"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/stretchr/testify/require"
)

// online says whether api lists the contact peer as online.
func online(t *testing.T, api *node.NodeApi, peer string) bool {
	t.Helper()
	contacts, err := api.GetContacts()
	require.NoError(t, err)
	for _, c := range contacts {
		if c.PeerID == peer {
			return c.IsOnline
		}
	}
	t.Fatalf("%s is not a contact", peer[:12])
	return false
}

// A contact is online while the swarm holds a connection to it, and the UI hears when that
// changes: online once two contacts connect, offline once one of them stops.
func TestContactsShowWhoIsOnline(t *testing.T) {
	aliceN, alice := startNode(t, "alice")
	bobN, bob := startNode(t, "bob")
	aliceID, bobID := aliceN.ID.String(), bobN.ID.String()
	require.NoError(t, alice.NewContact(dto.ContactDTO{PeerID: bobID, PubKey: bob.GetMyPubKey(), Alias: "Bob"}))
	require.NoError(t, bob.NewContact(dto.ContactDTO{PeerID: aliceID, PubKey: alice.GetMyPubKey(), Alias: "Alice"}))
	require.False(t, online(t, alice, bobID), "not connected yet")

	var mu sync.Mutex
	last := map[string]bool{} // what alice's UI was last told, by peer
	stop := alice.OnPeerState(func(ev node.PeerStateDTO) {
		mu.Lock()
		last[ev.PeerID] = ev.Online
		mu.Unlock()
	})
	defer stop()
	told := func(peer string, want bool) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			got, ok := last[peer]
			return ok && got == want
		}
	}

	require.NoError(t, bob.ConnectToPeer(joinOf(aliceN)))
	eventually(t, "alice sees bob online", func() bool { return online(t, alice, bobID) })
	eventually(t, "bob sees alice online", func() bool { return online(t, bob, aliceID) })
	eventually(t, "alice's UI hears bob came online", told(bobID, true))

	bobN.Stop()
	eventually(t, "alice sees bob offline", func() bool { return !online(t, alice, bobID) })
	eventually(t, "alice's UI hears bob went offline", told(bobID, false))
}
