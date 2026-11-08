package scenarios

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/testbed/gate"
)

func hiddenScenario() Scenario {
	return Scenario{Name: "v2-hidden", Title: "Hidden node: the invitation opens it, a stranger gets silence", Live: liveHidden, Run: runHidden}
}

// redeem opens an invitation link from n through a real handshake over the real carrier.
func (n *Node) redeem(ctx context.Context, link string) (node.Peer, error) {
	ln, err := links.New(links.Config{Dialer: n.RT, Clock: n.RT.Clock()})
	if err != nil {
		return node.Peer{}, err
	}
	return ln.Redeem(ctx, link)
}

func runHidden(ctx context.Context, env Env) Result {
	res := Result{Name: "v2-hidden", Mode: "model", Seed: env.Seed}
	dir, err := os.MkdirTemp(env.Dir, "hidden-")
	if err != nil {
		dir = os.TempDir()
	}
	defer os.RemoveAll(dir)
	fail := func(id, note string, err error) Result {
		res.Rows = append(res.Rows, newRow(id, "hidden node", err.Error(), "see the row", gate.Red, note))
		return res
	}

	w := NewWorld(dir)
	defer w.Close()
	h, err := w.Add(NodeSpec{Name: "hidden", Hidden: true})
	if err != nil {
		return fail("v2-hidden/not-listed", "the hidden node did not build", err)
	}
	guest, err := w.Add(NodeSpec{Name: "guest"})
	if err != nil {
		return fail("v2-hidden/invited-in", "the guest did not build", err)
	}
	stranger, err := w.Add(NodeSpec{Name: "stranger"})
	if err != nil {
		return fail("v2-hidden/stranger", "the stranger did not build", err)
	}
	if err := w.StartAll(ctx); err != nil {
		return fail("v2-hidden/not-listed", "start", err)
	}

	// 1. nothing public: no public card, nothing registered for the DHT
	_, hasPublic := h.RT.Cards().Own(objects.ScopePublic)
	pending := 0
	if cl := h.RT.DHT(); cl != nil {
		pending = cl.Pending()
	}
	st := gate.Green
	if hasPublic || pending != 0 {
		st = gate.Red
	}
	res.Rows = append(res.Rows, newRow("v2-hidden/not-listed", "public card / records for the DHT", fmt.Sprintf("card %v, records %d", hasPublic, pending), "no card, no records", st, ""))

	// 2. the invitation opens it
	minted, err := h.RT.MintInvitation(ctx, access.MintOptions{LinkTTL: time.Hour, AccessTTL: time.Hour, MaxEntries: 2})
	if err != nil {
		return fail("v2-hidden/invited-in", "MintInvitation", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	peer, err := guest.redeem(rctx, minted.Link)
	cancel()
	switch {
	case err != nil:
		res.Rows = append(res.Rows, newRow("v2-hidden/invited-in", "redeem", err.Error(), "a session with the hidden node", gate.Red, ""))
	case peer.Device != h.ID.DeviceID:
		res.Rows = append(res.Rows, newRow("v2-hidden/invited-in", "redeem", "another device answered", "a session with the hidden node", gate.Red, ""))
	default:
		res.Rows = append(res.Rows, newRow("v2-hidden/invited-in", "redeem", "session open over noise3", "a session with the hidden node", gate.Green, ""))
	}

	// 3. a stranger: junk to the invitation port gets nothing back, and a dial that knows the address
	// but not the secret does not get in
	res.Rows = append(res.Rows, strangerRow(ctx, h, stranger, minted.Link))

	// 4. revoked: the same link does not open any more
	if _, err := h.RT.RevokeAccess(ctx, minted.PassID); err != nil {
		return fail("v2-hidden/revoked", "RevokeAccess", err)
	}
	time.Sleep(time.Second) // the listener is rebuilt without the key
	qctx, stop := context.WithTimeout(ctx, 10*time.Second)
	_, err = stranger.redeem(qctx, minted.Link)
	stop()
	if err == nil {
		res.Rows = append(res.Rows, newRow("v2-hidden/revoked", "redeem after revoke", "a session opened", "refused", gate.Red, "a revoked pass still opens the node"))
	} else {
		res.Rows = append(res.Rows, newRow("v2-hidden/revoked", "redeem after revoke", "refused", "refused", gate.Green, ""))
	}
	res.NotChecked = []string{
		"a real censor's active probe of the invitation port: the junk is a handful of random datagrams from one socket",
		"the DHT never learns the node: shown by the absence of a public card and of records to publish, not by watching a DHT",
	}
	return res
}

// strangerRow sends random datagrams to the invitation port and dials without the secret.
func strangerRow(ctx context.Context, h, stranger *Node, link string) gate.Row {
	const id = "v2-hidden/stranger"
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return newRow(id, "stranger", err.Error(), "silence", gate.Red, "no socket")
	}
	defer pc.Close()
	to, err := net.ResolveUDPAddr("udp", h.AccessAddr)
	if err != nil {
		return newRow(id, "stranger", err.Error(), "silence", gate.Red, "address")
	}
	for _, size := range []int{1200, 64, 300, 1350} {
		junk := make([]byte, size)
		_, _ = rand.Read(junk)
		_, _ = pc.WriteTo(junk, to)
	}
	_ = pc.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 2048)
	if n, _, err := pc.ReadFrom(buf); err == nil {
		return newRow(id, "stranger", fmt.Sprintf("%d bytes came back", n), "silence", gate.Red, "the invitation port answered junk")
	}
	pv, err := links.Open(link, time.Now())
	if err != nil {
		return newRow(id, "stranger", err.Error(), "silence", gate.Red, "the link did not open offline")
	}
	dctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, _, err := stranger.RT.Session(dctx, node.DeviceID(pv.Device), node.DialOpt{Card: pv.Card}); err == nil {
		return newRow(id, "stranger", "a session opened without the secret", "refused", gate.Red, "")
	}
	return newRow(id, "stranger", "junk unanswered, dial without the secret refused", "silence / refused", gate.Green, "")
}
