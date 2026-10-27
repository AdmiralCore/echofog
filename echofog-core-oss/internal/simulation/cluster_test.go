package simulation

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"github.com/echofog/echofog-core/pkg/node"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/dht"
	"github.com/echofog/echofog-core/internal/p2p/vpnmarket"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
)

func getClusterNodeCount(defaultCount int) int {
	if val := os.Getenv("SIM_NODE_COUNT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n >= 2 {
			return n
		}
	}
	return defaultCount
}

// TestCluster_DHTMultiNodeMesh tests multi-node reading and writing across the Kademlia DHT mesh.
func TestCluster_DHTMultiNodeMesh(t *testing.T) {
	if raceBuild {
		t.Skip("cluster simulation skipped under race detector; tested in non-race CI job")
	}
	nodeCount := getClusterNodeCount(15)
	t.Logf("=== Starting %d-Node DHT Network Simulation ===", nodeCount)

	cluster, err := NewCluster(t, nodeCount)
	if err != nil {
		t.Fatalf("Failed to initialize cluster: %v", err)
	}
	defer cluster.Close()

	// Wait for network mesh to converge (bootstrap + peer discovery)
	t.Log("[Cluster] Waiting for Kademlia routing tables to converge...")
	if err := cluster.WaitForConvergence(10*time.Second, 1); err != nil {
		t.Logf("Convergence warning: %v (continuing test with available peers)", err)
	}

	// 1. Single Node Store -> Remote Node Get
	writerNode := cluster.Nodes[1]
	readerNode := cluster.Nodes[nodeCount-1]

	// a node publishes only under its own keys (an exit advert key here)
	testKey := vpnmarket.Key(writerNode.ID)
	testVal := make([]byte, 1024)
	_, _ = rand.Read(testVal)

	t.Logf("[DHT Test] Writer (%s) setting key in DHT...", writerNode.ID.String()[:8])
	if err := writerNode.DhtEngine.Set(testKey, testVal); err != nil {
		t.Fatalf("Failed to Set key in DHT: %v", err)
	}

	// Small propagation window
	time.Sleep(500 * time.Millisecond)

	t.Logf("[DHT Test] Remote Reader (%s) fetching key from DHT...", readerNode.ID.String()[:8])
	retrievedVal, err := readerNode.DhtEngine.Get(testKey)
	if err != nil {
		t.Fatalf("Failed to Get key from DHT on remote node: %v", err)
	}

	if !bytes.Equal(retrievedVal, testVal) {
		t.Fatalf("Data mismatch: retrieved bytes do not match stored payload")
	}
	t.Log("[DHT Test] SUCCESS: Remote node fetched and validated stored DHT data!")

	// 2. High-concurrency multi-node DHT write & read
	t.Log("=== Testing Concurrent Multi-Node DHT Mesh Writes ===")
	concurrentOps := 15
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentOps)

	for i := 0; i < concurrentOps; i++ {
		opIdx := i
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			srcNode := cluster.Nodes[(idx*2)%nodeCount]
			dstNode := cluster.Nodes[(idx*2+1)%nodeCount]

			key := vpnmarket.Key(srcNode.ID) // every op has its own source node
			val := []byte(fmt.Sprintf("secret_payload_from_node_%d_timestamp_%d", idx, time.Now().UnixNano()))

			if err := srcNode.DhtEngine.Set(key, val); err != nil {
				errCh <- fmt.Errorf("node %d set error: %w", idx, err)
				return
			}

			var got []byte
			var err error
			for attempt := 0; attempt < 8; attempt++ {
				time.Sleep(200 * time.Millisecond)
				got, err = dstNode.DhtEngine.Get(key)
				if err == nil && bytes.Equal(got, val) {
					break
				}
			}

			if err != nil {
				errCh <- fmt.Errorf("node %d get error: %w", idx, err)
				return
			}

			if !bytes.Equal(got, val) {
				errCh <- fmt.Errorf("node %d payload mismatch", idx)
				return
			}
		}(opIdx)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("Concurrent DHT operation failed: %v", err)
		}
	}
	t.Log("[DHT Test] All concurrent multi-node DHT operations passed perfectly!")
}

// TestCluster_NodeChurnResilience tests that the network and DHT remain operational
// even when 25% of nodes suddenly go offline.
func TestCluster_NodeChurnResilience(t *testing.T) {
	if raceBuild {
		t.Skip("cluster simulation skipped under race detector; tested in non-race CI job")
	}
	nodeCount := getClusterNodeCount(12)
	t.Logf("=== Starting %d-Node Churn & Fault-Tolerance Test ===", nodeCount)

	cluster, err := NewCluster(t, nodeCount)
	if err != nil {
		t.Fatalf("Failed to initialize cluster: %v", err)
	}
	defer cluster.Close()

	_ = cluster.WaitForConvergence(5*time.Second, 1)

	// Store data on node 1
	stableKey := vpnmarket.Key(cluster.Nodes[1].ID)
	stableVal := []byte("indestructible_data_across_churn")
	if err := cluster.Nodes[1].DhtEngine.Set(stableKey, stableVal); err != nil {
		t.Fatalf("Initial Set failed: %v", err)
	}

	// Set stores in the background, on the closest peers it can reach after a lookup: wait until
	// copies exist on other nodes, as they would long before any churn in a real network
	deadline := time.Now().Add(15 * time.Second)
	for replicas := 0; time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		replicas = 0
		for i, n := range cluster.Nodes {
			if i == 1 {
				continue
			}
			if raw, err := n.Swarm.GetStorage().Get(dht.RecordKey(stableKey)); err == nil && len(raw) > 0 {
				replicas++
			}
		}
		if replicas >= 2 {
			break
		}
	}

	// Abruptly kill 5 nodes in the middle of the cluster
	killedNodes := 5
	t.Logf("[Churn] Killing %d nodes concurrently...", killedNodes)
	for i := 5; i < 5+killedNodes; i++ {
		cluster.Nodes[i].Stop()
	}

	time.Sleep(500 * time.Millisecond)

	// Surviving node on the edge queries the data
	survivorNode := cluster.Nodes[nodeCount-1]
	t.Logf("[Churn] Survivor Node (%s) querying DHT under churn conditions...", survivorNode.ID.String()[:8])
	got, err := survivorNode.DhtEngine.Get(stableKey)
	if err != nil {
		t.Fatalf("Failed to retrieve key from DHT after node churn: %v", err)
	}

	if !bytes.Equal(got, stableVal) {
		t.Fatalf("Data corrupted after node churn")
	}

	t.Log("[Churn] SUCCESS: Network successfully absorbed node churn and served DHT data without disruption!")
}

// TestCluster_MultiHop_And_OfflineMailbox verifies:
// 1. Pure linear topology: Node 1 <-> Node 2 <-> Node 3 <-> Node 4 (Node 1 is NOT connected to Node 4).
// 2. Multi-hop E2EE message delivery: Node 1 sends an encrypted chat message to Node 4 through intermediate Relays 2 & 3.
// 3. Offline Mailbox (Store-and-Forward): Node 4 disconnects/stops; Node 1 sends a second message; Relay Node 3 holds it in its Mailbox.
// 4. Reactive Mailbox Flush: Node 4 reconnects to Node 3; Node 3 immediately delivers the pending message, and deletes it from storage.
// 5. Mailbox TTL & Auto-cleanup: Expired messages are automatically purged by CleanupExpired.
func TestCluster_MultiHop_And_OfflineMailbox(t *testing.T) {
	if raceBuild {
		t.Skip("cluster simulation skipped under race detector; tested in non-race CI job")
	}
	t.Log("=== Starting 4-Node Multi-Hop & Offline Mailbox Simulation ===")

	cluster, err := NewIsolatedCluster(t, 4)
	if err != nil {
		t.Fatalf("Failed to create isolated cluster: %v", err)
	}
	defer cluster.Close()

	node1 := cluster.Nodes[0]
	node2 := cluster.Nodes[1]
	node3 := cluster.Nodes[2]
	node4 := cluster.Nodes[3]

	// 1. Construct strict linear chain topology:
	// Node 1 <-> Node 2
	// Node 2 <-> Node 3
	// Node 3 <-> Node 4
	t.Log("[Topology] Connecting Node 1 <-> Node 2...")
	node1.Swarm.Bootstrap([]string{NodeAddr(node2)})
	time.Sleep(300 * time.Millisecond)

	t.Log("[Topology] Connecting Node 2 <-> Node 3...")
	node2.Swarm.Bootstrap([]string{NodeAddr(node3)})
	time.Sleep(300 * time.Millisecond)

	t.Log("[Topology] Connecting Node 3 <-> Node 4...")
	node3.Swarm.Bootstrap([]string{NodeAddr(node4)})
	time.Sleep(500 * time.Millisecond)

	// Verify isolation: Node 1 must NOT have direct connection to Node 4!
	if node1.Swarm.GetPeer(node4.ID) != nil {
		t.Fatalf("Test setup failure: Node 1 must not be directly connected to Node 4")
	}
	if node4.Swarm.GetPeer(node1.ID) != nil {
		t.Fatalf("Test setup failure: Node 4 must not be directly connected to Node 1")
	}
	t.Log("[Topology] Linear chain verified: Node 1 is separated from Node 4 by 2 intermediate relays (Nodes 2 & 3)")

	// 2. Exchange public PreKeyBundles into local storage caches for asynchronous E2EE initiation
	node1Bundle, err := node1.Swarm.CreateMyPreKeyBundle()
	if err != nil {
		t.Fatalf("Failed to create Node 1 PreKeyBundle: %v", err)
	}
	node4Bundle, err := node4.Swarm.CreateMyPreKeyBundle()
	if err != nil {
		t.Fatalf("Failed to create Node 4 PreKeyBundle: %v", err)
	}

	if err := node1.Swarm.CacheBundle(node4.ID, node4Bundle); err != nil {
		t.Fatal(err)
	}
	if err := node4.Swarm.CacheBundle(node1.ID, node1Bundle); err != nil {
		t.Fatal(err)
	}

	// 3. Online Multi-Hop E2EE Test: Node 1 -> Node 2 -> Node 3 -> Node 4
	receivedMsgChan := make(chan string, 1)
	node4.GetMessenger().AddHandleCallback(func(d *dto.MessageDTO) {
		t.Logf("[Node 4] Received incoming message: %s", d.Text)
		receivedMsgChan <- d.Text
	})

	testText := "Hello Node 4 across 3 hops and 2 relays!"
	t.Logf("[Multi-Hop] Node 1 sending E2EE message to Node 4: %q", testText)

	if err := node1.SendMessage(node4.ID, testText); err != nil {
		t.Fatalf("Node 1 SendMessage failed: %v", err)
	}

	select {
	case msgText := <-receivedMsgChan:
		if msgText != testText {
			t.Fatalf("Received message mismatch: got %q, expected %q", msgText, testText)
		}
		t.Log("[Multi-Hop] SUCCESS: Message delivered through 1 -> 2 -> 3 -> 4 multi-hop chain and decrypted!")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for multi-hop message delivery")
	}

	// 4. Offline Mailbox Test:
	// Node 4 goes offline. Dropping the connection is not enough: its process still runs and the
	// others dial it again on their own (DHT, reconnects), which delivers the message at once and
	// skips the mailbox. So nobody may connect to it, and it to nobody, until it comes back.
	t.Log("[Mailbox] Simulating Node 4 going offline (no node can reach it)...")
	for _, n := range []*node.Node{node1, node2, node3} {
		n.Swarm.BanPeer(node4.ID)
		node4.Swarm.BanPeer(n.ID)
	}
	time.Sleep(200 * time.Millisecond)

	offlineMsgText := "Important offline alert: wake up Node 4!"
	t.Logf("[Mailbox] Node 1 sending message to offline Node 4: %q", offlineMsgText)

	if err := node1.SendMessage(node4.ID, offlineMsgText); err != nil {
		t.Fatalf("Node 1 SendMessage during offline failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Verify that Relay Node 3 stored the message in its Mailbox!
	mailboxCount := node3.GetMailbox().Count(node4.ID)
	if mailboxCount != 1 {
		t.Fatalf("Node 3 Mailbox should have exactly 1 pending message for Node 4, got: %d", mailboxCount)
	}
	t.Logf("[Mailbox] SUCCESS: Relay Node 3 successfully stored offline message for Node 4 (Pending: %d)", mailboxCount)

	// 5. Reactive Reconnection & Mailbox Delivery:
	// Node 4 comes back online and reconnects to Node 3
	t.Log("[Mailbox] Node 4 coming back online and reconnecting to Node 3...")
	node3.Swarm.UnBanPeer(node4.ID)
	node4.Swarm.UnBanPeer(node3.ID)
	node4.Swarm.Bootstrap([]string{NodeAddr(node3)})

	select {
	case msgText := <-receivedMsgChan:
		if msgText != offlineMsgText {
			t.Fatalf("Received offline message mismatch: got %q, expected %q", msgText, offlineMsgText)
		}
		t.Log("[Mailbox] SUCCESS: Stored message flushed from Mailbox and delivered to Node 4 upon reconnection!")
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for mailbox message flush upon reconnection")
	}

	// Verify mailbox was pruned. The relay hands stored mail over again ten seconds after the
	// reconnect (the first frame on a fresh connection can be lost) and drops it only then.
	remainingInMailbox := node3.GetMailbox().Count(node4.ID)
	for deadline := time.Now().Add(15 * time.Second); remainingInMailbox != 0 && time.Now().Before(deadline); {
		time.Sleep(200 * time.Millisecond)
		remainingInMailbox = node3.GetMailbox().Count(node4.ID)
	}
	if remainingInMailbox != 0 {
		t.Fatalf("Mailbox should be empty after delivery, but still contains %d items", remainingInMailbox)
	}
	t.Log("[Mailbox] Mailbox storage cleanly acknowledged and cleared!")

	// 6. TTL Auto-Cleanup Test:
	t.Log("[Mailbox] Testing TTL expiration and auto-cleanup...")
	expiredItem := messenger.MailboxItem{
		ID:          "expired-msg-001",
		RecipientID: node4.ID,
		SenderID:    node1.ID,
		Payload:     []byte("old data"),
		CreatedAt:   time.Now().Unix() - 100,
		ExpiresAt:   time.Now().Unix() - 10, // already expired
	}
	_ = node3.GetMailbox().Store(expiredItem)

	purged, err := node3.GetMailbox().CleanupExpired()
	if err != nil {
		t.Fatalf("CleanupExpired failed: %v", err)
	}
	if purged != 1 {
		t.Fatalf("Expected 1 expired item purged, got: %d", purged)
	}
	t.Log("[Mailbox] SUCCESS: Expired items purged cleanly by TTL garbage collector!")
}

// TestCluster_VirtualCircuit_UniversalRelay tests end-to-end Virtual Circuit creation
// and zero-copy streaming of arbitrary payload types (file chunks, DHT packets, etc.)
// between two isolated peers (Node A and Node B) connected ONLY through an intermediate Relay R.
func TestCluster_VirtualCircuit_UniversalRelay(t *testing.T) {
	if raceBuild {
		t.Skip("cluster simulation skipped under race detector; tested in non-race CI job")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cluster, err := NewIsolatedCluster(t, 3)
	if err != nil {
		t.Fatalf("Failed to create isolated cluster: %v", err)
	}
	defer cluster.Close()

	nodeA := cluster.Nodes[0] // Client Initiator
	nodeR := cluster.Nodes[1] // Relay Server
	nodeB := cluster.Nodes[2] // Target Peer

	// 1. Establish star topology: Node A <-> Node R and Node B <-> Node R.
	// Note: Node A has NO direct connection to Node B!
	t.Log("[Circuit] Connecting Node A <-> Relay R...")
	nodeA.Swarm.Bootstrap([]string{NodeAddr(nodeR)})
	time.Sleep(300 * time.Millisecond)

	t.Log("[Circuit] Connecting Node B <-> Relay R...")
	nodeB.Swarm.Bootstrap([]string{NodeAddr(nodeR)})
	time.Sleep(300 * time.Millisecond)

	// Verify Node A cannot reach Node B directly
	if nodeA.Swarm.ThisIsActivePeer(nodeB.ID) {
		t.Fatalf("Test assumption failed: Node A is directly connected to Node B")
	}

	// 2. Register data listener on Node B for universal data frames
	type circuitFrame struct {
		msgType net_types.MessageType
		from    types.PeerID
		data    []byte
	}
	received := make(chan circuitFrame, 10)
	nodeB.GetRelayClient().AddDataListener(func(circuitID uint32, msgType net_types.MessageType, payload []byte, remotePeer types.PeerID) {
		received <- circuitFrame{msgType, remotePeer, append([]byte(nil), payload...)}
	})

	// 3. Node A opens a Virtual Circuit to Node B through Relay R
	t.Log("[Circuit] Node A requesting Virtual Circuit to Node B via Relay R...")
	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	defer openCancel()

	circuitID, err := nodeA.GetRelayClient().OpenCircuit(openCtx, nodeR.ID, nodeB.ID)
	if err != nil {
		t.Fatalf("Failed to open Virtual Circuit: %v", err)
	}
	t.Logf("[Circuit] SUCCESS: Virtual Circuit #%d established!", circuitID)

	// 4. Test Zero-Copy Data Streaming over the Virtual Circuit:
	// Case 4A: Arbitrary File Chunk (TypeBlockData)
	fileChunkPayload := []byte("SIMULATED_MERKLE_DAG_FILE_CHUNK_SHA256_ABC123_ZERO_COPY_PAYLOAD")
	start := time.Now()
	err = nodeA.GetRelayClient().SendCircuitData(circuitID, net_types.TypeBlockData, fileChunkPayload)
	if err != nil {
		t.Fatalf("Failed to send TypeBlockData across circuit: %v", err)
	}

	select {
	case f := <-received:
		data := f.data
		elapsed := time.Since(start)
		if !bytes.Equal(data, fileChunkPayload) {
			t.Fatalf("Payload mismatch on Node B! Expected: %s, Got: %s", string(fileChunkPayload), string(data))
		}
		if f.msgType != net_types.TypeBlockData {
			t.Fatalf("MessageType mismatch! Expected: %v, Got: %v", net_types.TypeBlockData, f.msgType)
		}
		if f.from != nodeA.ID {
			t.Fatalf("Origin mismatch! Expected Node A (%s), Got: %s", nodeA.ID.String()[:8], f.from.String()[:8])
		}
		t.Logf("[Circuit] SUCCESS: File chunk delivered across circuit in %v! (Zero loss, full integrity)", elapsed)
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for circuit data on Node B")
	}

	// Case 4B: Arbitrary DHT Query (TypeFindNode)
	dhtQueryPayload := []byte("KADEMLIA_FIND_NODE_QUERY_RANDOM_TARGET_HASH")
	err = nodeA.GetRelayClient().SendCircuitData(circuitID, net_types.TypeFindNode, dhtQueryPayload)
	if err != nil {
		t.Fatalf("Failed to send TypeFindNode across circuit: %v", err)
	}

	select {
	case f := <-received:
		data := f.data
		if !bytes.Equal(data, dhtQueryPayload) {
			t.Fatalf("DHT payload mismatch on Node B")
		}
		if f.msgType != net_types.TypeFindNode {
			t.Fatalf("MessageType mismatch! Expected: %v, Got: %v", net_types.TypeFindNode, f.msgType)
		}
		t.Log("[Circuit] SUCCESS: DHT query successfully relayed across virtual circuit!")
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for second circuit packet on Node B")
	}

	// 5. Test SendUniversal: auto-routing through existing circuit
	universalPayload := []byte("UNIVERSAL_P2P_TRANSPARENT_PACKET")
	err = nodeA.GetRelayClient().SendUniversal(ctx, nodeB.ID, net_types.TypeChatMessage, universalPayload)
	if err != nil {
		t.Fatalf("SendUniversal failed: %v", err)
	}

	select {
	case f := <-received:
		data := f.data
		if !bytes.Equal(data, universalPayload) {
			t.Fatalf("Universal payload mismatch")
		}
		t.Log("[Circuit] SUCCESS: SendUniversal automatically utilized open circuit without re-handshaking!")
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for SendUniversal payload")
	}
}
