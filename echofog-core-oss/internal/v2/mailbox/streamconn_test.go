package mailbox

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// scriptStream answers every frame written to it with the frames its script returns.
type scriptStream struct {
	in     chan frame.Frame
	mu     sync.Mutex
	out    []frame.Frame
	code   uint64
	closed chan struct{}
	once   sync.Once
	script func(f frame.Frame) []frame.Frame
}

func newScript(script func(f frame.Frame) []frame.Frame) *scriptStream {
	return &scriptStream{in: make(chan frame.Frame, 16), closed: make(chan struct{}), script: script}
}

func (s *scriptStream) ReadFrame() (frame.Frame, error) {
	select {
	case f := <-s.in:
		return f, nil
	case <-s.closed:
		return frame.Frame{}, io.EOF
	}
}

func (s *scriptStream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	f := frame.Frame{Type: typ, Flags: flags, Payload: payload}
	s.mu.Lock()
	s.out = append(s.out, f)
	s.mu.Unlock()
	for _, r := range s.script(f) {
		s.in <- r
	}
	return nil
}

func (s *scriptStream) CloseWithCode(code uint64, reason string) error {
	s.mu.Lock()
	s.code = code
	s.mu.Unlock()
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *scriptStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func reply(typ uint64, payload []byte) frame.Frame { return frame.Frame{Type: typ, Payload: payload} }

func TestStreamConnRequestsAndAnswers(t *testing.T) {
	st := newScript(func(f frame.Frame) []frame.Frame {
		switch f.Type {
		case FrameQueueCreate:
			m, _ := ParseQueueCreate(f.Payload)
			return []frame.Frame{reply(FrameQueueOK, QueueOK{Req: m.Req, Expires: 77}.Append(nil))}
		case FrameDeposit:
			m, _ := ParseDeposit(f.Payload)
			// an answer to another request comes first and is ignored
			return []frame.Frame{reply(FrameDeposited, Deposited{Req: m.Req + 5, Status: StatusFull}.Append(nil)),
				reply(FrameDeposited, Deposited{Req: m.Req, Status: StatusStored, Item: []byte{1}}.Append(nil))}
		case FrameForward:
			m, _ := ParseForward(f.Payload)
			return []frame.Frame{reply(FrameDeposited, Deposited{Req: m.Req, Status: StatusRelayUnreachable}.Append(nil))}
		case FrameFetch:
			m, _ := ParseFetch(f.Payload)
			return []frame.Frame{reply(FrameItems, Items{Req: m.Req, Items: []Item{{ID: make([]byte, 16), Blob: []byte("b")}}, More: true}.Append(nil))}
		}
		return nil
	})
	c := NewStreamConn(st, [16]byte{3}, nil)
	ctx := context.Background()
	if c.SID() != ([16]byte{3}) {
		t.Fatal("SID")
	}
	if exp, err := c.CreateQueue(ctx, QueueCreate{Req: 1, Queue: make([]byte, 16), AuthKey: make([]byte, 32)}); err != nil || exp != 77 {
		t.Fatalf("CreateQueue: %d %v", exp, err)
	}
	if d, err := c.Deposit(ctx, Deposit{Req: 2, Queue: make([]byte, 16), Blob: []byte("x")}); err != nil || d.Status != StatusStored || d.Req != 2 {
		t.Fatalf("Deposit: %+v %v", d, err)
	}
	if d, err := c.Forward(ctx, Forward{Req: 3, Relay: make([]byte, 32), Sealed: make([]byte, 60)}); err != nil || d.Status != StatusRelayUnreachable {
		t.Fatalf("Forward: %+v %v", d, err)
	}
	if it, err := c.Fetch(ctx, Fetch{Req: 4, Queue: make([]byte, 16), Max: 10}); err != nil || len(it.Items) != 1 || !it.More {
		t.Fatalf("Fetch: %+v %v", it, err)
	}
	// ACK has no answer: it returns at once and the frame was written
	if err := c.Ack(ctx, Ack{Req: 5, Queue: make([]byte, 16), IDs: [][]byte{make([]byte, 16)}}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	st.mu.Lock()
	last := st.out[len(st.out)-1]
	st.mu.Unlock()
	if last.Type != FrameAck {
		t.Fatalf("last frame 0x%x", last.Type)
	}
}

func TestStreamConnErrors(t *testing.T) {
	st := newScript(func(f frame.Frame) []frame.Frame {
		m, _ := ParseDeposit(f.Payload)
		return []frame.Frame{reply(FrameError, Error{Req: m.Req, Code: CodePowInvalid}.Append(nil))}
	})
	c := NewStreamConn(st, [16]byte{}, nil)
	_, err := c.Deposit(context.Background(), Deposit{Req: 9, Queue: make([]byte, 16), Blob: []byte("x")})
	var re *RemoteError
	if !errors.As(err, &re) || re.Code != CodePowInvalid {
		t.Fatalf("%v", err)
	}

	// a frame of another kind, and a closed stream, are no answer
	st = newScript(func(f frame.Frame) []frame.Frame { return []frame.Frame{reply(FrameItems, nil)} })
	c = NewStreamConn(st, [16]byte{}, nil)
	if _, err := c.Deposit(context.Background(), Deposit{Req: 1, Queue: make([]byte, 16)}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a wrong frame: %v", err)
	}
	st = newScript(func(f frame.Frame) []frame.Frame { return nil })
	_ = st.Close()
	c = NewStreamConn(st, [16]byte{}, nil)
	if _, err := c.Fetch(context.Background(), Fetch{Req: 1, Queue: make([]byte, 16)}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("a closed stream: %v", err)
	}
}

func TestStreamConnTimeouts(t *testing.T) {
	cases := []struct {
		name string
		want time.Duration
		call func(c *StreamConn) error
	}{
		{"QUEUE_CREATE", CreateTimeout, func(c *StreamConn) error {
			_, err := c.CreateQueue(context.Background(), QueueCreate{Req: 1, Queue: make([]byte, 16), AuthKey: make([]byte, 32)})
			return err
		}},
		{"DEPOSIT", DepositTimeout, func(c *StreamConn) error {
			_, err := c.Deposit(context.Background(), Deposit{Req: 1, Queue: make([]byte, 16)})
			return err
		}},
		{"FORWARD", ForwardTimeout, func(c *StreamConn) error {
			_, err := c.Forward(context.Background(), Forward{Req: 1, Relay: make([]byte, 32), Sealed: make([]byte, 60)})
			return err
		}},
		{"FETCH wait 30", 30*time.Second + FetchGrace, func(c *StreamConn) error {
			_, err := c.Fetch(context.Background(), Fetch{Req: 1, Queue: make([]byte, 16), Wait: 30})
			return err
		}},
	}
	for _, tc := range cases {
		st := newScript(func(f frame.Frame) []frame.Frame { return nil }) // never answers
		fire := make(chan time.Time, 1)
		var asked time.Duration
		c := NewStreamConn(st, [16]byte{}, func(d time.Duration) <-chan time.Time { asked = d; return fire })
		done := make(chan error, 1)
		go func() { done <- tc.call(c) }()
		fire <- time.Time{}
		select {
		case err := <-done:
			if !errors.Is(err, ErrNoAnswer) {
				t.Errorf("%s: %v", tc.name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: no return after the timer", tc.name)
		}
		if asked != tc.want {
			t.Errorf("%s: timer of %v, want %v", tc.name, asked, tc.want)
		}
		if st.code != frame.CodeTimeout {
			t.Errorf("%s: stream closed with code %d", tc.name, st.code)
		}
	}
}
