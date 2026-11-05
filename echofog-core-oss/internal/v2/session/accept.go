package session

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
)

// AcceptConfig configures one Accept call. Local and Local.Device are required; everything else
// has a production default.
type AcceptConfig struct {
	// Local is the identity that answers: its device keys, own cards, features and services.
	Local LocalIdentity
	// Hidden marks a hidden node: XK only, no puzzle, and every failure is the uniform silent
	// close of handshake §5.
	Hidden bool
	// PrevStatic is the previous hs key during a rotation (handshake §7.1).
	PrevStatic *ecdh.PrivateKey
	// Passes resolves invitation and personal pass secrets for a hidden node.
	Passes handshake.PassStore
	// Puzzle enables HS_RETRY under load (handshake §6).
	Puzzle *handshake.PuzzleConfig
	// Replay is the 0-RTT replay cache (handshake §10).
	Replay *handshake.ReplayCache
	// Observed is the initiator's address as seen (feature observed-addr, wire.md §7.2).
	Observed []byte
	// Binder receives a stream 0 that starts with PATH_BIND (wire.md §8.2). When nil, a
	// PATH_BIND is an unexpected first frame and the carrier is closed silently.
	Binder *mux.Binder
	// Kind is the path kind of the accepted carrier (direct here; relay is T6.1).
	Kind mux.PathKind
	// Clock is the injectable time source (AD13); nil means the system clock.
	Clock carrier.Clock
	// Offset is the learned clock offset (T3.8).
	Offset time.Duration
	// Rand draws the hidden node's uniform close delay (tests inject a deterministic reader).
	Rand io.Reader
	// Verifier replaces the card verifier (default handshake.DefaultCardVerifier).
	Verifier handshake.CardVerifier
	// Bindings sets the hs-key-to-device binding table (handshake §7).
	Bindings handshake.KeyBinder
	// MessageTimeout and HandshakeTimeout override the handshake §11 defaults.
	MessageTimeout   time.Duration
	HandshakeTimeout time.Duration
}

func (c *AcceptConfig) messageTimeout() time.Duration {
	if c.MessageTimeout > 0 {
		return c.MessageTimeout
	}
	return handshake.MessageTimeout
}

func (c *AcceptConfig) handshakeTimeout() time.Duration {
	if c.HandshakeTimeout > 0 {
		return c.HandshakeTimeout
	}
	return handshake.HandshakeTimeout
}

// Accept accepts one carrier from l and either completes a new session (Established) or, when
// stream 0 starts with a PATH_BIND and Binder is set, binds it to an existing session and
// returns ErrPathBound. Every other first frame closes the carrier without a message.
func Accept(ctx context.Context, l carrier.Listener, cfg AcceptConfig) (*Established, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil || cfg.Local.Device == nil {
		return nil, fmt.Errorf("%w: listener and device are required", ErrConfig)
	}
	clk := clockOr(cfg.Clock)

	c, err := l.Accept(ctx)
	if err != nil {
		return nil, err
	}
	g := mux.NewGuard(c)

	var pend *mux.Pending
	if cfg.Binder != nil {
		_, p, err := cfg.Binder.Accept(ctx, g, cfg.Kind)
		switch {
		case err == nil:
			return nil, ErrPathBound
		case errors.Is(err, mux.ErrNewSession):
			pend = p
		default:
			return nil, wrapSilent(err)
		}
	} else {
		raw, err := g.Stream0(ctx)
		if err != nil {
			_ = c.Close()
			return nil, wrapSilent(err)
		}
		first, err := readFrame(frame.NewReader(raw, nil), raw, clk.Now().Add(cfg.messageTimeout()))
		if err != nil || first.Type != frame.TypeHsHello {
			_ = c.Close()
			return nil, wrapSilent(err)
		}
		pend = &mux.Pending{Guard: g, Stream: raw, First: first}
	}

	static, err := staticKey(cfg.Local.Device)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	resp := handshake.NewResponder(handshake.ResponderConfig{
		Static:     static,
		PrevStatic: cfg.PrevStatic,
		DeviceID:   cfg.Local.Device.ID(),
		OwnCards:   cfg.Local.OwnCards,
		Suites:     cfg.Local.Suites,
		Hidden:     cfg.Hidden,
		Passes:     cfg.Passes,
		Features:   cfg.Local.Features,
		Services:   cfg.Local.Services,
		Now:        clk.Now,
		Offset:     cfg.Offset,
		Puzzle:     cfg.Puzzle,
		Replay:     cfg.Replay,
		Observed:   cfg.Observed,
		Cards:      cfg.Verifier,
		Bindings:   cfg.Bindings,
		Rand:       cfg.Rand,
	})

	cb := c.ChannelBinding()
	rd := frame.NewReader(pend.Stream, nil)
	whole := clk.Now().Add(cfg.handshakeTimeout())
	hello := pend.First.Payload
	retries := 0
	for {
		if retries >= 2 {
			// A third flight after two HS_RETRY is never served (handshake §6).
			_ = c.Close()
			return nil, ErrSilent
		}
		f, err := readFrame(rd, pend.Stream, messageDeadline(clk, whole))
		if err != nil {
			_ = c.Close()
			return nil, wrapSilent(err)
		}
		if f.Type != frame.TypeHsMsg {
			_ = c.Close()
			return nil, wrapSilent(fmt.Errorf("unexpected %s after HS_HELLO", frame.TypeName(f.Type)))
		}
		arrival := clk.Now()
		reply := resp.HandleFirst(cb[:], hello, f.Payload, arrival)
		switch reply.Action {
		case handshake.ActSendAndClose:
			if reply.FrameType != 0 {
				_ = writeFrame(pend.Stream, clk, uint64(reply.FrameType), reply.Payload)
			}
			closeAfterFlush(c, clk)
			// REJECT{suite} on the control stream and a Reject inside P2 both carry a
			// *handshake.Reject; mapRejected turns either into ErrRejected.
			if reply.Err != nil {
				return nil, mapHandshakeError(reply.Err)
			}
			return nil, ErrSilent
		case handshake.ActClose:
			_ = c.Close()
			return nil, wrapSilent(reply.Err)
		case handshake.ActSilentClose:
			waitUntil(clk, reply.CloseAt)
			_ = c.Close()
			return nil, ErrSilent
		case handshake.ActSend:
			if reply.FrameType == handshake.FrameRetry {
				if err := writeFrame(pend.Stream, clk, uint64(reply.FrameType), reply.Payload); err != nil {
					_ = c.Close()
					return nil, wrapSilent(err)
				}
				retries++
				next, err := readFrame(rd, pend.Stream, messageDeadline(clk, whole))
				if err != nil {
					_ = c.Close()
					return nil, wrapSilent(err)
				}
				if next.Type != frame.TypeHsHello {
					_ = c.Close()
					return nil, wrapSilent(fmt.Errorf("unexpected %s after HS_RETRY", frame.TypeName(next.Type)))
				}
				hello = next.Payload
				continue
			}
			if reply.FrameType != handshake.FrameMsg {
				_ = c.Close()
				return nil, wrapSilent(fmt.Errorf("unexpected reply type %d", reply.FrameType))
			}
			if err := writeFrame(pend.Stream, clk, uint64(reply.FrameType), reply.Payload); err != nil {
				_ = c.Close()
				return nil, wrapSilent(err)
			}
			fin, err := readFrame(rd, pend.Stream, messageDeadline(clk, whole))
			if err != nil {
				_ = c.Close()
				return nil, wrapSilent(err)
			}
			if fin.Type != frame.TypeHsMsg {
				_ = c.Close()
				return nil, wrapSilent(fmt.Errorf("unexpected %s instead of message 3", frame.TypeName(fin.Type)))
			}
			hs, err := resp.HandleFinish(fin.Payload)
			if err != nil {
				return nil, rejectAfterFinish(c, pend.Stream, clk, err)
			}
			return finish(c, pend.Guard, pend.Stream, hs, RoleResponder, cfg.Kind, cfg.Local, remoteOf(c))
		default:
			_ = c.Close()
			return nil, wrapSilent(fmt.Errorf("unexpected handshake action %d", reply.Action))
		}
	}
}

// rejectAfterFinish sends REJECT{card-invalid} after message 3 when the initiator's card is
// invalid (handshake §8); every other failure closes without a message (§11). On a sealed carrier
// the switch point has already passed, so this plaintext REJECT ends the path instead; the
// ambiguity is recorded in reports/T3.1.md.
func rejectAfterFinish(c mux.Carrier, s mux.RawStream, clk carrier.Clock, err error) error {
	card := errors.Is(err, handshake.ErrPeerCard) || errors.Is(err, handshake.ErrKeyBinding)
	if !card {
		_ = c.Close()
		return wrapSilent(err)
	}
	payload := (&handshake.Reject{Reason: handshake.ReasonCardInvalid}).Marshal()
	if werr := writeFrame(s, clk, frame.TypeReject, payload); werr != nil {
		_ = c.Close()
		return wrapSilent(err)
	}
	closeAfterFlush(c, clk)
	return &ErrRejected{Reason: handshake.ReasonCardInvalid, Cause: err}
}
