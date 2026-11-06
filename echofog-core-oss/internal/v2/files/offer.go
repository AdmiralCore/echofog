package files

import (
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

// Offer is what the receiver learns about a file, inside a chat/2 message (services.md §8).
type Offer struct {
	FID       [16]byte
	Size      uint64
	Root      [32]byte
	ChunkSize uint32
	Name      string
	Mime      string
	Sender    [32]byte // node ID of the sender
}

// Marshal encodes the offer canonically.
func (o Offer) Marshal() []byte { return nil }

// ParseOffer decodes and validates an offer.
func ParseOffer(p []byte) (Offer, error) { return Offer{}, ErrOffer }

// Validate checks the rules of an offer.
func (o Offer) Validate() error { return ErrOffer }

// Chunks is the number of chunks of the file (an empty file has one).
func (o Offer) Chunks() int { return 0 }

// Message wraps the offer into a chat/2 plaintext of type FILE.
func (o Offer) Message() *chatpb.PersonalMessageProto { return nil }

// OfferFrom reads the offer out of a chat/2 plaintext of type FILE.
func OfferFrom(m *chatpb.PersonalMessageProto) (Offer, error) { return Offer{}, ErrOffer }
