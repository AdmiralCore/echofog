package node_test

import (
	"testing"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/service"

	"context"
	"time"
)

func filesNode(t *testing.T, vis cards.Visibility, listeners []carrier.Listener, onDone func([16]byte, [32]byte)) *testNode {
	t.Helper()
	return newTestNodeWith(t, vis, true, listeners, nil, func(c *node.Config) { c.Files, c.FilesOnDone = true, onDone })
}

func openFiles(t *testing.T, a, b *testNode, queue *carrierQueue) (*mux.Stream, error) {
	t.Helper()
	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, st, err := a.rt.Open(ctx, b.id.DeviceID, service.Files, node.DialOpt{Card: b.publicCard(t), Carrier: pair.Client})
	return st, err
}

// A node built with Config.Files serves files/2 and offers the server to the app; one built without it
// offers no files service at all.
func TestFilesServiceIsOfferedOnlyWithTheFlag(t *testing.T) {
	queue := newCarrierQueue()
	a := filesNode(t, cards.Hidden, nil, nil)
	b := filesNode(t, cards.Visible, []carrier.Listener{queue}, func([16]byte, [32]byte) {})
	b.start(t)
	a.start(t)
	if b.rt.Files() == nil {
		t.Fatal("a node with Config.Files has no files server")
	}
	st, err := openFiles(t, a, b, queue)
	if err != nil {
		t.Fatalf("Open files: %v", err)
	}
	_ = st.Close()

	queue2 := newCarrierQueue()
	c := newTestNode(t, cards.Hidden, false, nil, nil)
	d := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue2}, nil, nil)
	d.start(t)
	c.start(t)
	if d.rt.Files() != nil {
		t.Fatal("a node without Config.Files has a files server")
	}
	if st, err := openFiles(t, c, d, queue2); err == nil {
		_ = st.Close()
		t.Fatal("files/2 was offered without Config.Files")
	}
}

// OpenFiles finds a device of the account and opens a files/2 stream to it; an account nobody can find
// is an error, not a hang.
func TestOpenFilesFailsForAnAccountItCannotFind(t *testing.T) {
	a := filesNode(t, cards.Hidden, nil, nil)
	a.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if st, err := a.rt.OpenFiles(ctx, [32]byte{1, 2, 3}); err == nil {
		_ = st.Close()
		t.Fatal("OpenFiles to an unknown account succeeded")
	}
}
