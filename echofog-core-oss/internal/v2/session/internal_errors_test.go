package session

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/noise"
)

// TestMapHandshakeError pins the local error mapping: a selection violation aborts without a
// message (handshake §7 and §11), so it is ErrSilent, not a REJECT; version and clock skew were
// already carried by P2.reject, so they stay ErrRejected.
func TestMapHandshakeError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason uint32
		silent bool
	}{
		{"selection", handshake.ErrSelection, 0, true},
		{"peer card", handshake.ErrPeerCard, handshake.ReasonCardInvalid, false},
		{"key binding", handshake.ErrKeyBinding, handshake.ReasonCardInvalid, false},
		{"version", handshake.ErrVersion, handshake.ReasonVersion, false},
		{"clock skew", handshake.ErrClockSkew, handshake.ReasonClockSkew, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mapHandshakeError(tc.err)
			var rejected *ErrRejected
			if tc.silent {
				if !errors.Is(err, ErrSilent) {
					t.Fatalf("%v maps to %v, want ErrSilent", tc.err, err)
				}
				if errors.As(err, &rejected) {
					t.Fatalf("%v maps to a REJECT: %v", tc.err, err)
				}
				return
			}
			if !errors.As(err, &rejected) || rejected.Reason != tc.reason {
				t.Fatalf("%v maps to %v, want reason %d", tc.err, err, tc.reason)
			}
		})
	}
}

// TestFinishWrapsMuxNewSessionError checks that a failure of the mux layer is a local
// configuration error (errors.Is(err, ErrConfig)), not an opaque error.
func TestFinishWrapsMuxNewSessionError(t *testing.T) {
	// CipherStates without a key make mux.NewSealer fail.
	hs := &handshake.Session{KI2R: &noise.CipherState{}, KR2I: &noise.CipherState{}}
	_, err := finish(stubCarrier{}, nil, stubStream{}, hs, RoleResponder, mux.PathDirect, LocalIdentity{}, netip.AddrPort{})
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("finish: %v, want ErrConfig", err)
	}
}

type stubStream struct{}

func (stubStream) Read([]byte) (int, error)         { return 0, io.EOF }
func (stubStream) Write(p []byte) (int, error)      { return len(p), nil }
func (stubStream) ID() uint64                       { return 0 }
func (stubStream) CloseWrite() error                { return nil }
func (stubStream) Close() error                     { return nil }
func (stubStream) Reset(uint64)                     {}
func (stubStream) SetReadDeadline(time.Time) error  { return nil }
func (stubStream) SetWriteDeadline(time.Time) error { return nil }

type stubCarrier struct{}

func (stubCarrier) OpenStream(context.Context) (mux.RawStream, error) { return nil, errors.New("stub") }
func (stubCarrier) AcceptStream(context.Context) (mux.RawStream, error) {
	return nil, errors.New("stub")
}
func (stubCarrier) SendDatagram([]byte) error                       { return nil }
func (stubCarrier) ReceiveDatagram(context.Context) ([]byte, error) { return nil, errors.New("stub") }
func (stubCarrier) ChannelBinding() [32]byte                        { return [32]byte{} }
func (stubCarrier) Sealed() bool                                    { return false }
func (stubCarrier) IsClient() bool                                  { return true }
func (stubCarrier) LastReceive() time.Time                          { return time.Time{} }
func (stubCarrier) Close() error                                    { return nil }
func (stubCarrier) Done() <-chan struct{}                           { return nil }
