package mailbox

import (
	"context"
	"errors"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// maxInFlight bounds concurrent requests on one service stream. A long-poll FETCH must not stop
// the stream from carrying a DEPOSIT or ACK (services.md §1: answers may come out of order), but a
// peer cannot open an unbounded number of goroutines either.
const maxInFlight = 32

// Handle serves one mailbox/2 stream. It owns the stream and closes it when the peer stops
// reading or the handler stops.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	ctx, cancel := context.WithCancel(ctx)
	var (
		wg  sync.WaitGroup
		wmu sync.Mutex
	)
	sem := make(chan struct{}, maxInFlight)

loop:
	for {
		f, err := st.ReadFrame()
		if err != nil {
			break
		}
		if f.Type != FrameFetch {
			// Everything except a long-poll FETCH is short: handling it inline keeps the
			// order a client sees on one stream (in particular ACK before the next FETCH).
			s.handleFrame(ctx, p, st, &wmu, f)
			continue
		}
		// A long-poll FETCH waits up to 60 s. It must not stop the stream from carrying a
		// DEPOSIT, ACK or another request (services.md §1: answers may come out of order).
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		default:
			s.fail(st, frame.CodeOverloaded)
			break loop
		}
		wg.Add(1)
		go func(f frame.Frame) {
			defer wg.Done()
			defer func() { <-sem }()
			s.handleFrame(ctx, p, st, &wmu, f)
		}(f)
	}

	cancel()
	_ = st.Close()
	wg.Wait()
}

func (s *Server) handleFrame(ctx context.Context, p Peer, st *mux.Stream, wmu *sync.Mutex, f frame.Frame) {
	switch f.Type {
	case FrameQueueCreate:
		m, err := ParseQueueCreate(f.Payload)
		if err != nil {
			s.fail(st, frame.CodeProtocolViolation)
			return
		}
		expires, err := s.CreateQueue(p, m)
		if err != nil {
			s.respondErr(st, wmu, m.Req, err)
			return
		}
		s.write(wmu, st, FrameQueueOK, QueueOK{Req: m.Req, Expires: expires}.Append(nil))

	case FrameDeposit:
		m, err := ParseDeposit(f.Payload)
		if err != nil {
			s.fail(st, frame.CodeProtocolViolation)
			return
		}
		status, item, err := s.Deposit(p, m)
		if err != nil {
			s.respondErr(st, wmu, m.Req, err)
			return
		}
		s.write(wmu, st, FrameDeposited, Deposited{Req: m.Req, Status: status, Item: item}.Append(nil))

	case FrameFetch:
		m, err := ParseFetch(f.Payload)
		if err != nil {
			s.fail(st, frame.CodeProtocolViolation)
			return
		}
		items, more, err := s.Fetch(ctx, p, m)
		if err != nil {
			s.respondErr(st, wmu, m.Req, err)
			return
		}
		s.write(wmu, st, FrameItems, Items{Req: m.Req, Items: items, More: more}.Append(nil))

	case FrameAck:
		m, err := ParseAck(f.Payload)
		if err != nil {
			s.fail(st, frame.CodeProtocolViolation)
			return
		}
		if err := s.Ack(p, m); err != nil {
			s.respondErr(st, wmu, m.Req, err)
		}
		// A successful ACK has no answer frame in services.md §5.1.

	case FrameForward:
		m, err := ParseForward(f.Payload)
		if err != nil {
			s.fail(st, frame.CodeProtocolViolation)
			return
		}
		status, err := s.Forward(ctx, p, m)
		if err != nil {
			s.respondErr(st, wmu, m.Req, err)
			return
		}
		s.write(wmu, st, FrameDeposited, Deposited{Req: m.Req, Status: status}.Append(nil))

	case FrameError:
		// ERROR is a response type; receiving one means the peer is confused about who answers.
		s.fail(st, frame.CodeProtocolViolation)

	default:
		// The stream limiter knows only the nine mailbox/2 types, so an unknown type reaches us
		// only when it is IGNORABLE; such a frame is simply skipped. A non-ignorable one has
		// already ended the stream in mux.Stream.ReadFrame.
		if !f.Ignorable() {
			s.fail(st, frame.CodeUnknownFrame)
		}
	}
}

// respondErr maps a service error to its wire answer (services.md §5 and §1).
func (s *Server) respondErr(st *mux.Stream, wmu *sync.Mutex, req uint64, err error) {
	switch {
	case errors.Is(err, ErrAuth):
		// A signature for another session or queue is not retryable on this stream.
		_ = st.CloseWithCode(frame.CodeRefused, "")
	case errors.Is(err, ErrPowInvalid):
		s.write(wmu, st, FrameError, Error{Req: req, Code: CodePowInvalid}.Append(nil))
	case errors.Is(err, ErrBadRequest), errors.Is(err, ErrBadFrame), errors.Is(err, ErrCorrupt):
		_ = st.CloseWithCode(frame.CodeProtocolViolation, "")
	case errors.Is(err, ErrUnknownQueue), errors.Is(err, ErrQueueExists), errors.Is(err, ErrRefused):
		s.write(wmu, st, FrameError, Error{Req: req, Code: uint32(frame.CodeRefused)}.Append(nil))
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The stream is going away; the FETCH is abandoned (services.md §1).
	default:
		s.write(wmu, st, FrameError, Error{Req: req, Code: uint32(frame.CodeInternal)}.Append(nil))
	}
}

func (s *Server) fail(st *mux.Stream, code uint64) {
	_ = st.CloseWithCode(code, "")
}

func (s *Server) write(wmu *sync.Mutex, st *mux.Stream, typ uint64, payload []byte) {
	wmu.Lock()
	defer wmu.Unlock()
	_ = st.WriteFrame(typ, 0, payload)
}
