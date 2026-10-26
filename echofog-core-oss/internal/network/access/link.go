package access

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// An invitation link names a node and how to reach it, and for a hidden or isolated node carries
// its access secret:
//
//	echofog://join?v=1&net=echofog&p=101&id=<peer id>&s=<secret>&a=<host:port>&r=<host:port/relay id>
//
// v is the link format, net the network (a fork names its own), p the protocol the node speaks,
// a an address to dial (repeated), r a relay the node is reachable through when no address is
// (repeated). Without s it is the address of a public node.

// LinkVersion is the newest link format this build reads and the one it writes.
const LinkVersion = 1

const linkPrefix = "echofog://join?"

var (
	// ErrNotInvite: the text is not an invitation link at all.
	ErrNotInvite = errors.New("not an invitation link")
	// ErrLinkTooNew: a newer app made the link; this one has to be updated to read it.
	ErrLinkTooNew = errors.New("the link was made by a newer version of the app: update it")
)

// OtherNetworkError: the link belongs to another network (a fork, or the original for a fork).
type OtherNetworkError struct{ Network string }

func (e *OtherNetworkError) Error() string {
	return fmt.Sprintf("the link is for the network %q, this app is for another one", e.Network)
}

// Invite is what a link says.
type Invite struct {
	Version  int
	Network  string
	Protocol uint32
	Peer     types.PeerID
	Secret   []byte   // nil: a public node, no secret needed
	Addrs    []string // "host:port" or ws(s):// URLs, to dial directly
	Relays   []string // "host:port/<relay peer id>", through which the node can be reached
	SNI      string   // target node's camouflage SNI to use for TLS/QUIC
}

// IsLink reports whether text looks like an invitation link (it may still be unreadable).
func IsLink(text string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), linkPrefix)
}

// FormatLink writes inv as a link, in the current format.
func FormatLink(inv Invite) string {
	q := url.Values{}
	q.Set("v", strconv.Itoa(LinkVersion))
	q.Set("net", inv.Network)
	if inv.Protocol > 0 {
		q.Set("p", strconv.FormatUint(uint64(inv.Protocol), 10))
	}
	q.Set("id", inv.Peer.String())
	if inv.Secret != nil {
		q.Set("s", base64.RawURLEncoding.EncodeToString(inv.Secret))
	}
	if inv.SNI != "" {
		q.Set("sni", inv.SNI)
	}
	for _, a := range inv.Addrs {
		q.Add("a", a)
	}
	for _, r := range inv.Relays {
		q.Add("r", r)
	}
	// a fixed order keeps a link the same for the same node, and readable
	var b strings.Builder
	b.WriteString(linkPrefix)
	first := true
	for _, k := range []string{"v", "net", "p", "id", "s", "sni", "a", "r"} {
		for _, v := range q[k] {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(k + "=" + url.QueryEscape(v))
		}
	}
	return b.String()
}

// ParseLink reads a link. network is this build's network: a link of another one gives
// *OtherNetworkError, one of a newer format ErrLinkTooNew.
func ParseLink(text, network string) (Invite, error) {
	text = strings.TrimSpace(text)
	if !IsLink(text) {
		return Invite{}, ErrNotInvite
	}
	q, err := url.ParseQuery(text[len(linkPrefix):])
	if err != nil {
		return Invite{}, fmt.Errorf("broken invitation link: %w", err)
	}
	var inv Invite
	if inv.Version, err = strconv.Atoi(q.Get("v")); err != nil || inv.Version < 1 {
		return Invite{}, errors.New("broken invitation link: no format version")
	}
	if inv.Version > LinkVersion {
		return Invite{}, ErrLinkTooNew
	}
	inv.Network = q.Get("net")
	if inv.Network != network {
		return Invite{}, &OtherNetworkError{Network: inv.Network}
	}
	if p := q.Get("p"); p != "" {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return Invite{}, errors.New("broken invitation link: bad protocol version")
		}
		inv.Protocol = uint32(v)
	}
	if inv.Peer, err = types.PeerIDFromString(strings.ToLower(q.Get("id"))); err != nil {
		return Invite{}, errors.New("broken invitation link: bad node id")
	}
	if s := q.Get("s"); s != "" {
		if inv.Secret, err = base64.RawURLEncoding.DecodeString(s); err != nil || len(inv.Secret) != SecretLen {
			return Invite{}, errors.New("broken invitation link: bad secret")
		}
	}
	inv.SNI = strings.TrimSpace(q.Get("sni"))
	inv.Addrs = q["a"]
	inv.Relays = q["r"]
	if len(inv.Addrs) > 16 || len(inv.Relays) > 16 {
		return Invite{}, errors.New("broken invitation link: too many addresses")
	}
	if len(inv.Addrs) == 0 && len(inv.Relays) == 0 {
		return Invite{}, errors.New("broken invitation link: no address to reach the node at")
	}
	return inv, nil
}

// EncodeSecret and DecodeSecret write a secret as it appears in links and settings.
func EncodeSecret(s []byte) string { return base64.RawURLEncoding.EncodeToString(s) }

func DecodeSecret(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != SecretLen {
		return nil, ErrBadSecret
	}
	return b, nil
}
