package session

import (
	"context"
	"crypto/ecdh"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/netip"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/shape"
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
	// Puzzle enables HS_RETRY under load (handshake §6). When nil and Limits is set, the puzzle
	// of Limits is used with the difficulty of this carrier's admission.
	Puzzle *handshake.PuzzleConfig
	// Limits admits the carrier before any handshake byte is read (handshake §11), picks the puzzle
	// difficulty, scores authenticated abuse of the session and refuses banned devices. Nil
	// disables all of it.
	Limits *Limits
	// Source overrides the address the carrier reports for Limits (tests; a relay circuit, whose
	// carrier address is the relay's).
	Source netip.Addr
	// AdmitVersion refuses peers by protocol version with REJECT{version} (versioning §5). The
	// runtime wires objects.ReleaseState.PeerRefused here; nil admits everyone.
	AdmitVersion func(peerProto uint32) bool
	// Replay is the 0-RTT replay cache (handshake §10).
	Replay *handshake.ReplayCache
	// Observed overrides the initiator's address as seen (feature observed-addr, wire.md §7.2).
	// Empty means the address the carrier reports, for a direct path only: over a relay circuit
	// the carrier's peer is the relay, and reporting it would mislead the initiator.
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
	// Control handles CARD_UPDATE for the accepted session (objects.md §3.6).
	Control *Control
	// Timers replaces the mux session timers (tests and platform profiles).
	Timers mux.Timers
	// Policy builds the mux stream policy from the negotiated services (T3.7a).
	Policy PolicyFunc
	// MessageTimeout and HandshakeTimeout override the handshake §11 defaults.
	MessageTimeout   time.Duration
	HandshakeTimeout time.Duration
	// LowPower marks the platform as low_power (platforms.md §2): no standby paths and no
	// idle-padding feature offered.
	LowPower bool
	// IdlePadding is the shape profile whose idle_pad_ms drives PADDING-only frames when the
	// feature was negotiated (wire.md §6.4, §9). Nil disables idle padding.
	IdlePadding *shape.Profile
	// IdlePaddingRand is the deterministic random source for tests.
	IdlePaddingRand *mrand.Rand
	// OnEstablished receives the mux session before Accept returns, so the runtime can register
	// it in its PATH_BIND binder without a window (wire.md §8.2).
	OnEstablished func(*mux.Session)
	// OnPathFailed reports every path failure of the session (wire.md §8.3).
	OnPathFailed func(mux.PathFailure)
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

// Accept accepts one carrier from l and runs AcceptCarrier on it. A caller that must serve many
// carriers at once (the node) calls l.Accept itself and gives each carrier its own AcceptCarrier,
// so that one slow initiator cannot hold up the others and the limits of handshake §11 count
// handshakes that are really in flight.
func Accept(ctx context.Context, l carrier.Listener, cfg AcceptConfig) (*Established, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil || cfg.Local.Device == nil {
		return nil, fmt.Errorf("%w: listener and device are required", ErrConfig)
	}
	c, err := l.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return AcceptCarrier(ctx, c, cfg)
}

// AcceptCarrier serves one already accepted carrier: it either completes a new session
// (Established) or, when stream 0 starts with a PATH_BIND and Binder is set, binds it to an
// existing session and returns ErrPathBound. Every other first frame closes the carrier without a
// message. The carrier is always closed or handed to the session. It is safe to call concurrently.
func AcceptCarrier(ctx context.Context, c mux.Carrier, cfg AcceptConfig) (*Established, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || cfg.Local.Device == nil {
		return nil, fmt.Errorf("%w: carrier and device are required", ErrConfig)
	}
	clk := clockOr(cfg.Clock)
	g := mux.NewGuard(c)

	var adm *Admission
	if cfg.Limits != nil {
		src := cfg.Source
		if !src.IsValid() {
			src = remoteOf(c).Addr()
		}
		adm = cfg.Limits.Admit(src, cfg.Hidden)
		defer adm.Release()
		if adm.Refused() {
			// A hidden node over a per-source limit: no byte is read or sent; the carrier is
			// closed after the uniform delay of handshake §5, as for every other failure.
			waitUntil(clk, clk.Now().Add(uniformSilentDelay(cfg.Rand)))
			_ = c.Close()
			return nil, ErrSilent
		}
	}
	puzzle := cfg.Puzzle
	if puzzle == nil && adm != nil && !cfg.Hidden {
		pc := cfg.Limits.Puzzle()
		d := adm.Difficulty()
		pc.Difficulty = func() uint8 { return d }
		puzzle = pc
	}

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
	local := cfg.Local
	local.Features = FeaturesFor(local.Features, cfg.LowPower)
	observed := cfg.Observed
	if len(observed) == 0 && cfg.Kind == mux.PathDirect {
		if ra := remoteOf(c); ra.IsValid() {
			observed = frame.AppendObservedAddr(nil, ra)
		}
	}
	resp := handshake.NewResponder(handshake.ResponderConfig{
		Static:     static,
		PrevStatic: cfg.PrevStatic,
		DeviceID:   cfg.Local.Device.ID(),
		OwnCards:   cfg.Local.OwnCards,
		Suites:     cfg.Local.Suites,
		Hidden:     cfg.Hidden,
		Passes:     cfg.Passes,
		Features:   local.Features,
		Services:   cfg.Local.Services,
		Now:        clk.Now,
		Offset:     cfg.Offset,
		Puzzle:     puzzle,
		Replay:     cfg.Replay,
		Observed:   observed,
		Cards:      cfg.Verifier,
		Bindings:   cfg.Bindings,
		Rand:       cfg.Rand,

		AdmitVersion: cfg.AdmitVersion,
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
				cfg.Limits.noteRetry()
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
				return nil, rejectAfterFinish(c, pend.Stream, clk, hs, cfg.Kind, err)
			}
			if hs.Peer != nil && cfg.Limits.BannedPeer(hs.Peer.DeviceID, hs.Peer.NodeID) {
				// A ban applies to an authenticated device or node ID only, so it is enforced
				// here, once message 3 has named the peer; never to a source address
				// (handshake §11). The answer is REJECT{banned} (§8), sent like every other
				// refusal after message 3 so that timing does not tell the reasons apart (AD20).
				return nil, sendRejectAfterFinish(c, pend.Stream, clk, hs, cfg.Kind, handshake.ReasonBanned, ErrBanned)
			}
			return finish(c, pend.Guard, pend.Stream, hs, RoleResponder, cfg.Kind, local, remoteOf(c),
				cfg.Control, cfg.Timers, cfg.Policy, cfg.LowPower,
				newIdlePadding(cfg.IdlePadding, cfg.IdlePaddingRand, cfg.LowPower),
				cfg.OnEstablished, cfg.OnPathFailed, cfg.Limits)
		default:
			_ = c.Close()
			return nil, wrapSilent(fmt.Errorf("unexpected handshake action %d", reply.Action))
		}
	}
}

// rejectAfterFinish sends REJECT{card-invalid} after message 3 when the initiator's card is
// invalid (handshake §8); every other failure closes without a message (§11). The switch point
// has already passed on a sealed carrier, so there the REJECT is a sealed kind-1 record on the
// control stream (wire §8.5, decisions.md T3.1), never plaintext.
func rejectAfterFinish(c mux.Carrier, s mux.RawStream, clk carrier.Clock, hs *handshake.Session, kind mux.PathKind, err error) error {
	card := errors.Is(err, handshake.ErrPeerCard) || errors.Is(err, handshake.ErrKeyBinding)
	if !card || hs == nil {
		_ = c.Close()
		return wrapSilent(err)
	}
	return sendRejectAfterFinish(c, s, clk, hs, kind, handshake.ReasonCardInvalid, err)
}

// sendRejectAfterFinish sends REJECT{reason} after message 3 and closes once it is flushed.
func sendRejectAfterFinish(c mux.Carrier, s mux.RawStream, clk carrier.Clock, hs *handshake.Session, kind mux.PathKind, reason uint32, cause error) error {
	payload := (&handshake.Reject{Reason: reason}).Marshal()
	sealed := c.Sealed() || kind == mux.PathRelay
	if sealed {
		body := frame.AppendFrame(nil, frame.Frame{Type: frame.TypeReject, Payload: payload})
		if werr := writeSealed(c, s, clk, hs, body); werr != nil {
			_ = c.Close()
			return wrapSilent(werr)
		}
	} else if werr := writeFrame(s, clk, frame.TypeReject, payload); werr != nil {
		_ = c.Close()
		return wrapSilent(cause)
	}
	closeAfterFlush(c, clk)
	return &ErrRejected{Reason: reason, Cause: cause}
}

// writeSealed writes one plaintext frame as a sealed record of path 0 on the control stream. A
// byte-stream carrier wraps its whole connection-level frame sequence as one pipe; a carrier with
// native streams (QUIC) seals stream 0 as its own pipe (wire §8.5).
func writeSealed(c mux.Carrier, control mux.RawStream, clk carrier.Clock, hs *handshake.Session, body []byte) error {
	sealer, err := mux.NewSealer(sessionKeys{hs: hs})
	if err != nil {
		return err
	}
	if err := control.SetWriteDeadline(clk.Now().Add(handshake.MessageTimeout)); err != nil {
		return err
	}
	if sw, ok := c.(interface {
		StartSealing(*mux.Sealer, uint64)
	}); ok {
		sw.StartSealing(sealer, 0)
		_, err = control.Write(body)
		return err
	}
	rc := mux.NewRecordConn(control)
	rc.StartSealing(sealer, 0, control.ID())
	_, err = rc.Write(body)
	return err
}

// uniformSilentDelay draws uniform(0.5, 3) s for the silent close of a refused carrier (handshake
// §5). A failing source of randomness gives the upper bound.
func uniformSilentDelay(r io.Reader) time.Duration {
	if r == nil {
		r = crand.Reader
	}
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 3 * time.Second
	}
	return 500*time.Millisecond + time.Duration(binary.BigEndian.Uint64(b[:])%uint64(2500*time.Millisecond+1))
}
