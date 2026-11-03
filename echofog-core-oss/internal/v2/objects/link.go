package objects

import (
	"bytes"
	"compress/flate"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Links v2 (objects.md §10):
//
//	echofog://join?v=2&card=<card64>[&s=<secret64>]
//	card64 = base64url-nopad(method(u8) ‖ data), method 0: the envelope, 1: raw DEFLATE of it

const (
	linkPrefix    = "echofog://join?"
	webMarker     = "/join/#join?"
	maxCardEnv    = 4096
	accessSecretN = 16
	// maxCard64 bounds the card parameter before decoding (a DEFLATE stream of a 4096-byte card
	// is at most a few bytes longer than the card).
	maxCard64 = 8192

	LinkMethodRaw     = 0x00
	LinkMethodDeflate = 0x01

	passIDLabel = "echofog/hs2/pass-id"
)

var (
	// ErrOtherVersion is the refusal of a link with v ≠ 2 ("made by another version").
	ErrOtherVersion = errors.New("objects: link made by another version")
	// ErrBadLink reports a link that cannot be parsed.
	ErrBadLink = errors.New("objects: bad link")
)

var b64 = base64.RawURLEncoding.Strict()

// Link is a parsed v2 link. Card is the card envelope exactly as carried.
type Link struct {
	Card   []byte
	Secret []byte // 16-byte access secret of an invitation link, nil for a contact link
	Method byte
}

func badLink(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadLink, fmt.Sprintf(format, args...))
}

// EncodeLink builds a link for a card envelope, with the shorter of the two methods (raw on a tie).
// secret is nil for a contact link.
func EncodeLink(card, secret []byte) (string, error) {
	if len(card) == 0 || len(card) > maxCardEnv {
		return "", badLink("card of %d bytes", len(card))
	}
	if secret != nil && len(secret) != accessSecretN {
		return "", badLink("secret of %d bytes", len(secret))
	}
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(card); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	data := append([]byte{LinkMethodRaw}, card...)
	if z.Len()+1 < len(data) {
		data = append([]byte{LinkMethodDeflate}, z.Bytes()...)
	}
	s := linkPrefix + "v=2&card=" + b64.EncodeToString(data)
	if secret != nil {
		s += "&s=" + b64.EncodeToString(secret)
	}
	return s, nil
}

// WebLink returns the web form of a link: https://<join host>/join/#<link without echofog://>.
func WebLink(joinHost, link string) string {
	return "https://" + joinHost + "/join/#" + strings.TrimPrefix(link, "echofog://")
}

// ParseLink parses a link or its web form. It decodes the card but does not verify it (see
// Link.Verify). Unknown parameters are ignored; the known ones must appear in the order v, card, s,
// each at most once.
func ParseLink(s string) (*Link, error) {
	var q string
	switch {
	case strings.HasPrefix(s, linkPrefix):
		q = s[len(linkPrefix):]
	case strings.HasPrefix(s, "https://"):
		rest := s[len("https://"):]
		i := strings.Index(rest, webMarker)
		if i <= 0 || strings.ContainsAny(rest[:i], "/?#@\\") {
			return nil, badLink("not a join page")
		}
		q = rest[i+len(webMarker):]
	default:
		return nil, badLink("not an echofog://join link")
	}
	var v, card, secret string
	var seenV, seenCard, seenS bool
	order := 0
	for _, kv := range strings.Split(q, "&") {
		k, val, _ := strings.Cut(kv, "=")
		var rank int
		switch k {
		case "v":
			if seenV {
				return nil, badLink("v twice")
			}
			v, seenV, rank = val, true, 1
		case "card":
			if seenCard {
				return nil, badLink("card twice")
			}
			card, seenCard, rank = val, true, 2
		case "s":
			if seenS {
				return nil, badLink("s twice")
			}
			secret, seenS, rank = val, true, 3
		default:
			continue
		}
		if rank < order {
			return nil, badLink("parameters out of order")
		}
		order = rank
	}
	if !seenV {
		return nil, badLink("no version")
	}
	if v != "2" {
		return nil, ErrOtherVersion
	}
	if !seenCard || card == "" || len(card) > maxCard64 {
		return nil, badLink("card parameter")
	}
	raw, err := b64.DecodeString(card)
	if err != nil || len(raw) < 2 {
		return nil, badLink("card encoding")
	}
	l := &Link{Method: raw[0]}
	switch raw[0] {
	case LinkMethodRaw:
		l.Card = raw[1:]
	case LinkMethodDeflate:
		r := flate.NewReader(bytes.NewReader(raw[1:]))
		out, err := io.ReadAll(io.LimitReader(r, maxCardEnv+1))
		_ = r.Close()
		if err != nil {
			return nil, badLink("inflate: %v", err)
		}
		l.Card = out
	default:
		return nil, badLink("card method 0x%02x", raw[0])
	}
	if len(l.Card) == 0 || len(l.Card) > maxCardEnv {
		return nil, badLink("card of %d bytes", len(l.Card))
	}
	if seenS {
		sec, err := b64.DecodeString(secret)
		if err != nil || len(sec) != accessSecretN {
			return nil, badLink("access secret")
		}
		l.Secret = sec
	}
	return l, nil
}

// Verify verifies the link's card. A link with an access secret must carry an invitation card whose
// invite_id is the secret's pass_id; a link without one must carry a public card.
func (l *Link) Verify(now time.Time) (*Card, error) {
	c, err := VerifyCard(l.Card, now)
	if err != nil {
		return nil, err
	}
	if l.Secret != nil {
		pid := PassID(l.Secret)
		if c.Scope != ScopeInvite || !bytes.Equal(c.Body.GetInviteId(), pid[:]) {
			return nil, invalid("invitation link: card is not the invitation's card")
		}
		return c, nil
	}
	if c.Scope != ScopePublic {
		return nil, invalid("contact link: card is not public")
	}
	return c, nil
}

// PassID returns the invitation pass selector HMAC-SHA256(S, "echofog/hs2/pass-id")[:8]
// (handshake.md §5): the invite_id of the invitation card.
func PassID(secret []byte) [8]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(passIDLabel))
	var out [8]byte
	copy(out[:], m.Sum(nil))
	return out
}
