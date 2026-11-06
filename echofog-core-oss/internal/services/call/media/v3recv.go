package media

import (
	"crypto/ed25519"
	"errors"
	"time"
)

const (
	// MaxCandidates3 is how many different units with one number a receiver keeps.
	MaxCandidates3 = 4
	// LateVerify3 is how long after a CALL_AUTH a late unit is still checked against it.
	LateVerify3 = time.Second
	// CandidateTTL3 is how long a unit waits for its CALL_AUTH.
	CandidateTTL3 = 500 * time.Millisecond
	// ForgeryMute3 is how long a sender's unverified units are not played after a forgery.
	ForgeryMute3 = 2 * time.Second
)

var (
	ErrUnknownMediaKey = errors.New("media v3: no media key for sender")
	ErrForgedAuth      = errors.New("media v3: CALL_AUTH signature fails")
	ErrForgedUnit      = errors.New("media v3: unit does not match its authenticator")
)

// ReceiverConfig configures one Receiver.
type ReceiverConfig struct {
	Now    func() time.Time
	Strict bool
	// MediaKey returns the media key a sender announced in its signed join.
	MediaKey func(sender, gen byte) (ed25519.PublicKey, bool)
	// Report tells the coordinator about a forgery; it must not block.
	Report func(sender, gen byte, reason error)
}

// UnitResult says what to do with a unit that just arrived.
type UnitResult struct {
	// Play: decode and play it now. Forward: a relay forwards it now. Verified: it matched an
	// authenticator already. Duplicate: dropped.
	Play, Forward, Verified, Duplicate bool
}

// VerifiedUnit is a unit that matched a verified authenticator.
type VerifiedUnit struct {
	Header     Header3
	Ciphertext []byte
}

// AuthResult is the outcome of one CALL_AUTH.
type AuthResult struct {
	Verified []VerifiedUnit
	Forged   bool
}

// Receiver authenticates units by batch (services.md 9.3).
type Receiver struct{}

// NewReceiver returns a Receiver.
func NewReceiver(cfg ReceiverConfig) *Receiver { return &Receiver{} }

// OnUnit handles one reassembled sealed unit.
func (r *Receiver) OnUnit(h Header3, ct []byte) UnitResult { return UnitResult{} }

// OnAuth handles one CALL_AUTH payload.
func (r *Receiver) OnAuth(payload []byte) (AuthResult, error) { return AuthResult{}, ErrNotImplemented }

// Muted reports whether the sender's unverified units are held back.
func (r *Receiver) Muted(sender byte) bool { return false }

// Sweep drops expired candidates and authenticators.
func (r *Receiver) Sweep() {}
