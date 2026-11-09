// Package files implements files/2 of EchoFog protocol v2 (services.md §8): a file travels
// between two nodes over their session, chunk by chunk, each chunk with its audit path to the
// Merkle root named in the offer. The receiver pulls (WANT), the sender answers (CHUNK).
//
// The Merkle tree is the one of v1 (internal/services/filexfer, used in place, AD2). The offer
// travels inside a chat/2 message (Offer.Message).
package files

import (
	"errors"
)

// Service-local frame types of files/2 (registries.md §4).
const (
	TypeWant   uint64 = 0x40
	TypeChunk  uint64 = 0x41
	TypeCancel uint64 = 0x42
	TypeDone   uint64 = 0x43
)

// CANCEL reasons (services.md §8).
const (
	ReasonCancelled   uint32 = 1
	ReasonDeclined    uint32 = 2
	ReasonUnavailable uint32 = 3
)

// Bounds of the files/2 bodies.
const (
	ChunkSize     = 64 << 10
	MaxWantChunks = 256
	MaxProof      = 40
	MaxOfferSize  = int64(4) << 40
)

// Errors.
var (
	// ErrMalformed reports a body that does not parse or breaks a bound.
	ErrMalformed = errors.New("files: malformed")
	// ErrOffer reports an offer that breaks a rule (chunk size, size, name, root).
	ErrOffer = errors.New("files: bad offer")
	// ErrUnavailable: the sender cannot serve the file to this node now (unknown file, not a
	// recipient, no free slot, or the sender is gone: all look alike).
	ErrUnavailable = errors.New("files: unavailable")
	// ErrDeclined: the sender says the receiver declined (CANCEL declined).
	ErrDeclined = errors.New("files: declined")
	// ErrCancelled: the transfer was cancelled.
	ErrCancelled = errors.New("files: cancelled")
	// ErrCorrupt: the sender keeps sending chunks that do not verify.
	ErrCorrupt = errors.New("files: corrupt data")
	// ErrBusy: this node already runs its limit of transfers.
	ErrBusy = errors.New("files: too many transfers")
	// ErrQuota: a storage or memory quota of the holder would be exceeded.
	ErrQuota = errors.New("files: quota exceeded")
	// ErrDisk: no room on the disk for the file.
	ErrDisk = errors.New("files: not enough disk space")
)
