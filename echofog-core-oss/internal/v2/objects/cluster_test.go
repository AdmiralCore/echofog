package objects

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// The card field and flag of operator clusters (cluster.md §2, registries.md §11): the reference
// travels verbatim in public and peer cards and is not checked here; verifying it needs the
// cluster objects and belongs to the cluster/2 work.

func TestClusterRefCardField(t *testing.T) {
	if FlagCluster != 1<<7 {
		t.Fatalf("FlagCluster = %#x, want 1<<7", FlagCluster)
	}
	f := newFixture(t)
	root := testSigner(t, "cluster-root")
	ref := &pb.ClusterRef{
		Root:       root.Public(),
		Delegation: []byte{0xde, 0xad, 0xbe, 0xef},
		Membership: []byte{0xca, 0xfe, 0xba, 0xbe},
	}
	cases := []struct {
		name string
		body *pb.NodeCard
	}{
		{"public", f.public(t)},
		{"peer", f.cv.peerCard(t)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.body.Cluster = ref
			c.body.Flags |= FlagCluster
			card, err := VerifyCard(f.seal(t, c.body), vectorNow)
			if err != nil {
				t.Fatalf("card with cluster reference: %v", err)
			}
			got := card.Body.GetCluster()
			if got == nil {
				t.Fatal("cluster reference dropped")
			}
			eqHex(t, "cluster root", got.GetRoot(), root.Public())
			eqHex(t, "cluster delegation", got.GetDelegation(), ref.GetDelegation())
			eqHex(t, "cluster membership", got.GetMembership(), ref.GetMembership())
			if card.Body.GetFlags()&FlagCluster == 0 {
				t.Fatal("flag cluster not kept")
			}
		})
	}
}

func TestClusterRefCanonical(t *testing.T) {
	f := newFixture(t)
	body, err := Marshal(f.public(t))
	if err != nil {
		t.Fatal(err)
	}
	// Field 2 (delegation) before field 1 (root): the nested message breaks the canonical order.
	inner := []byte{0x12, 0x01, 'd', 0x0a, 0x01, 'r'}
	env := sealRaw(LabelCard, f.device, fixedTime, fixedTime+day, appendField(body, 19, protowire.BytesType, inner))
	if _, err := VerifyCard(env, vectorNow); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("non-canonical cluster reference: %v", err)
	}
}
