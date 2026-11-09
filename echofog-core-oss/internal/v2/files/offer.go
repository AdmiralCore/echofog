package files

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/echofog/echofog-core/internal/services/filexfer"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"google.golang.org/protobuf/encoding/protowire"
)

// Bounds of an offer.
const (
	maxNameBytes = 255
	maxMimeBytes = 127
)

// Offer is what the receiver learns about a file, inside a chat/2 message (services.md §8):
// { bytes fid = 1; uint64 size = 2; bytes root = 3; uint32 chunk_size = 4; string name = 5;
// string mime = 6; bytes sender = 7; }.
type Offer struct {
	FID       [16]byte
	Size      uint64
	Root      [32]byte
	ChunkSize uint32
	Name      string
	Mime      string
	Sender    [32]byte // node ID of the sender
}

func offerErr(what string) error { return fmt.Errorf("%w: %s", ErrOffer, what) }

// Marshal encodes the offer canonically.
func (o Offer) Marshal() []byte {
	var b []byte
	b = appendBytesField(b, 1, o.FID[:])
	b = appendVarintField(b, 2, o.Size)
	b = appendBytesField(b, 3, o.Root[:])
	b = appendVarintField(b, 4, uint64(o.ChunkSize))
	if o.Name != "" {
		b = appendBytesField(b, 5, []byte(o.Name))
	}
	if o.Mime != "" {
		b = appendBytesField(b, 6, []byte(o.Mime))
	}
	return appendBytesField(b, 7, o.Sender[:])
}

// ParseOffer decodes and validates an offer.
func ParseOffer(p []byte) (Offer, error) {
	var o Offer
	var seen [8]bool
	once := func(n int) error {
		if seen[n] {
			return offerErr("repeated field")
		}
		seen[n] = true
		return nil
	}
	err := walk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1, 3, 7:
			if typ != protowire.BytesType || once(int(num)) != nil {
				return offerErr("field")
			}
			switch num {
			case 1:
				if len(s) != 16 {
					return offerErr("fid")
				}
				copy(o.FID[:], s)
			case 3:
				if len(s) != 32 {
					return offerErr("root")
				}
				copy(o.Root[:], s)
			case 7:
				if len(s) != 32 {
					return offerErr("sender")
				}
				copy(o.Sender[:], s)
			}
		case 2, 4:
			if typ != protowire.VarintType || once(int(num)) != nil {
				return offerErr("field")
			}
			if num == 2 {
				o.Size = v
			} else {
				if v > math.MaxUint32 {
					return offerErr("chunk size")
				}
				o.ChunkSize = uint32(v)
			}
		case 5, 6:
			if typ != protowire.BytesType || once(int(num)) != nil {
				return offerErr("field")
			}
			if num == 5 {
				if len(s) > maxNameBytes {
					return offerErr("name")
				}
				o.Name = string(s)
			} else {
				if len(s) > maxMimeBytes {
					return offerErr("mime")
				}
				o.Mime = string(s)
			}
		}
		return nil
	})
	if err != nil {
		return Offer{}, offerErr("does not parse")
	}
	if err := o.Validate(); err != nil {
		return Offer{}, err
	}
	return o, nil
}

// Validate checks the rules of an offer.
func (o Offer) Validate() error {
	if err := o.validateGeometry(); err != nil {
		return err
	}
	switch {
	case o.FID == [16]byte{} || o.Root == [32]byte{} || o.Sender == [32]byte{}:
		return offerErr("zero identifier")
	case !plainText(o.Name, maxNameBytes, true) || o.Name == "." || o.Name == "..":
		return offerErr("name")
	case !plainText(o.Mime, maxMimeBytes, false):
		return offerErr("mime")
	}
	return nil
}

// validateGeometry checks what the chunk arithmetic relies on.
func (o Offer) validateGeometry() error {
	switch {
	case o.ChunkSize != ChunkSize:
		return offerErr("chunk size")
	case o.Size > uint64(MaxOfferSize):
		return offerErr("size")
	}
	return nil
}

// plainText: valid UTF-8, no control characters; a file name also has no path separators or drive.
func plainText(s string, max int, name bool) bool {
	if len(s) > max || !utf8.ValidString(s) || (name && s == "") {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
		if name && (r == '/' || r == '\\') {
			return false
		}
	}
	return !(name && len(s) >= 2 && s[1] == ':')
}

// Chunks is the number of chunks of the file (an empty file has one).
func (o Offer) Chunks() int {
	if o.Size > uint64(MaxOfferSize) {
		return 0
	}
	return filexfer.NumChunks(int64(o.Size))
}

// Message wraps the offer into a chat/2 plaintext of type FILE: the name is the text, the offer
// travels in the metadata (base64url, no padding).
func (o Offer) Message() *chatpb.PersonalMessageProto {
	return &chatpb.PersonalMessageProto{
		Id:          hex.EncodeToString(o.FID[:]),
		SenderId:    hex.EncodeToString(o.Sender[:]),
		Type:        chatpb.PersonalMessageType_MSG_TYPE_FILE,
		Text:        o.Name,
		ContentHash: hex.EncodeToString(o.Root[:]),
		Metadata:    base64.RawURLEncoding.EncodeToString(o.Marshal()),
	}
}

// OfferFrom reads the offer out of a chat/2 plaintext of type FILE. The content hash the message
// states must be the offer's root.
func OfferFrom(m *chatpb.PersonalMessageProto) (Offer, error) {
	if m == nil || m.Type != chatpb.PersonalMessageType_MSG_TYPE_FILE {
		return Offer{}, offerErr("not a file message")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(m.Metadata))
	if err != nil || len(raw) == 0 || len(raw) > 2048 {
		return Offer{}, offerErr("metadata")
	}
	o, err := ParseOffer(raw)
	if err != nil {
		return Offer{}, err
	}
	if m.ContentHash != "" && !strings.EqualFold(m.ContentHash, hex.EncodeToString(o.Root[:])) {
		return Offer{}, offerErr("content hash differs from the root")
	}
	return o, nil
}
