package node_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/pex"
	pexpb "github.com/echofog/echofog-core/internal/v2/pex/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
)

func releaseSigners(t *testing.T) []*keys.Signer {
	t.Helper()
	var out []*keys.Signer
	for i := byte(1); i <= 3; i++ {
		seed := sha256.Sum256([]byte{'n', 'r', i})
		s, err := keys.NewEd25519Signer(seed[:])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func releaseKeysOf(ss []*keys.Signer) []keys.AKey {
	out := make([]keys.AKey, len(ss))
	for i, s := range ss {
		out[i] = s.Public()
	}
	return out
}

func sealRelease(t *testing.T, ss []*keys.Signer, minSupported, recommended uint32) []byte {
	t.Helper()
	rel := &pb.Release{
		Version:    "0.3.1",
		ReleasedAt: uint64(time.Now().Add(-time.Hour).Unix()),
		Policies:   []*pb.Policy{{Component: "proto", MinSupported: minSupported, Recommended: recommended}},
	}
	raw, err := objects.SealRelease(ss[0], rel.ReleasedAt+86400*30, rel, ss[1])
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestNoReleaseKeysNoManifest(t *testing.T) {
	n := newTestNode(t, cards.Hidden, false, nil, nil)
	if n.rt.Release() != nil {
		t.Fatal("a manifest manager without release keys")
	}
	if node.AdmitVersionOf(n.rt) != nil {
		t.Fatal("a version admission without release keys")
	}
}

// TestReleaseOverPexAndAdmission: a node with release keys hands its manifest out in pex/2 (GET
// release=true), the lazy pex client of the other node applies it, and the handshake admission
// refuses a peer below min_supported but never the node's own version (never lock out).
func TestReleaseOverPexAndAdmission(t *testing.T) {
	ss := releaseSigners(t)
	mutate := func(c *node.Config) { c.ReleaseKeys = releaseKeysOf(ss) }
	queue := newCarrierQueue()
	a := newTestNodeWith(t, cards.Hidden, false, nil, nil, mutate)
	b := newTestNodeWith(t, cards.Visible, true, []carrier.Listener{queue}, nil, mutate)
	b.start(t)
	a.start(t)

	if a.rt.Release() == nil || b.rt.Release() == nil {
		t.Fatal("no release manager although release keys are configured")
	}
	raw := sealRelease(t, ss, 150, 200)
	if ok, err := b.rt.Release().Offer(raw); err != nil || !ok {
		t.Fatalf("Offer: %v, %v", ok, err)
	}
	admit := node.AdmitVersionOf(b.rt)
	if admit == nil {
		t.Fatal("no version admission")
	}
	if admit(149) || !admit(150) || !admit(200) {
		t.Fatal("admission does not follow min_supported 150")
	}

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.PEX, node.DialOpt{Card: b.publicCard(t), Carrier: pair.Client})
	if err != nil {
		t.Fatalf("Open pex: %v", err)
	}
	get, err := objects.Marshal(&pexpb.Get{Req: 7, Max: 16, Release: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.WriteFrame(pex.TypeGet, 0, get); err != nil {
		t.Fatal(err)
	}
	f, err := stream.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var answer pexpb.Cards
	if err := proto.Unmarshal(f.Payload, &answer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(answer.GetRelease(), raw) {
		t.Fatalf("pex answer carries %d bytes of release, want the applied manifest", len(answer.GetRelease()))
	}
	_ = stream.Close()

	// The lazy client of a asks the same GET after 1–10 s and applies what it gets.
	deadline := time.Now().Add(20 * time.Second)
	for !bytes.Equal(a.rt.Release().Latest(), raw) {
		if time.Now().After(deadline) {
			t.Fatal("the lazy pex client did not apply the manifest from the answer")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestReleaseCheckGuardsTheDHT: the runtime builds the DHT release check from its manager.
func TestReleaseCheckGuardsTheDHT(t *testing.T) {
	ss := releaseSigners(t)
	n := newTestNodeWith(t, cards.Hidden, false, nil, nil, func(c *node.Config) { c.ReleaseKeys = releaseKeysOf(ss) })
	n.start(t)
	check := node.ReleaseCheckOf(n.rt)
	if check == nil {
		t.Fatal("no release check for the DHT server")
	}
	good := sealRelease(t, ss, 200, 200)
	if err := check(good, time.Now()); err != nil {
		t.Fatalf("a valid manifest: %v", err)
	}
	one, err := objects.SealRelease(ss[0], uint64(time.Now().Add(time.Hour).Unix()), &pb.Release{Version: "x", ReleasedAt: uint64(time.Now().Unix())})
	if err != nil {
		t.Fatal(err)
	}
	if err := check(one, time.Now()); err == nil {
		t.Fatal("a manifest with one signature passes the DHT check")
	}
}
