package mailbox

import (
	"context"
	"errors"
	"sync"
	"time"

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

var errNotImplemented = errors.New("mailbox: not implemented")

func (c *StreamConn) CreateQueue(ctx context.Context, m QueueCreate) (uint64, error) {
	return 0, errNotImplemented
}
func (c *StreamConn) Deposit(ctx context.Context, m Deposit) (Deposited, error) {
	return Deposited{}, errNotImplemented
}
func (c *StreamConn) Forward(ctx context.Context, m Forward) (Deposited, error) {
	return Deposited{}, errNotImplemented
}
func (c *StreamConn) Fetch(ctx context.Context, m Fetch) (Items, error) {
	return Items{}, errNotImplemented
}
func (c *StreamConn) Ack(ctx context.Context, m Ack) error { return errNotImplemented }
