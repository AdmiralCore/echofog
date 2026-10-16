package simulation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
)

func startSimNode(t *testing.T, ctx context.Context, name string, bootstrap []string) *node.Node {
	t.Helper()
	dir, err := os.MkdirTemp("", "echofog-seeds-"+name+"-*")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig(fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
	cfg.Storage.DatabasePath = filepath.Join(dir, "db")
	cfg.Network.ListenAddr = "127.0.0.1:0"
	cfg.Network.BootstrapNodes = bootstrap
	n := node.NewNode(ctx, cfg, name)
	if _, err := n.Register([]byte("seeds-simulation-pass"), nil); err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		n.Stop()
		_ = os.RemoveAll(dir)
	})
	return n
}

// Two groups of seeds, each bootstrapped only from its own group, know nothing of each other.
// A client that bootstraps from one seed of each group introduces them (peer exchange): the
// seeds connect, and each keeps the other in its peer store and routing table.
func TestSeedsFromDisjointSetsMeetThroughAClient(t *testing.T) {
	increaseWorkingSetQuota()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a1 := startSimNode(t, ctx, "seed-a1", nil)
	a2 := startSimNode(t, ctx, "seed-a2", []string{NodeAddr(a1)})
	b1 := startSimNode(t, ctx, "seed-b1", nil)
	b2 := startSimNode(t, ctx, "seed-b2", []string{NodeAddr(b1)})

	waitFor := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out: %s", what)
	}
	waitFor("each group forms", 10*time.Second, func() bool {
		return a1.Swarm.GetPeer(a2.ID) != nil && b1.Swarm.GetPeer(b2.ID) != nil
	})
	for _, a := range []*node.Node{a1, a2} {
		for _, b := range []*node.Node{b1, b2} {
			if a.Swarm.GetPeer(b.ID) != nil {
				t.Fatal("setup: the groups must start apart")
			}
		}
	}

	client := startSimNode(t, ctx, "client", []string{NodeAddr(a1), NodeAddr(b1)})
	waitFor("the client joins both groups", 10*time.Second, func() bool {
		return client.Swarm.GetPeer(a1.ID) != nil && client.Swarm.GetPeer(b1.ID) != nil
	})

	waitFor("seed a1 and seed b1 connect", 20*time.Second, func() bool {
		return a1.Swarm.GetPeer(b1.ID) != nil && b1.Swarm.GetPeer(a1.ID) != nil
	})
	for _, pair := range [][2]*node.Node{{a1, b1}, {b1, a1}} {
		from, to := pair[0], pair[1]
		if _, err := from.Swarm.GetPeerStore().GetPeer(to.ID); err != nil {
			t.Fatalf("%s did not store %s for later: %v", from.Cfg.Network.ListenAddr, to.ID, err)
		}
		if !slices.Contains(from.DhtEngine.KnownPeers(), to.ID) {
			t.Fatal("the other seed is in the routing table")
		}
	}
}
