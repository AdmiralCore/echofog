package exit

import (
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Reason says why an exit refused or could not be used. It is what the facade (T11.4) shows
// instead of v1's single "exit relay is not connected". The payment reasons (PaymentRequired,
// FreeQuotaUsed, PriceChanged) belong to T10.3.
type Reason uint8

const (
	// ReasonUnknown is an error that is not an exit refusal.
	ReasonUnknown Reason = iota
	// ReasonNoCamouflage: the exit has no camouflaged address (reality2 or ws2) and could not be
	// reached; strangers cannot use it (D37).
	ReasonNoCamouflage
	// ReasonUnreachable: refused, reset, timed out or DNS failure, all alike (D19).
	ReasonUnreachable
	// ReasonBusy: the exit is out of tunnel slots for this client.
	ReasonBusy
	// ReasonPolicy: the exit's policy forbids the target or the request (denied, blocked port,
	// private range, CLOSE policy).
	ReasonPolicy
	// ReasonRateLimited: the client's own rate limit at the exit was hit.
	ReasonRateLimited
	// ReasonNoPath: no session to the exit could be set up.
	ReasonNoPath
)

// String is the stable code of the reason (logs, events, the facade's enum).
func (r Reason) String() string { return "" }

// Message is the neutral Russian text for the user.
func (r Reason) Message() string { return "" }

// Refusal is an exit refusal with its reason. Its text is fixed per reason: it never carries the
// target, the exit's address or the underlying cause (AD20).
type Refusal struct {
	Reason Reason
	cause  error
}

func (e *Refusal) Error() string { return "" }

// Unwrap returns the underlying error, for errors.Is; it is never shown to users.
func (e *Refusal) Unwrap() error { return e.cause }

// ReasonOf classifies an error returned by the client or read from a tunnel.
func ReasonOf(err error) Reason { return ReasonUnknown }

// CardHasCamouflage reports whether the card has an address strangers can reach through a cover.
func CardHasCamouflage(c *objects.Card) bool { return false }
