package holepunch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/echofog/echofog-core/pkg/api/types"
)

type sentMsg struct {
	to types.PeerID
	m  message
}

type recSignal struct {
	mu   sync.Mutex
	sent []sentMsg
}

func (r *recSignal) Send(_ context.Context, to types.PeerID, payload []byte) error {
	var m message
	if err := json.Unmarshal(payload, &m); err != nil {
		return err
	}
	r.mu.Lock()
	r.sent = append(r.sent, sentMsg{to, m})
	r.mu.Unlock()
	return nil
}

func (r *recSignal) answers() []message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []message
	for _, s := range r.sent {
		if s.m.T == "answer" {
			out = append(out, s.m)
		}
	}
	return out
}

func newResponder(sig *recSignal, admit func(types.PeerID, []byte) bool) *Service {
	return New(Config{
		Signal:     sig,
		OwnAddrs:   func() []string { return []string{"203.0.113.10:4242"} },
		LocalAddrs: func() []string { return []string{"192.168.1.5:4242"} },
		Connected:  func(types.PeerID) bool { return false },
		Admit:      admit,
	})
}

func connectFrom(s *Service, from types.PeerID, n uint64, public string) {
	raw, _ := json.Marshal(message{T: "connect", N: n, Addr: []string{public}, Local: []string{"192.168.1.9:4242"}})
	s.Handle(from, raw)
}

// The answer to a CONNECT carries our LAN addresses only for a peer behind the same router (its
// public IP is ours); anyone else learns nothing of our home network.
func TestAnswerGivesLANOnlyBehindTheSameNAT(t *testing.T) {
	sig := &recSignal{}
	s := newResponder(sig, nil)
	defer s.Close()

	connectFrom(s, types.PeerID{1}, 1, "198.51.100.20:5000")
	connectFrom(s, types.PeerID{2}, 2, "203.0.113.10:6000")
	a := sig.answers()
	if len(a) != 2 {
		t.Fatalf("%d answers, want 2", len(a))
	}
	if len(a[0].Local) != 0 {
		t.Fatalf("a peer elsewhere got our LAN addresses %v", a[0].Local)
	}
	if len(a[1].Local) == 0 {
		t.Fatal("a peer behind the same router got no LAN address")
	}
}

// A peer this node set out to reach is wanted for a while: its CONNECT is one we have reason to
// answer. Connect notes the want by itself.
func TestWantedPeers(t *testing.T) {
	s := newResponder(&recSignal{}, nil)
	defer s.Close()
	a, b := types.PeerID{1}, types.PeerID{2}
	if s.Wanted(a) {
		t.Fatal("a stranger is wanted")
	}
	s.Want(a)
	if !s.Wanted(a) || s.Wanted(b) {
		t.Fatal("Want did not mark exactly the peer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Connect(ctx, b)
	if !s.Wanted(b) {
		t.Fatal("a peer we tried to punch to is not wanted")
	}
}

// A CONNECT the node's policy does not admit gets no answer, so no address at all.
func TestUnadmittedConnectGetsNothing(t *testing.T) {
	sig := &recSignal{}
	s := newResponder(sig, func(types.PeerID, []byte) bool { return false })
	defer s.Close()
	connectFrom(s, types.PeerID{1}, 1, "198.51.100.20:5000")
	if len(sig.answers()) != 0 {
		t.Fatal("an unadmitted peer got an answer")
	}
}
