package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// StreamLink is the Link over chat/2 streams: one DELIVER per stream, answered by DELIVERED.
type StreamLink struct {
	// Open opens a chat/2 stream to a device (node.Host.Open with service.Chat).
	Open func(ctx context.Context, device [32]byte) (Stream, error)
	// After is the timer (AD13); nil means time.After.
	After func(d time.Duration) <-chan time.Time

	req atomic.Uint64
}

// ErrNoAnswer is a DELIVER that got no DELIVERED: closed, timed out or answered with another frame.
var ErrNoAnswer = errors.New("chat: no DELIVERED")

// Deliver sends one envelope and waits for DELIVERED for DeliverTimeout.
func (l *StreamLink) Deliver(ctx context.Context, device [32]byte, envelope []byte) (uint32, error) {
	st, err := l.Open(ctx, device)
	if err != nil {
		return 0, err
	}
	req := l.req.Add(1)
	body, err := objects.Marshal(&chatpb.Deliver{Req: req, Envelope: envelope})
	if err != nil {
		_ = st.Close()
		return 0, err
	}
	if err := st.WriteFrame(FrameDeliver, 0, body); err != nil {
		_ = st.Close()
		return 0, err
	}
	type answer struct {
		status uint32
		err    error
	}
	got := make(chan answer, 1)
	go func() {
		for {
			f, err := st.ReadFrame()
			if err != nil {
				got <- answer{err: ErrNoAnswer}
				return
			}
			if f.Ignorable() {
				continue
			}
			if f.Type != FrameDelivered {
				got <- answer{err: ErrNoAnswer}
				return
			}
			d, err := chatpb.ParseDelivered(f.Payload)
			if err != nil {
				got <- answer{err: ErrNoAnswer}
				return
			}
			if d.Req != req {
				continue // the answer to another request
			}
			got <- answer{status: d.Status}
			return
		}
	}()
	after := l.After
	if after == nil {
		after = time.After
	}
	select {
	case a := <-got:
		if a.err != nil {
			_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
			return 0, a.err
		}
		_ = st.Close()
		return a.status, nil
	case <-after(DeliverTimeout):
		_ = st.CloseWithCode(frame.CodeTimeout, "")
		return 0, ErrNoAnswer
	case <-ctx.Done():
		_ = st.CloseWithCode(frame.CodeTimeout, "")
		return 0, ctx.Err()
	}
}
