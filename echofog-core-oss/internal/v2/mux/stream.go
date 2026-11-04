package mux

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

var (
	ErrPathFailed         = errors.New("mux: path failed")
	ErrGoingAway          = errors.New("mux: session is going away")
	ErrNoPath             = errors.New("mux: session has no live path")
	ErrSessionClosed      = errors.New("mux: session closed")
	ErrProtocolViolation  = errors.New("mux: protocol violation")
	ErrFeatureNotSelected = errors.New("mux: feature not negotiated")
)

// CloseError: the stream ended with CLOSE{code, reason} (wire.md §6.3).
type CloseError struct {
	Code   uint64
	Reason string
	Remote bool
}

func (e *CloseError) Error() string {
	side := "local"
	if e.Remote {
		side = "peer"
	}
	return fmt.Sprintf("mux: stream closed by %s: code %d %q", side, e.Code, e.Reason)
}

// UnsupportedError: the peer answered OPEN with UNSUPPORTED (it does not serve the pair).
type UnsupportedError struct{ frame.Unsupported }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("mux: service %d unsupported by peer (serves %d..%d)", e.Service, e.Min, e.Max)
}

// Stream is one stream of a session: a sequence of stream frames (wire.md §6.2) over a carrier
// stream, sealed into records when its path is in sealed mode (wire.md §8.5).
type Stream struct {
	sess    *Session
	path    *path
	raw     RawStream
	rd      *frame.Reader
	w       io.Writer
	control bool
	local   bool

	open     frame.Open
	selected bool

	wmu  sync.Mutex
	wbuf []byte

	mu        sync.Mutex
	readDone  bool
	writeDone bool
	released  bool
	err       error // terminal error, once known
}

func (s *Session) newStream(p *path, raw RawStream, local, control bool) *Stream {
	st := &Stream{sess: s, path: p, raw: raw, local: local, control: control}
	if p.pipeSeal {
		rc := newPipeRecordConn(raw, s.sealer, p.id, raw.ID())
		st.rd = frame.NewReader(rc, nil)
		st.w = rc
	} else {
		st.rd = frame.NewReader(raw, nil)
		st.w = raw
	}
	return st
}

// Open returns the stream's OPEN (service, version, flags).
func (st *Stream) Open() frame.Open { return st.open }

// Selected reports whether the stream's (service, version) was selected for this session.
func (st *Stream) Selected() bool { return st.selected }

// PathID returns the ID of the path that carries the stream.
func (st *Stream) PathID() uint64 { return st.path.id }

// SetLimiter sets the service's frame limiter (the maximum payload of each service-local type).
func (st *Stream) SetLimiter(l frame.Limiter) { st.rd.Lim = l }

// SetReadDeadline sets the read deadline of the carrier stream.
func (st *Stream) SetReadDeadline(t time.Time) error { return st.raw.SetReadDeadline(t) }

// WriteFrame writes one frame.
func (st *Stream) WriteFrame(typ uint64, flags byte, payload []byte) error {
	if typ < frame.ServiceLocalMin {
		if info, ok := frame.SessionType(typ); ok && len(payload) > info.Max {
			return fmt.Errorf("mux: %s payload %d exceeds %d", info.Name, len(payload), info.Max)
		}
	}
	if len(payload) > frame.MaxPayload {
		return fmt.Errorf("mux: payload %d exceeds 1 MiB", len(payload))
	}
	st.wmu.Lock()
	defer st.wmu.Unlock()
	st.wbuf = frame.AppendFrame(st.wbuf[:0], frame.Frame{Type: typ, Flags: flags, Payload: payload})
	_, err := st.w.Write(st.wbuf)
	if err == nil {
		st.path.touchSend()
	}
	return st.mapErr(err)
}

// WritePadding writes a PADDING frame of n zero bytes (a sender policy, wire.md §6.4).
func (st *Stream) WritePadding(n int) error {
	return st.WriteFrame(frame.TypePadding, frame.FlagIgnorable, make([]byte, min(n, 16384)))
}

// WriteGrease writes an IGNORABLE frame of the GREASE type with random payload (D18).
func (st *Stream) WriteGrease() error {
	var nb [1]byte
	rand.Read(nb[:])
	p := make([]byte, int(nb[0])%frame.GreaseMaxPayload)
	rand.Read(p)
	return st.WriteFrame(frame.TypeGrease, frame.FlagIgnorable, p)
}

// ReadFrame returns the next frame for the service. It skips PADDING and unknown IGNORABLE
// frames; it answers an unknown frame without IGNORABLE with CLOSE{unknown-frame} and a
// too-long frame with CLOSE{frame-too-large}; it returns *CloseError for a received CLOSE and
// *UnsupportedError for UNSUPPORTED. Only malformed or misplaced frames of known types are
// reported as abuse.
func (st *Stream) ReadFrame() (frame.Frame, error) {
	for {
		f, err := st.rd.ReadFrame()
		if err != nil {
			return frame.Frame{}, st.readErr(err)
		}
		st.path.touchRecv()
		switch {
		case f.Type == frame.TypePadding:
			continue
		case f.Type >= frame.ServiceLocalMin:
			return f, nil
		case f.Type == frame.TypeClose:
			c, perr := frame.ParseClose(f.Payload)
			if perr != nil {
				st.abuse(f.Type, perr)
				c = frame.Close{Code: frame.CodeProtocolViolation}
			}
			ce := &CloseError{Code: c.Code, Reason: c.Reason, Remote: true}
			st.finish(ce)
			st.raw.Reset(c.Code)
			return frame.Frame{}, ce
		case f.Type == frame.TypeUnsupported:
			u, perr := frame.ParseUnsupported(f.Payload)
			if perr != nil {
				st.abuse(f.Type, perr)
				return frame.Frame{}, st.violation(perr)
			}
			ue := &UnsupportedError{u}
			st.finish(ue)
			st.raw.Close()
			return frame.Frame{}, ue
		}
		// A known session-level type that is not valid on a service stream.
		err = fmt.Errorf("%w: %s on a service stream", ErrProtocolViolation, frame.TypeName(f.Type))
		st.abuse(f.Type, err)
		return frame.Frame{}, st.violation(err)
	}
}

// readErr turns a reader error into the stream's terminal error, closing the stream as the
// spec requires.
func (st *Stream) readErr(err error) error {
	var tl *frame.TooLargeError
	switch {
	case err == io.EOF:
		st.mu.Lock()
		st.readDone = true
		st.mu.Unlock()
		st.maybeRelease()
		return io.EOF
	case frame.IsUnknown(err):
		st.CloseWithCode(frame.CodeUnknownFrame, "")
		return err
	case errors.As(err, &tl):
		if tl.Known {
			st.abuse(tl.Type, err)
		}
		st.CloseWithCode(frame.CodeFrameTooLarge, "")
		return err
	case err == frame.ErrNonMinimal:
		st.CloseWithCode(frame.CodeProtocolViolation, "")
		return err
	}
	err = st.mapErr(err)
	st.finish(err)
	return err
}

func (st *Stream) violation(err error) error {
	st.CloseWithCode(frame.CodeProtocolViolation, "")
	return err
}

func (st *Stream) abuse(typ uint64, err error) {
	if h := st.sess.cfg.OnAbuse; h != nil {
		h(Abuse{Path: st.path.id, Stream: st.raw.ID(), Type: typ, Err: err})
	}
}

func (st *Stream) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if !st.path.isDead() {
		select {
		case <-st.path.carrier.Done():
			// The carrier broke under this stream: the path has failed, whichever goroutine
			// notices first.
			st.sess.failPath(st.path, ErrPathFailed)
		default:
		}
	}
	if st.path.isDead() {
		return fmt.Errorf("%w: %v", ErrPathFailed, err)
	}
	return err
}

// CloseWrite ends the sending direction (FIN on the carrier stream).
func (st *Stream) CloseWrite() error {
	err := st.raw.CloseWrite()
	st.mu.Lock()
	st.writeDone = true
	st.mu.Unlock()
	st.maybeRelease()
	return err
}

// CloseWithCode sends CLOSE{code, reason} and resets the carrier stream with code (wire.md
// §6.3: on the mux MUX_RESET follows the CLOSE frame).
func (st *Stream) CloseWithCode(code uint64, reason string) error {
	st.mu.Lock()
	if st.released {
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()
	err := st.WriteFrame(frame.TypeClose, 0, frame.Close{Code: code, Reason: reason}.Append(nil))
	st.raw.Reset(code)
	st.finish(&CloseError{Code: code, Reason: reason})
	return err
}

// Close ends the stream normally: FIN, and the receiving direction is abandoned.
func (st *Stream) Close() error {
	err := st.raw.Close()
	st.finish(nil)
	return err
}

// Reset aborts the carrier stream without a CLOSE frame.
func (st *Stream) Reset(code uint64) {
	st.raw.Reset(code)
	st.finish(&ResetError{Code: code})
}

func (st *Stream) finish(err error) {
	st.mu.Lock()
	st.readDone, st.writeDone = true, true
	if st.err == nil {
		st.err = err
	}
	st.mu.Unlock()
	st.maybeRelease()
}

func (st *Stream) maybeRelease() {
	st.mu.Lock()
	if st.released || !st.readDone || !st.writeDone {
		st.mu.Unlock()
		return
	}
	st.released = true
	st.mu.Unlock()
	st.sess.releaseStream(st)
}

// failPath ends the stream because its path died (streams are reset with path-failed).
func (st *Stream) failPath() {
	st.raw.Reset(frame.CodePathFailed)
	st.finish(ErrPathFailed)
}
