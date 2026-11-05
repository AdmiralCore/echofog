package node_test

import (
	"context"
	"crypto/sha256"
	"net/netip"
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

// thirdPartyCard builds a verified public card that belongs to neither of the two test nodes.
func thirdPartyCard(t *testing.T, name, host string, port uint32) *objects.Card {
	t.Helper()
	now := time.Now()
	seed := func(what string) []byte {
		h := sha256.Sum256([]byte("node pex test " + name + " " + what))
		return h[:]
	}
	account, err := keys.NewEd25519Signer(seed("account"))
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	device, err := keys.NewEd25519Signer(seed("device"))
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	raw := make([]byte, 32)
	copy(raw, seed("x25519"))
	hsKey := keys.New(keys.X25519, raw)
	mask := keys.New(keys.X25519, raw)
	notAfter := uint64(now.Add(24 * time.Hour).Unix())
	devices, err := objects.SealDeviceList(account, 1, notAfter, &pb.DeviceList{
		Devices: []*pb.Device{{Key: device.Public(), Added: uint64(now.Unix())}},
	})
	if err != nil {
		t.Fatalf("device list: %v", err)
	}
	body := &pb.NodeCard{
		ProtoVer: 200,
		HsKey:    hsKey,
		L1Suites: []uint32{1, 2},
		Role:     objects.RoleServer,
		Flags:    objects.FlagRelay,
		Account:  account.Public(),
		Devices:  devices,
		Scope:    uint32(objects.ScopePublic),
		MaskKeys: []*pb.MaskKey{{Key: mask, NotAfter: notAfter}},
		Addrs:    []*pb.Addr{{Suite: objects.SuiteReality2QUIC, Host: host, Port: port, Mask: 1}},
	}
	env, err := objects.SealCard(device, uint64(now.Unix()), notAfter, body)
	if err != nil {
		t.Fatalf("seal card: %v", err)
	}
	c, err := objects.VerifyCard(env, now)
	if err != nil {
		t.Fatalf("verify card: %v", err)
	}
	return c
}

func cardAddr(t *testing.T, c *objects.Card) netip.AddrPort {
	t.Helper()
	a := c.Addrs()[0]
	ip, err := netip.ParseAddr(a.GetHost())
	if err != nil {
		t.Fatalf("parse card host: %v", err)
	}
	return netip.AddrPortFrom(ip, uint16(a.GetPort()))
}

// TestPexGetOverTwoRuntimes pins the T5.4 acceptance path: pex/2 is registered, GET is answered with
// the answerer's own card first, release is absent without T4.5, and the lazy client puts a card
// learned from the answer into the new table.
func TestPexGetOverTwoRuntimes(t *testing.T) {
	queue := newCarrierQueue()
	a := newTestNode(t, cards.Hidden, false, nil, nil)
	b := newTestNode(t, cards.Visible, true, []carrier.Listener{queue}, nil)
	b.start(t)
	a.start(t)

	third := thirdPartyCard(t, "third", "192.0.2.77", 9443)
	if !b.rt.Peers().NoteSession(third, cardAddr(t, third), time.Now()) {
		t.Fatal("the answerer did not record a reached card")
	}

	pair := memcarrier.NewQUIC()
	queue.push(pair.Server)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	peer, stream, err := a.rt.Open(ctx, b.id.DeviceID, service.PEX, node.DialOpt{
		Card:    b.publicCard(t),
		Carrier: pair.Client,
	})
	if err != nil {
		t.Fatalf("Open pex: %v", err)
	}
	if peer.Device != b.id.DeviceID {
		t.Fatalf("peer device %x, want %x", peer.Device, b.id.DeviceID)
	}
	if a.rt.Peers().LenTried() != 1 {
		tried := time.Now().Add(2 * time.Second)
		for a.rt.Peers().LenTried() != 1 {
			if time.Now().After(tried) {
				t.Fatalf("the working session did not enter the tried table: %d", a.rt.Peers().LenTried())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	get, err := objects.Marshal(&pexpb.Get{Req: 7, Max: 16, Release: true})
	if err != nil {
		t.Fatalf("marshal GET: %v", err)
	}
	if err := stream.WriteFrame(pex.TypeGet, 0, get); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	f, err := stream.ReadFrame()
	if err != nil {
		t.Fatalf("read CARDS: %v", err)
	}
	if f.Type != pex.TypeCards {
		t.Fatalf("frame type 0x%x, want CARDS", f.Type)
	}
	var answer pexpb.Cards
	if err := proto.Unmarshal(f.Payload, &answer); err != nil {
		t.Fatalf("unmarshal CARDS: %v", err)
	}
	if answer.GetReq() != 7 {
		t.Fatalf("CARDS req = %d, want 7", answer.GetReq())
	}
	if len(answer.GetRelease()) != 0 {
		t.Fatal("release manifest returned without T4.5")
	}
	if len(answer.GetCards()) != 2 {
		t.Fatalf("cards = %d, want own + a reached third party", len(answer.GetCards()))
	}
	own, err := objects.VerifyCard(answer.GetCards()[0], time.Now())
	if err != nil {
		t.Fatalf("first card does not verify: %v", err)
	}
	if own.NodeID != [32]byte(b.id.NodeID) {
		t.Fatal("the answerer's own card is not first")
	}
	_ = stream.Close()

	// The lazy PEX client of the session waits uniform(1, 10) s, then asks and stores the
	// candidate from the answer in the new table.
	deadline := time.Now().Add(13 * time.Second)
	for {
		cands := a.rt.Peers().DialCandidates(16)
		for _, c := range cands {
			if c.DeviceID == third.DeviceID {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the candidate from the PEX answer did not reach the new table (%d new, %d tried)",
				a.rt.Peers().LenNew(), a.rt.Peers().LenTried())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
