package pkg

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/config"
	"github.com/echofog/echofog-core/pkg/api/node"
	node3 "github.com/echofog/echofog-core/pkg/node"
)

func TestAlpha_FullMessageFlow(t *testing.T) {
	cfg1 := config.DefaultConfig("")
	cfg1.Network.ListenAddr = "127.0.0.1:9001"
	cfg1.Storage.DatabasePath = t.TempDir() + "/db1"

	cfg2 := config.DefaultConfig("")
	cfg2.Network.ListenAddr = "127.0.0.1:9002"
	cfg2.Storage.DatabasePath = t.TempDir() + "/db2"

	aliceNode := node3.NewNode(context.Background(), cfg1, "Alice")
	if _, err := aliceNode.Register([]byte("pass123"), []byte("pass123")); err != nil {
		t.Fatalf("Alice register failed: %v", err)
	}
	if err := aliceNode.Start(); err != nil {
		t.Fatalf("Alice start failed: %v", err)
	}

	bobNode := node3.NewNode(context.Background(), cfg2, "Bob")
	if _, err := bobNode.Register([]byte("pass123"), []byte("pass123")); err != nil {
		t.Fatalf("Bob register failed: %v", err)
	}
	if err := bobNode.Start(); err != nil {
		t.Fatalf("Bob start failed: %v", err)
	}

	defer func() {
		aliceNode.Stop()
		bobNode.Stop()
	}()

	api1 := node.NewNodeApi(aliceNode)
	api2 := node.NewNodeApi(bobNode)

	msgChan1 := api1.MessageHandler()
	msgChan2 := api2.MessageHandler()

	if err := api1.ConnectToPeer("127.0.0.1:9002/" + bobNode.ID.String()); err != nil {
		t.Fatalf("Connection failed: %v", err)
	}

	success := false
	for i := 0; i < 50; i++ {
		if len(api1.GetPeers()) > 0 && len(api2.GetPeers()) > 0 {
			success = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !success {
		t.Fatal("Nodes connected but Peer not found in Swarm active maps symmetrically")
	}

	aliceID := api1.GetMyPubKey()
	bobID := api2.GetMyPubKey()
	t.Logf("Alice sending message to Bob: %s", bobID)

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := api1.SendMessage(bobID, "Hello EchoFog!"); err != nil {
			t.Errorf("Alice send message failed: %v", err)
		}
	}()
	wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := api2.SendMessage(aliceID, "Hello from api2"); err != nil {
			t.Errorf("Bob send message failed: %v", err)
		}
	}()
	wg.Wait()

	receivedCount := 0
	for receivedCount < 2 {
		select {
		case msg := <-msgChan1:
			if msg.IsMine {
				continue
			}
			if msg.Text == "Hello from api2" {
				t.Log("✅ Alice (Node 1) received message from Bob (Node 2)")
				receivedCount++
			} else {
				t.Errorf("Alice: unexpected text '%s'", msg.Text)
			}

		case msg := <-msgChan2:
			if msg.IsMine {
				continue
			}
			if msg.Text == "Hello EchoFog!" {
				t.Log("✅ Bob (Node 2) received message from Alice (Node 1)")
				receivedCount++
			} else {
				t.Errorf("Bob: unexpected text '%s'", msg.Text)
			}

		case <-time.After(5 * time.Second):
			t.Fatalf("❌ Timeout: only %d/2 messages received", receivedCount)
		}
	}
}
