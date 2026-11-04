package objects

import (
	"errors"

	"github.com/echofog/echofog-core/internal/v2/keys"
)

// Signed-object labels (registries.md §7.1).
const (
	LabelCard        = "echofog/card/2"
	LabelPrekeys     = "echofog/prekeys/2"
	LabelDevices     = "echofog/devices/2"
	LabelMailboxHint = "echofog/mailbox-hint/2"
	LabelName        = "echofog/name/2"
	LabelRelease     = "echofog/release/2"
)

const (
	day  = 86400
	hour = 3600

	// ClockSkew is the tolerance of the expiry checks (objects.md §2.2 step 4).
	ClockSkew = 60
	// UnknownLabelMaxEnvelope is the generic limit of every label this build does not know, whatever
	// its prefix (objects.md §2.2 step 3, registries.md §9).
	UnknownLabelMaxEnvelope = 2048
	// UnknownLabelStoreLifetime clips the stored lifetime of objects with unknown labels.
	UnknownLabelStoreLifetime = 24 * hour

	// CardMaxLifetime and InviteCardMaxLifetime are the card lifetimes (objects.md §3.2, D12).
	CardMaxLifetime       = 7 * day
	InviteCardMaxLifetime = 30 * day
)

// LabelInfo holds the limits of a known label.
type LabelInfo struct {
	// MaxBody is the body limit of registries.md §7.1.
	MaxBody int
	// MaxEnvelope is the envelope limit of registries.md §7.1 and §9.
	MaxEnvelope int
	// MaxLifetime is the longest allowed not_after − now, in seconds: the label's largest. For cards
	// it is the invite-scope maximum; VerifyCard applies the limit of the card's scope.
	MaxLifetime uint64
}

var labels = map[string]LabelInfo{
	LabelCard:        {3800, 4096, InviteCardMaxLifetime},
	LabelPrekeys:     {6144, 8192, 30 * day},
	LabelDevices:     {4096, 6144, 30 * day},
	LabelMailboxHint: {6144, 6400, 6 * hour},
	LabelName:        {512, 768, 30 * day},
	LabelRelease:     {20000, 20480, 400 * day},
}

// Limits returns the limits of a known label.
func Limits(label string) (LabelInfo, bool) {
	i, ok := labels[label]
	return i, ok
}

var (
	// ErrMalformed reports an envelope that cannot be parsed (format, lengths, label, seq).
	ErrMalformed = errors.New("objects: malformed envelope")
	// ErrUnusable reports an object signed with an algorithm this build does not implement: it is
	// skipped, not treated as malformed (objects.md §2.2 step 2).
	ErrUnusable = errors.New("objects: unknown signature algorithm, object unusable")
	// ErrTooLarge reports an envelope or body beyond its label's size limit.
	ErrTooLarge = errors.New("objects: object too large")
	// ErrBadSignature reports a signature that does not verify (the same value as keys.ErrBadSignature).
	ErrBadSignature = keys.ErrBadSignature
	// ErrExpired reports not_after ≤ now − 60 s.
	ErrExpired = errors.New("objects: object expired")
	// ErrLifetime reports a lifetime beyond the label's maximum (+60 s).
	ErrLifetime = errors.New("objects: lifetime too long")
	// ErrNonCanonical reports a body that breaks the canonical protobuf rules (objects.md §2.3).
	ErrNonCanonical = errors.New("objects: body not canonical")
	// ErrInvalid reports a body that breaks a label-specific rule.
	ErrInvalid = errors.New("objects: invalid object")
	// ErrDevices reports a card whose embedded device list is missing, invalid or does not list
	// the signer (objects.md §3.1, D35). It is returned together with ErrInvalid.
	ErrDevices = errors.New("objects: card device list invalid")
	// ErrWrongLabel reports an object of another label than the one expected.
	ErrWrongLabel = errors.New("objects: unexpected label")
)
