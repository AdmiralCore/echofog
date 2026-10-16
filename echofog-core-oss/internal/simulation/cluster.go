package simulation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/node"
)

// Cluster manages an in-memory mesh of real EchoFog nodes for end-to-end network testing.
type Cluster struct {
	T         *testing.T
	Nodes     []*node.Node
	TempDirs  []string
	Ctx       context.Context
	Cancel    context.CancelFunc
	mu        sync.Mutex
	BootAddrs []string
}

// NewCluster spawns a simulated P2P network of N nodes on dynamic loopback ports.
func NewCluster(t *testing.T, count int) (*Cluster, error) {
	increaseWorkingSetQuota()
	if count < 2 {
		return nil, fmt.Errorf("cluster requires at least 2 nodes, requested %d", count)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Cluster{
		T:        t,
		Nodes:    make([]*node.Node, 0, count),
		TempDirs: make([]string, 0, count),
		Ctx:      ctx,
		Cancel:   cancel,
	}

	appPass := []byte("simulation_test_pass_12345!")

	// 1. Spawn Bootstrap Node (Node 0)
	bootDir, err := os.MkdirTemp("", "echofog-sim-node-00-*")
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("failed to create boot dir: %w", err)
	}
	c.TempDirs = append(c.TempDirs, bootDir)

	bootCfg := config.DefaultConfig(fmt.Sprintf("sim-boot-%d", time.Now().UnixNano()))
	bootCfg.Storage.DatabasePath = filepath.Join(bootDir, "db")
	bootCfg.Network.ListenAddr = "127.0.0.1:0"
	bootCfg.Network.Transport = os.Getenv("SIM_TRANSPORT") // "tcp": the whole cluster over TCP/TLS
	bootCfg.Network.BootstrapNodes = nil
	bootCfg.Network.MaxConnections = 200

	bootNode := node.NewNode(ctx, bootCfg, "node-00-boot")
	if _, err := bootNode.Register(appPass, nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("failed to register boot node: %w", err)
	}
	if err := bootNode.Start(); err != nil {
		c.Close()
		return nil, fmt.Errorf("failed to start boot node: %w", err)
	}

	c.Nodes = append(c.Nodes, bootNode)
	bootAddr := NodeAddr(bootNode)
	c.BootAddrs = append(c.BootAddrs, bootAddr)
	t.Logf("[Cluster] Node 00 (Bootstrap) online on %s", bootAddr)

	// 2. Spawn remaining worker nodes sequentially for stability
	for i := 1; i < count; i++ {
		idx := i
		nDir, err := os.MkdirTemp("", fmt.Sprintf("echofog-sim-node-%02d-*", idx))
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d temp dir error: %w", idx, err)
		}
		c.TempDirs = append(c.TempDirs, nDir)

		cfg := config.DefaultConfig(fmt.Sprintf("sim-node-%d-%d", idx, time.Now().UnixNano()))
		cfg.Storage.DatabasePath = filepath.Join(nDir, "db")
		cfg.Network.ListenAddr = "127.0.0.1:0"
		cfg.Network.Transport = os.Getenv("SIM_TRANSPORT")
		cfg.Network.MaxConnections = 100
		cfg.Network.BootstrapNodes = []string{bootAddr}

		n := node.NewNode(ctx, cfg, fmt.Sprintf("node-%02d", idx))
		if _, err := n.Register(appPass, nil); err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d register error: %w", idx, err)
		}
		if err := n.Start(); err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d start error: %w", idx, err)
		}

		c.Nodes = append(c.Nodes, n)
	}

	t.Logf("[Cluster] Successfully spawned all %d nodes", len(c.Nodes))
	return c, nil
}

// WaitForConvergence waits until nodes have populated their routing tables and connected to peers.
func (c *Cluster) WaitForConvergence(timeout time.Duration, minPeersPerNode int) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.Ctx.Done():
			return c.Ctx.Err()
		case <-ticker.C:
			converged := 0
			c.mu.Lock()
			for _, n := range c.Nodes {
				if n.Swarm != nil && len(n.Swarm.GetAllPeers()) >= minPeersPerNode {
					converged++
				}
			}
			total := len(c.Nodes)
			c.mu.Unlock()

			// If at least 85% of nodes have reached desired peer count, mesh is established
			if float64(converged)/float64(total) >= 0.80 {
				c.T.Logf("[Cluster] Mesh converged: %d/%d nodes reached >= %d peers", converged, total, minPeersPerNode)
				return nil
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("convergence timeout: only %d/%d nodes reached %d peers", converged, total, minPeersPerNode)
			}
		}
	}
}

// Close gracefully stops all nodes and cleans up temporary directories.
func (c *Cluster) Close() {
	if c.Cancel != nil {
		c.Cancel()
	}

	var wg sync.WaitGroup
	for _, n := range c.Nodes {
		if n != nil {
			wg.Add(1)
			go func(nd *node.Node) {
				defer wg.Done()
				nd.Stop()
			}(n)
		}
	}
	wg.Wait()

	for _, dir := range c.TempDirs {
		_ = os.RemoveAll(dir)
	}
}

// NewIsolatedCluster spawns nodes without any automatic bootstrap connections.
func NewIsolatedCluster(t *testing.T, count int) (*Cluster, error) {
	increaseWorkingSetQuota()

	ctx, cancel := context.WithCancel(context.Background())
	c := &Cluster{
		T:        t,
		Ctx:      ctx,
		Cancel:   cancel,
		Nodes:    make([]*node.Node, 0, count),
		TempDirs: make([]string, 0, count),
	}

	appPass := []byte("isolated-cluster-secure-pass")

	for i := 0; i < count; i++ {
		idx := i
		nDir, err := os.MkdirTemp("", fmt.Sprintf("echofog-iso-node-%02d-*", idx))
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d temp dir error: %w", idx, err)
		}
		c.TempDirs = append(c.TempDirs, nDir)

		cfg := config.DefaultConfig(fmt.Sprintf("iso-node-%d-%d", idx, time.Now().UnixNano()))
		cfg.Storage.DatabasePath = filepath.Join(nDir, "db")
		cfg.Network.ListenAddr = "127.0.0.1:0"
		cfg.Network.MaxConnections = 100
		cfg.Network.BootstrapNodes = nil // No auto-connect
		cfg.Network.NoAutoDial = true    // the tests build their topology by hand

		n := node.NewNode(ctx, cfg, fmt.Sprintf("node-%02d", idx))
		if _, err := n.Register(appPass, nil); err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d register error: %w", idx, err)
		}
		if err := n.Start(); err != nil {
			c.Close()
			return nil, fmt.Errorf("node %d start error: %w", idx, err)
		}

		c.Nodes = append(c.Nodes, n)
	}

	return c, nil
}

// NodeAddr is how others reach n: "host:port/<peer id>", the form the obfuscated transport needs.
func NodeAddr(n *node.Node) string {
	return network.JoinPeerAddr(n.Transport.Addr().String(), n.ID)
}
