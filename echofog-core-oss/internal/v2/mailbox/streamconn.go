package mailbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Stream is the part of *mux.Stream a client uses.
type Stream interface {
	ReadFrame() (frame.Frame, error)
	WriteFrame(typ uint64, flags byte, payload []byte) error
	CloseWithCode(code uint64, reason string) error
	Close() error
}

// RemoteError is an ERROR frame of the relay.
type RemoteError struct{ Code uint32 }

func (e *RemoteError) Error() string { return "mailbox: relay error " + itoa(e.Code) }

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// ErrNoAnswer is a request that got no answer: the stream closed or the time of services.md 5.1 passed.
var ErrNoAnswer = errors.New("mailbox: no answer")

// Timeouts (services.md 5.1); FETCH is its wait plus FetchGrace.
const (
	CreateTimeout  = 10 * time.Second
	DepositTimeout = 10 * time.Second
	AckTimeout     = 10 * time.Second
	FetchGrace     = 5 * time.Second
)

// StreamConn is a Conn over one mailbox/2 stream. Calls are made one at a time.
type StreamConn struct {
	st    Stream
	sid   [16]byte
	after func(time.Duration) <-chan time.Time
	mu    sync.Mutex
}

// NewStreamConn wraps a stream of the session sid. after is the timer (nil means time.After).
func NewStreamConn(st Stream, sid [16]byte, after func(time.Duration) <-chan time.Time) *StreamConn {
	if after == nil {
		after = time.After
	}
	return &StreamConn{st: st, sid: sid, after: after}
}

func (c *StreamConn) SID() [16]byte { return c.sid }
func (c *StreamConn) Close() error  { return c.st.Close() }

// call writes a request and waits for the answer with the same req: want is the answer frame type.
func (c *StreamConn) call(ctx context.Context, typ uint64, body []byte, req uint64, want uint64, limit time.Duration) (frame.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.st.WriteFrame(typ, 0, body); err != nil {
		return frame.Frame{}, err
	}
	type result struct {
		f   frame.Frame
		err error
	}
	got := make(chan result, 1)
	go func() {
		for {
			f, err := c.st.ReadFrame()
			if err != nil {
				got <- result{err: ErrNoAnswer}
				return
			}
			if f.Ignorable() {
				continue
			}
			switch f.Type {
			case FrameError:
				e, perr := ParseError(f.Payload)
				if perr != nil {
					got <- result{err: ErrNoAnswer}
					return
				}
				if e.Req != req {
					continue
				}
				got <- result{err: &RemoteError{Code: e.Code}}
				return
			case want:
				// the request number is the first field of every answer
				if r, ok := answerReq(f.Payload); ok && r != req {
					continue
				}
				got <- result{f: f}
				return
			default:
				got <- result{err: ErrNoAnswer}
				return
			}
		}
	}()
	select {
	case r := <-got:
		return r.f, r.err
	case <-c.after(limit):
		_ = c.st.CloseWithCode(frame.CodeTimeout, "")
		return frame.Frame{}, ErrNoAnswer
	case <-ctx.Done():
		_ = c.st.CloseWithCode(frame.CodeTimeout, "")
		return frame.Frame{}, ctx.Err()
	}
}

// answerReq reads field 1 (req) of an answer.
func answerReq(p []byte) (uint64, bool) {
	var req uint64
	var found bool
	_ = pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		if num == 1 && typ == protowire.VarintType {
			req, found = v, true
		}
		return nil
	})
	return req, found
}

func (c *StreamConn) CreateQueue(ctx context.Context, m QueueCreate) (uint64, error) {
	f, err := c.call(ctx, FrameQueueCreate, m.Append(nil), m.Req, FrameQueueOK, CreateTimeout)
	if err != nil {
		return 0, err
	}
	ok, err := ParseQueueOK(f.Payload)
	if err != nil {
		return 0, err
	}
	return ok.Expires, nil
}

func (c *StreamConn) Deposit(ctx context.Context, m Deposit) (Deposited, error) {
	f, err := c.call(ctx, FrameDeposit, m.Append(nil), m.Req, FrameDeposited, DepositTimeout)
	if err != nil {
		return Deposited{}, err
	}
	return ParseDeposited(f.Payload)
}

func (c *StreamConn) Forward(ctx context.Context, m Forward) (Deposited, error) {
	f, err := c.call(ctx, FrameForward, m.Append(nil), m.Req, FrameDeposited, ForwardTimeout)
	if err != nil {
		return Deposited{}, err
	}
	return ParseDeposited(f.Payload)
}

func (c *StreamConn) Fetch(ctx context.Context, m Fetch) (Items, error) {
	f, err := c.call(ctx, FrameFetch, m.Append(nil), m.Req, FrameItems, time.Duration(m.Wait)*time.Second+FetchGrace)
	if err != nil {
		return Items{}, err
	}
	return ParseItems(f.Payload)
}

// Ack sends ACK, which has no answer (decisions.md T7.3). Later calls on the stream are handled after
// it, and an error of the relay ends the stream, which the next call reports.
func (c *StreamConn) Ack(ctx context.Context, m Ack) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.WriteFrame(FrameAck, 0, m.Append(nil))
}
