// Package v1sig maps the call layer's v1 signals (a JSON envelope with a per-action JSON payload,
// internal/services/call/signaling) to the typed call/2 CallSignal and back (T9.3b, services.md
// 9.1). The envelope's v1 signature is dropped: on call/2 the signature is the originator's
// device-key signature over the exact CallSignal bytes (call.Encode), and the sender is the
// verified device ID, never a value read from the payload.
package v1sig

import (
	"errors"

	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
)

// ErrMapping: a signal cannot be mapped between v1 and call/2.
var ErrMapping = errors.New("v1sig: cannot map signal")

// ErrNotImplemented is returned by the stubs.
var ErrNotImplemented = errors.New("v1sig: not implemented")

// ToPB converts a v1 envelope (JSON bytes) to a CallSignal. The payload must match the action.
func ToPB(v1 []byte) (*pb.CallSignal, error) { return nil, ErrNotImplemented }

// ToV1 converts a verified CallSignal to the v1 envelope JSON the call layer reads. The envelope
// is unsigned: the caller has verified the signal on call/2.
func ToV1(m *pb.CallSignal) ([]byte, error) { return nil, ErrNotImplemented }
