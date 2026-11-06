package voice

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/memcarrier"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// A small copy of the files/2 test stand: node A (the sender) holds the file, others dial it over
// a real session on an in-memory carrier. Identities are made once per test binary: memguard limits
// how many a Windows test process can hold.

var (
	fileServices = []handshake.ServiceOffer{{ID: 7, Versions: []uint32{2}}}
	fileSuites   = []uint16{handshake.SuiteXK, handshake.SuiteXX}
)

type tnode struct {
	account *identity.Account
	device  *identity.Device
	devices []byte
	now     time.Time
	tw      *identity.Throwaway
}

func newTNode() *tnode {
	tw, err := identity.NewThrowaway()
	if err != nil {
		panic(err)
	}
	devices, err := tw.DeviceList(1, time.Now())
	if err != nil {
		panic(err)
	}
	return &tnode{account: tw.Account, device: tw.Device, devices: devices, now: time.Now(), tw: tw}
}

func (n *tnode) node() [32]byte { return n.account.NodeID() }

var nodeA, nodeB, nodeC *tnode

func TestMain(m *testing.M) {
	nodeA, nodeB, nodeC = newTNode(), newTNode(), newTNode()
	code := m.Run()
	for _, n := range []*tnode{nodeA, nodeB, nodeC} {
		n.tw.Destroy()
	}
	os.Exit(code)
}

func (n *tnode) card(seq uint64) []byte {
	body := &pb.NodeCard{ProtoVer: handshake.ProtoVer, HsKey: n.device.Handshake().Public(),
		L1Suites: []uint32{uint32(handshake.SuiteXK), uint32(handshake.SuiteXX)},
		Account:  n.account.Public(), Devices: n.devices, Scope: uint32(objects.ScopePeer)}
	var env []byte
	err := n.device.WithSigner(func(s *keys.Signer) error {
		var err error
		env, err = objects.SealCard(s, seq, uint64(n.now.Add(24*time.Hour).Unix()), body)
		return err
	})
	if err != nil {
		panic(err)
	}
	return env
}

func (n *tnode) local(withOwn bool) session.LocalIdentity {
	peer := n.card(10)
	l := session.LocalIdentity{Device: n.device, Card: n.card(7), Suites: fileSuites,
		Features: []uint32{uint32(frame.FeaturePathMigration)}, Services: fileServices}
	if withOwn {
		l.OwnCards = handshake.OwnCards{
			Current: map[uint32]handshake.OwnCard{uint32(objects.ScopePeer): {Seq: 10, Envelope: peer}},
			Issued:  map[uint64]handshake.IssuedCard{10: {Scope: uint32(objects.ScopePeer), At: n.now}},
		}
	}
	return l
}

func (n *tnode) verified() *objects.Card {
	c, err := objects.VerifyCard(n.card(10), n.now)
	if err != nil {
		panic(err)
	}
	return c
}

type oneListener struct {
	c    mux.Carrier
	mu   sync.Mutex
	used bool
}

func (l *oneListener) Accept(ctx context.Context) (mux.Carrier, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	l.used = true
	return l.c, nil
}
func (l *oneListener) Addr() net.Addr { return nil }
func (l *oneListener) Close() error   { return nil }

type stand struct {
	t   *testing.T
	ctx context.Context
	srv *files.Server
}

func newStand(t *testing.T) *stand {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return &stand{t: t, ctx: ctx, srv: files.NewServer(files.ServerConfig{})}
}

// open dials node A from a client node and returns a files/2 stream.
func (s *stand) open(from *tnode) *mux.Stream {
	t := s.t
	t.Helper()
	pair := memcarrier.NewBytes()
	dialed := make(chan *session.Established, 1)
	go func() {
		est, err := session.Dial(s.ctx, nodeA.verified(), carrier.Attempt{Kind: carrier.AttemptCarrier},
			from.local(false), session.WithCarrier(pair.Client))
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- est
	}()
	serverEst, err := session.Accept(s.ctx, &oneListener{c: pair.Server}, session.AcceptConfig{Local: nodeA.local(true)})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	clientEst := <-dialed
	if clientEst == nil {
		t.Fatal("client session not established")
	}
	t.Cleanup(func() { _ = clientEst.Session.Close(); _ = serverEst.Session.Close() })
	peer := files.Peer{Node: [32]byte(serverEst.Peer.NodeID), Device: [32]byte(serverEst.Peer.DeviceID)}
	go func() {
		for {
			st, err := serverEst.Session.AcceptStream(s.ctx)
			if err != nil {
				return
			}
			go s.srv.Handle(s.ctx, peer, st)
		}
	}()
	st, err := clientEst.Session.OpenStream(s.ctx, 7, 2)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	sp, _ := service.Builtin(service.Files)
	st.SetLimiter(sp.Versions[2].Limiter())
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// queue is the mailbox of an offline recipient: what the sender put in while the recipient was away,
// as the padded bytes chat/2 seals.
type queue struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (q *queue) put(t *testing.T, m *chatpb.PersonalMessageProto) {
	b, err := chatpb.PadPersonal(m)
	if err != nil {
		t.Fatalf("PadPersonal: %v", err)
	}
	q.mu.Lock()
	q.msgs = append(q.msgs, b)
	q.mu.Unlock()
}

func (q *queue) take() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.msgs
	q.msgs = nil
	return out
}

// 30 seconds and 5 minutes reach a recipient who was offline when they were sent: the offer waits in
// the mailbox, the clip is fetched from the sender over files/2 when the recipient comes back, the
// root of the file is proved, and a message delivered twice is one message.
func TestVoiceReachesAnOfflineRecipient(t *testing.T) {
	for _, secs := range []int{30, 300} {
		secs := secs
		t.Run(time.Duration(secs*int(time.Second)).String(), func(t *testing.T) {
			st := newStand(t)
			d, err := Compose(rec(t, secs), nodeA.node(), [][32]byte{nodeB.node()})
			if err != nil {
				t.Fatal(err)
			}
			if d.Share == nil {
				t.Fatal("the clip should be a file")
			}
			if err := st.srv.Add(*d.Share); err != nil {
				t.Fatal(err)
			}
			var q queue
			q.put(t, d.Message)
			q.put(t, d.Message) // the same message again: a retry and the direct copy

			// B comes online, reads the mailbox
			in := NewInbox(32)
			var entry *Entry
			for _, raw := range q.take() {
				m, err := chatpb.ParsePersonal(raw)
				if err != nil {
					t.Fatalf("ParsePersonal: %v", err)
				}
				e, fresh, err := in.Add(m)
				if err != nil {
					t.Fatalf("Add: %v", err)
				}
				if fresh {
					entry = e
				}
			}
			if in.Len() != 1 || entry == nil {
				t.Fatalf("two copies made %d entries", in.Len())
			}
			offer, ok := entry.Offer()
			if !ok {
				t.Fatal("no offer")
			}
			store, err := files.OpenFileStore(t.TempDir(), offer, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := files.Fetch(st.ctx, st.open(nodeB), offer, store, files.FetchOptions{}); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			got, err := os.ReadFile(store.Path())
			if err != nil {
				t.Fatal(err)
			}
			if err := entry.Finish(got); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if a, ok := entry.Audio(); !ok || !bytes.Equal(a, d.Audio) {
				t.Fatal("the clip that arrived is not the clip that was sent")
			}
			if entry.Meta().DurationMS != uint32(secs*1000) {
				t.Fatalf("duration %d", entry.Meta().DurationMS)
			}
		})
	}
}

// In a group the clip goes to every member the share names, and only to them.
func TestVoiceInAGroupIsServedToMembersOnly(t *testing.T) {
	st := newStand(t)
	d, err := Compose(rec(t, 45), nodeA.node(), [][32]byte{nodeB.node()}) // C is not a recipient
	if err != nil {
		t.Fatal(err)
	}
	if err := st.srv.Add(*d.Share); err != nil {
		t.Fatal(err)
	}
	offer := d.Share.Offer
	store, err := files.OpenFileStore(t.TempDir(), offer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = files.Fetch(st.ctx, st.open(nodeC), offer, store, files.FetchOptions{})
	if !errors.Is(err, files.ErrUnavailable) {
		t.Fatalf("a node outside the share fetched the clip: %v", err)
	}
}
