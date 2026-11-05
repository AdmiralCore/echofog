package call

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/keys"
)

type fakePair struct {
	done chan struct{}
	once sync.Once
}

func (p *fakePair) close() { p.once.Do(func() { close(p.done) }) }

type fakeStream struct {
	pair *fakePair
	in   chan frame.Frame
	out  *fakeStream
}

func newPair() (*fakeStream, *fakeStream) {
	p := &fakePair{done: make(chan struct{})}
	a := &fakeStream{pair: p, in: make(chan frame.Frame, 32)}
	b := &fakeStream{pair: p, in: make(chan frame.Frame, 32)}
	a.out, b.out = b, a
	return a, b
}

func (s *fakeStream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	f := frame.Frame{Type: typ, Flags: flags, Payload: append([]byte(nil), payload...)}
	select {
	case s.out.in <- f:
		return nil
	case <-s.pair.done:
		return io.ErrClosedPipe
	}
}

func (s *fakeStream) ReadFrame() (frame.Frame, error) {
	select {
	case f := <-s.in:
		return f, nil
	case <-s.pair.done:
		return frame.Frame{}, io.EOF
	}
}

func (s *fakeStream) SetLimiter(frame.Limiter) {}
func (s *fakeStream) Close() error             { s.pair.close(); return nil }
func (s *fakeStream) CloseWithCode(uint64, string) error {
	return s.Close()
}

type fakePort struct {
	in     chan Datagram
	sent   chan Datagram
	closed chan struct{}
	once   sync.Once
}

func newFakePort() *fakePort {
	return &fakePort{
		in:     make(chan Datagram, 16),
		sent:   make(chan Datagram, 16),
		closed: make(chan struct{}),
	}
}

func (p *fakePort) Send(ctx context.Context, peer [32]byte, typ uint64, payload []byte) error {
	d := Datagram{Peer: Peer{Device: peer}, Type: typ, Payload: append([]byte(nil), payload...)}
	select {
	case p.sent <- d:
		return nil
	case <-p.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *fakePort) Receive(ctx context.Context) (Datagram, error) {
	select {
	case d := <-p.in:
		return d, nil
	case <-p.closed:
		return Datagram{}, io.ErrClosedPipe
	case <-ctx.Done():
		return Datagram{}, ctx.Err()
	}
}

func (p *fakePort) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// pairOpen wires two services: Open returns one end to the caller and serves the other end in
// the peer's service.
func pairOpen(t *testing.T, peer *Service, peerDevice, localDevice []byte) func(context.Context, [32]byte) (Peer, Stream, error) {
	t.Helper()
	return func(ctx context.Context, device [32]byte) (Peer, Stream, error) {
		a, b := newPair()
		go peer.Handle(ctx, Peer{Device: id32(localDevice)}, b)
		return Peer{Device: id32(peerDevice)}, a, nil
	}
}

func id32(b []byte) [32]byte {
	var id [32]byte
	copy(id[:], b)
	return id
}

func newCallService(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestServiceSendsAndReceivesForwardedSignal(t *testing.T) {
	aSigner, bSigner := testSigner(t, 30), testSigner(t, 31)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	got := make(chan *pb.CallSignal, 4)
	rawCh := make(chan []byte, 4)

	b := newCallService(t, Config{
		Origin: bSigner.Public(), Sign: signFn(bSigner),
		OnSignal: func(_ context.Context, _ Peer, m *pb.CallSignal, raw []byte) {
			got <- m
			rawCh <- raw
		},
	})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID),
	})

	m := &pb.CallSignal{
		Action: ActionStreamAnnounce, CallId: "call-1",
		Payload: &pb.CallSignal_StreamAnnounce{StreamAnnounce: &pb.StreamAnnounce{
			StreamId: "s1", Broadcaster: aID, MediaType: "screen",
		}},
	}
	if err := a.Send(context.Background(), Peer{Device: deviceID(bSigner.Public())}, m); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		if !bytes.Equal(rec.Sender, aID) {
			t.Fatalf("sender %x, want %x", rec.Sender, aID)
		}
		if rec.Action != ActionStreamAnnounce || rec.MsgId == 0 || rec.Timestamp == 0 {
			t.Fatalf("received %+v", rec)
		}
		raw := <-rawCh
		if len(raw) == 0 {
			t.Fatal("no raw signal")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signal not delivered")
	}
}

func TestForgedForwardedSignalIsDropped(t *testing.T) {
	signer := testSigner(t, 32)
	self := deviceBytes(signer.Public())
	var delivered int
	var mu sync.Mutex
	b := newCallService(t, Config{
		Origin: signer.Public(), Sign: signFn(signer),
		OnSignal: func(context.Context, Peer, *pb.CallSignal, []byte) {
			mu.Lock()
			delivered++
			mu.Unlock()
		},
	})
	peer := Peer{Device: deviceID(signer.Public())}

	// A valid signal for reference.
	validMsg := testPayload(ActionStreamAnnounce)
	validMsg.Sender = self
	validRaw, err := Encode(validMsg, signer.Public(), signFn(signer))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(context.Background(), peer, validRaw); err != nil {
		t.Fatalf("valid signal: %v", err)
	}

	// A forged one: the body was changed after it was signed.
	s, err := Parse(validRaw)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(nil), s.Body...)
	idx := bytes.Index(body, []byte("screen"))
	if idx < 0 {
		t.Fatal("payload not found")
	}
	body[idx] = 'S'
	forged, err := marshalSignal(&pb.Signal{Origin: s.Origin, Body: body, Sig: s.Sig})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(context.Background(), peer, forged); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("forged signal: %v, want ErrBadSignature", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 {
		t.Fatalf("delivered %d signals, want 1 (the forged one dropped)", delivered)
	}
}

func TestProcessDedupesStreamAndDatagramCopies(t *testing.T) {
	signer := testSigner(t, 33)
	self := deviceBytes(signer.Public())
	got := make(chan *pb.CallSignal, 4)
	b := newCallService(t, Config{
		Origin: signer.Public(), Sign: signFn(signer),
		OnSignal: func(_ context.Context, _ Peer, m *pb.CallSignal, _ []byte) { got <- m },
	})
	peer := Peer{Device: deviceID(signer.Public())}
	m := testPayload(ActionNack)
	m.Sender = self
	raw, err := Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(context.Background(), peer, raw); err != nil {
		t.Fatal(err)
	}
	if err := b.Process(context.Background(), peer, raw); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate: %v, want ErrDuplicate", err)
	}
	// A different message from the same sender is not a duplicate.
	m2 := testPayload(ActionReport)
	m2.Sender = self
	m2.MsgId = 8
	raw2, err := Encode(m2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Process(context.Background(), peer, raw2); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("delivered %d signals, want 2", len(got))
	}
}

func TestDatagramPortDeliversSignals(t *testing.T) {
	signer := testSigner(t, 34)
	self := deviceBytes(signer.Public())
	port := newFakePort()
	got := make(chan *pb.CallSignal, 4)
	b := newCallService(t, Config{
		Origin: signer.Public(), Sign: signFn(signer), Datagrams: port,
		OnSignal: func(_ context.Context, _ Peer, m *pb.CallSignal, _ []byte) { got <- m },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.ReceiveDatagrams(ctx) }()

	m := testPayload(ActionNack)
	m.Sender = self
	raw, err := Encode(m, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := Peer{Device: deviceID(signer.Public())}
	port.in <- Datagram{Peer: peer, Type: frame.DgramCallSignal, Payload: raw}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("datagram not delivered")
	}
	// A duplicate datagram is dropped.
	port.in <- Datagram{Peer: peer, Type: frame.DgramCallSignal, Payload: raw}
	// A forged datagram is dropped as well.
	forged := append([]byte(nil), raw...)
	forged[len(forged)-1] ^= 0xff
	port.in <- Datagram{Peer: peer, Type: frame.DgramCallSignal, Payload: forged}
	m2 := testPayload(ActionReport)
	m2.Sender = self
	m2.MsgId = 9
	raw2, err := Encode(m2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	port.in <- Datagram{Peer: peer, Type: frame.DgramCallSignal, Payload: raw2}
	select {
	case rec := <-got:
		if rec.MsgId != 9 {
			t.Fatalf("delivered message %d, want 9 (duplicate and forged dropped)", rec.MsgId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second datagram not delivered")
	}
	select {
	case rec := <-got:
		t.Fatalf("unexpected extra signal %d", rec.MsgId)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSendDuplicatesLossySignalsOnly(t *testing.T) {
	aSigner, bSigner := testSigner(t, 35), testSigner(t, 36)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	port := newFakePort()
	b := newCallService(t, Config{Origin: bSigner.Public(), Sign: signFn(bSigner)})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID), Datagrams: port,
	})
	peer := Peer{Device: deviceID(bSigner.Public())}

	nack := testPayload(ActionNack)
	if err := a.Send(context.Background(), peer, nack); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-port.sent:
		if d.Type != frame.DgramCallSignal {
			t.Fatalf("datagram type %#x", d.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("lossy signal was not duplicated on CALL_SIGNAL")
	}

	ping := testPayload(ActionPing)
	if err := a.Send(context.Background(), peer, ping); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-port.sent:
		t.Fatalf("hop-local ping went by datagram: %x", d.Payload)
	case <-time.After(50 * time.Millisecond):
	}

	// A NACK too large for a datagram still goes by stream.
	big := testPayload(ActionNack)
	big.GetNack().Lost = make([]*pb.NackEntry, 200)
	for i := range big.GetNack().Lost {
		big.GetNack().Lost[i] = &pb.NackEntry{Seq: uint32(i), Have: 0xffff}
	}
	if err := a.Send(context.Background(), peer, big); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-port.sent:
		t.Fatalf("oversized NACK went by datagram (%d bytes)", len(d.Payload))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRequestTimesOutAfterTwoSecondsByDefault(t *testing.T) {
	if DefaultRequestTimeout != 2*time.Second {
		t.Fatalf("default request timeout %v, want 2s", DefaultRequestTimeout)
	}
	aSigner, bSigner := testSigner(t, 37), testSigner(t, 38)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	b := newCallService(t, Config{Origin: bSigner.Public(), Sign: signFn(bSigner)})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID), RequestTimeout: 30 * time.Millisecond,
	})
	ping := testPayload(ActionPing)
	start := time.Now()
	_, err := a.Request(context.Background(), Peer{Device: deviceID(bSigner.Public())}, ping)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("request: %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}
}

func TestRequestReceivesAnswer(t *testing.T) {
	aSigner, bSigner := testSigner(t, 39), testSigner(t, 40)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	var b *Service
	b = newCallService(t, Config{
		Origin: bSigner.Public(), Sign: signFn(bSigner),
		OnSignal: func(ctx context.Context, p Peer, m *pb.CallSignal, _ []byte) {
			if m.Action != ActionPing {
				return
			}
			_ = b.Send(ctx, p, &pb.CallSignal{Action: ActionPong, CallId: m.CallId, Req: m.Req})
		},
	})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID), RequestTimeout: time.Second,
	})
	ping := testPayload(ActionPing)
	answer, err := a.Request(context.Background(), Peer{Device: deviceID(bSigner.Public())}, ping)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Action != ActionPong || answer.Req != ping.Req {
		t.Fatalf("answer %+v", answer)
	}
}

func TestForwardWritesExactBytes(t *testing.T) {
	aSigner, bSigner := testSigner(t, 41), testSigner(t, 42)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	got := make(chan []byte, 1)
	b := newCallService(t, Config{
		Origin: bSigner.Public(), Sign: signFn(bSigner),
		OnSignal: func(_ context.Context, _ Peer, _ *pb.CallSignal, raw []byte) { got <- raw },
	})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID),
	})
	m := testPayload(ActionTreeManifest)
	m.Sender = aID
	raw, err := Encode(m, aSigner.Public(), signFn(aSigner))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Forward(context.Background(), Peer{Device: deviceID(bSigner.Public())}, raw); err != nil {
		t.Fatal(err)
	}
	select {
	case rec := <-got:
		if !bytes.Equal(rec, raw) {
			t.Fatal("forwarded bytes changed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("forwarded signal not delivered")
	}
}

func TestNewRequiresOriginAndSigner(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted an empty config")
	}
	s := testSigner(t, 43)
	if _, err := New(Config{Origin: s.Public()}); err == nil {
		t.Fatal("New accepted a config without a signer")
	}
	if _, err := New(Config{Origin: keys.New(keys.X25519, make([]byte, 32)), Sign: signFn(s)}); err == nil {
		t.Fatal("New accepted a non-Ed25519 origin")
	}
}

func TestConcurrentSendsFromOneService(t *testing.T) {
	aSigner, bSigner := testSigner(t, 44), testSigner(t, 45)
	aID, bID := deviceBytes(aSigner.Public()), deviceBytes(bSigner.Public())
	b := newCallService(t, Config{Origin: bSigner.Public(), Sign: signFn(bSigner)})
	a := newCallService(t, Config{
		Origin: aSigner.Public(), Sign: signFn(aSigner),
		Open: pairOpen(t, b, bID, aID),
	})
	peer := Peer{Device: deviceID(bSigner.Public())}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.Send(context.Background(), peer, testPayload(ActionVADState)); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()
}
