package chat

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

type receiver struct {
	mu     sync.Mutex
	got    [][]byte
	status uint32
}

func (r *receiver) Receive(ctx context.Context, env []byte) uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, env)
	return r.status
}

func deliver(t *testing.T, req uint64, env []byte) frame.Frame {
	t.Helper()
	b, err := objects.Marshal(&chatpb.Deliver{Req: req, Envelope: env})
	if err != nil {
		t.Fatal(err)
	}
	return frame.Frame{Type: FrameDeliver, Payload: b}
}

func TestDeliverIsAnsweredWithTheStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []uint32{chatpb.StatusOK, chatpb.StatusDuplicate, chatpb.StatusNoSession, chatpb.StatusTooLarge, chatpb.StatusRefused} {
		r := &receiver{status: status}
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		srv := New(Config{Bundles: &bundler{}, Receiver: r, Now: c.Now})
		st, stop := run(t, srv, Peer{SID: [16]byte{1}})
		st.in <- deliver(t, 31, []byte("envelope"))
		wait(t, st, 1)
		out := st.frames()[0]
		stop()
		if out.Type != FrameDelivered {
			t.Fatalf("type %#x", out.Type)
		}
		m, err := chatpb.ParseDelivered(out.Payload)
		if err != nil || m.Req != 31 || m.Status != status {
			t.Fatalf("status %d: %v %v", status, m, err)
		}
		if len(r.got) != 1 || string(r.got[0]) != "envelope" {
			t.Fatalf("receiver got %q", r.got)
		}
	}
}

func TestDeliverWorksOverARelayedSession(t *testing.T) {
	t.Parallel()
	r := &receiver{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Receiver: r, Now: c.Now})
	st, stop := run(t, srv, Peer{SID: [16]byte{2}, Path: 1}) // PathRelay
	defer stop()
	st.in <- deliver(t, 1, []byte("e"))
	wait(t, st, 1)
}

func TestDeliverWithoutAReceiverIsRefused(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	st, stop := run(t, New(Config{Now: c.Now}), Peer{SID: [16]byte{3}})
	defer stop()
	st.in <- deliver(t, 1, []byte("e"))
	wait(t, st, 1)
	m, _ := chatpb.ParseDelivered(st.frames()[0].Payload)
	if m == nil || m.Status != chatpb.StatusRefused {
		t.Fatalf("%v", m)
	}
}

func TestDeliverMalformedClosesTheStream(t *testing.T) {
	t.Parallel()
	r := &receiver{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	for name, f := range map[string]frame.Frame{
		"garbage":                    {Type: FrameDeliver, Payload: []byte{0xff, 0xff}},
		"no body":                    {Type: FrameDeliver},
		"DELIVERED sent to a server": {Type: FrameDelivered, Payload: nil},
	} {
		st, stop := run(t, New(Config{Receiver: r, Now: c.Now}), Peer{SID: [16]byte{4}})
		st.in <- f
		waitDone(t, st)
		stop()
		if st.code == 0 {
			t.Errorf("%s: closed without a code", name)
		}
	}
	if len(r.got) != 0 {
		t.Fatal("a malformed frame reached the receiver")
	}
}

// 20 DELIVER per second per session (services.md 4.4); the quota refills with the clock.
func TestDeliverRateLimit(t *testing.T) {
	t.Parallel()
	r := &receiver{}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	srv := New(Config{Receiver: r, Now: c.Now})
	st, stop := run(t, srv, Peer{SID: [16]byte{5}})
	defer stop()
	for i := 0; i < DeliverRate; i++ {
		st.in <- deliver(t, uint64(i+1), []byte("e"))
	}
	wait(t, st, DeliverRate)
	st.in <- deliver(t, 99, []byte("e"))
	waitDone(t, st)
	if st.code != frame.CodeOverloaded {
		t.Fatalf("code %d, want overloaded", st.code)
	}
	if len(r.got) != DeliverRate {
		t.Fatalf("%d envelopes reached the receiver", len(r.got))
	}

	// a new stream of the same session shares the quota; a second later it is free again
	st2, stop2 := run(t, srv, Peer{SID: [16]byte{5}})
	st2.in <- deliver(t, 1, []byte("e"))
	waitDone(t, st2)
	stop2()
	if st2.code != frame.CodeOverloaded {
		t.Fatalf("the quota did not follow the session: %d", st2.code)
	}
	c.Add(1100 * time.Millisecond)
	st3, stop3 := run(t, srv, Peer{SID: [16]byte{5}})
	st3.in <- deliver(t, 1, []byte("e"))
	wait(t, st3, 1)
	stop3()
}

// A BUNDLE_GET that takes longer than 5 s is refused like any other refusal.
func TestBundleGetTimesOut(t *testing.T) {
	t.Parallel()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	block := make(chan struct{})
	b := &slowBundler{block: block}
	fire := make(chan time.Time)
	var asked time.Duration
	srv := New(Config{Bundles: b, Now: c.Now, After: func(d time.Duration) <-chan time.Time { asked = d; return fire }})
	st, stop := run(t, srv, direct("203.0.113.5:4430"))
	defer func() { close(block); stop() }()
	st.in <- get(t, 1)
	fire <- time.Time{}
	waitDone(t, st)
	if st.code != frame.CodeRefused || len(st.frames()) != 0 {
		t.Fatalf("code %d, frames %d", st.code, len(st.frames()))
	}
	if asked != BundleGetTimeout {
		t.Fatalf("timer of %v, want %v", asked, BundleGetTimeout)
	}
}

type slowBundler struct{ block chan struct{} }

func (b *slowBundler) Bundle() ([]byte, error) { <-b.block; return []byte("x"), nil }

// ---- StreamLink -----------------------------------------------------------------------------

// replyStream answers every DELIVER written to it with the given frames.
type replyStream struct {
	*fakeStream
	reply func(req uint64) []frame.Frame
}

func (s *replyStream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	_ = s.fakeStream.WriteFrame(typ, flags, payload)
	if typ == FrameDeliver {
		d, err := chatpb.ParseDeliver(payload)
		if err == nil {
			for _, f := range s.reply(d.Req) {
				s.in <- f
			}
		}
	}
	return nil
}

func delivered(t *testing.T, req uint64, status uint32) frame.Frame {
	t.Helper()
	b, _ := objects.Marshal(&chatpb.Delivered{Req: req, Status: status})
	return frame.Frame{Type: FrameDelivered, Payload: b}
}

func TestStreamLinkDeliver(t *testing.T) {
	t.Parallel()
	var dev [32]byte
	dev[0] = 7
	var opened [32]byte
	rs := &replyStream{fakeStream: newStream()}
	rs.reply = func(req uint64) []frame.Frame {
		// an answer to another request is ignored; the right one follows
		return []frame.Frame{delivered(t, req+100, chatpb.StatusRefused), delivered(t, req, chatpb.StatusDuplicate)}
	}
	l := &StreamLink{Open: func(ctx context.Context, d [32]byte) (Stream, error) { opened = d; return rs, nil }}
	got, err := l.Deliver(context.Background(), dev, []byte("env"))
	if err != nil || got != chatpb.StatusDuplicate {
		t.Fatalf("%d %v", got, err)
	}
	if opened != dev {
		t.Fatal("opened a stream to another device")
	}
	sent := rs.frames()
	if len(sent) != 1 || sent[0].Type != FrameDeliver {
		t.Fatalf("sent %v", sent)
	}
	if d, _ := chatpb.ParseDeliver(sent[0].Payload); d == nil || string(d.Envelope) != "env" {
		t.Fatal("the envelope was not sent")
	}
	if !rs.closed {
		t.Fatal("the stream was left open")
	}
}

func TestStreamLinkFailures(t *testing.T) {
	t.Parallel()
	// cannot open
	l := &StreamLink{Open: func(ctx context.Context, d [32]byte) (Stream, error) { return nil, errors.New("no path") }}
	if _, err := l.Deliver(context.Background(), [32]byte{}, []byte("e")); err == nil {
		t.Fatal("an unreachable device was not an error")
	}
	// a frame of another type
	rs := &replyStream{fakeStream: newStream()}
	rs.reply = func(req uint64) []frame.Frame { return []frame.Frame{{Type: FrameBundle}} }
	l = &StreamLink{Open: func(ctx context.Context, d [32]byte) (Stream, error) { return rs, nil }}
	if _, err := l.Deliver(context.Background(), [32]byte{}, []byte("e")); err == nil {
		t.Fatal("a wrong frame was taken as an answer")
	}
	// the peer closes the stream without an answer
	rs = &replyStream{fakeStream: newStream()}
	rs.reply = func(req uint64) []frame.Frame { close(rs.in); return nil }
	l = &StreamLink{Open: func(ctx context.Context, d [32]byte) (Stream, error) { return rs, nil }}
	if _, err := l.Deliver(context.Background(), [32]byte{}, []byte("e")); err == nil {
		t.Fatal("a closed stream was taken as delivered")
	}
}

func TestStreamLinkTimesOut(t *testing.T) {
	t.Parallel()
	rs := &replyStream{fakeStream: newStream()}
	rs.reply = func(req uint64) []frame.Frame { return nil } // never answers
	fire := make(chan time.Time, 1)
	var asked time.Duration
	l := &StreamLink{
		Open:  func(ctx context.Context, d [32]byte) (Stream, error) { return rs, nil },
		After: func(d time.Duration) <-chan time.Time { asked = d; return fire },
	}
	done := make(chan error, 1)
	go func() { _, err := l.Deliver(context.Background(), [32]byte{}, []byte("e")); done <- err }()
	fire <- time.Time{}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a timeout was not an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Deliver did not return after the timer fired")
	}
	if asked != DeliverTimeout {
		t.Fatalf("timer of %v, want %v", asked, DeliverTimeout)
	}
	if rs.code == 0 {
		t.Fatal("the stream was not closed with a code")
	}
}
