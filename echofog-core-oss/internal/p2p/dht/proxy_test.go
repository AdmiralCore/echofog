package dht

import (
	"testing"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
)

// A user's node that is connected only to its guard — it dials nobody else — finds a record held
// by a server it has never heard of: the guard walks the DHT for it.
func TestGuardLooksUpForAUser(t *testing.T) {
	guard := newTestNodeRole(t, config.RoleServer)
	holder := newTestNodeRole(t, config.RoleServer)
	user := newTestNode(t)

	key := swarm.BundleKey(holder.id)
	if err := holder.dht.Set(key, []byte("card")); err != nil { // nobody is connected: held here only
		t.Fatal(err)
	}
	guard.dial(t, holder)
	user.dial(t, guard)

	if _, ok := guard.dht.localRecord(key); ok {
		t.Fatal("the guard holds the record itself; the test would prove nothing")
	}
	v, err := user.dht.GetFresh(key)
	if err != nil {
		t.Fatalf("GetFresh through the guard: %v", err)
	}
	if string(v) != "card" {
		t.Fatalf("value = %q", v)
	}
}

// Without a server in the chain the same lookup fails: this is what the proxy adds.
func TestPlainNodeDoesNotLookUpForAUser(t *testing.T) {
	relay := newTestNode(t) // not in the server role
	holder := newTestNode(t)
	user := newTestNode(t)
	key := swarm.BundleKey(holder.id)
	if err := holder.dht.Set(key, []byte("card")); err != nil {
		t.Fatal(err)
	}
	relay.dial(t, holder)
	user.dial(t, relay)
	if _, err := user.dht.GetFresh(key); err == nil {
		t.Fatal("a user's node reached a record two hops away without dialing: the test is not isolating the proxy")
	}
}

// Servers answer the same way to each other: no proxy for a server asking.
func TestServerAskingIsAnsweredLocally(t *testing.T) {
	a := newTestNodeRole(t, config.RoleServer)
	b := newTestNodeRole(t, config.RoleServer)
	a.dial(t, b)
	if b.sw.IsUserPeer(a.id) {
		t.Fatal("a server must not count as a user's node")
	}
}
