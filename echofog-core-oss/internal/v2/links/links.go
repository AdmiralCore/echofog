// Package links is the application layer of v2 links (objects.md §10): the node's contact link, the
// invitation links of a hidden node, the offline preview of a link and joining by it.
//
// The format and the card/link match rule live in objects (ParseLink, EncodeLink, Link.Verify);
// this package only decides what a node does with them. A link is untrusted input from a chat, a
// web page or a QR code: Open works offline, bounded, and never touches the network or the DHT.
package links

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Errors for the UI; UserMessage turns them into Russian texts.
var (
	// ErrNotLink reports text that is not an EchoFog v2 link, or a link that cannot be parsed.
	ErrNotLink = errors.New("links: not an EchoFog link")
	// ErrOtherVersion reports a link whose v is not 2 ("made by another version").
	ErrOtherVersion = errors.New("links: link made by another version")
	// ErrMismatch reports a link and card that do not match (objects.md §10.1).
	ErrMismatch = errors.New("links: link and card do not match")
	// ErrExpired reports a link whose card has expired.
	ErrExpired = errors.New("links: link expired")
	// ErrRevoked reports an invitation that could not be used. A hidden node closes silently on
	// every failure (handshake.md §5), so a revoked, expired or used-up invitation and an offline
	// node look the same from outside: the joiner is told "revoked or no longer valid".
	ErrRevoked = errors.New("links: link revoked")
	// ErrNoContactLink reports a node without a public card (hidden, anonymous, or no cover).
	ErrNoContactLink = errors.New("links: node has no contact link")
	// ErrNoInviter reports Invite on a node without an access manager.
	ErrNoInviter = errors.New("links: node cannot mint invitations")
	// ErrNoDialer reports Redeem on a service without a dialer.
	ErrNoDialer = errors.New("links: no dialer")
)

// Kind is the type of a link.
type Kind uint8

const (
	// KindContact is a link without an access secret: the public card of a node.
	KindContact Kind = iota + 1
	// KindInvitation is a link with an access secret: the invitation card of a hidden node.
	KindInvitation
)

func (k Kind) String() string {
	switch k {
	case KindContact:
		return "contact"
	case KindInvitation:
		return "invitation"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Preview is what a link shows before joining. It never carries the access secret.
type Preview struct {
	Kind   Kind
	Node   [32]byte
	Device [32]byte
	// NotAfter is when the card in the link expires.
	NotAfter time.Time
	// Card is the verified card.
	Card *objects.Card
}

// Clock is the node's corrected clock (AD13).
type Clock interface{ Now() time.Time }

// CardSource gives the node's current card of a scope. *cards.CardBuilder satisfies it.
type CardSource interface {
	Current(ctx context.Context, scope objects.Scope) (*cards.IssuedCard, error)
}

// Inviter mints invitations. *access.Manager satisfies it.
type Inviter interface {
	Mint(ctx context.Context, opt access.MintOptions) (*access.Minted, error)
}

// Dialer opens a session to a device. *node.Runtime satisfies it.
type Dialer interface {
	Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error)
}

// Config configures a Service. Every part but the clock is optional: a node that does not mint
// invitations has no Inviter, a preview-only service has no Dialer.
type Config struct {
	Cards   CardSource
	Inviter Inviter
	Dialer  Dialer
	// Clock is required.
	Clock Clock
	// JoinHost is the host of the join page for the web form of links ("" = no web form).
	JoinHost string
}

// Link is a contact link and its web form.
type Link struct {
	URL     string
	Web     string
	Expires time.Time
}

// Invitation is a freshly minted invitation link. Its URL is a bearer secret (security.md §13): it
// is shown to its owner once and never printed by String, %v, %+v, %#v or a logger.
type Invitation struct {
	PassID     access.PassID
	URL        string
	Web        string
	Expires    time.Time
	MaxEntries uint32
}

func (i Invitation) redacted() string {
	return fmt.Sprintf("links.Invitation{pass %s, expires %s, entries %d, link hidden}",
		i.PassID, i.Expires.UTC().Format(time.RFC3339), i.MaxEntries)
}

// String implements fmt.Stringer without the link.
func (i Invitation) String() string { return i.redacted() }

// GoString implements fmt.GoStringer without the link.
func (i Invitation) GoString() string { return i.redacted() }

// LogValue implements slog.LogValuer without the link.
func (i Invitation) LogValue() slog.Value { return slog.StringValue(i.redacted()) }

// Service is the link service of one node. It is safe for concurrent use.
type Service struct {
	cfg Config
}

// New creates a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Clock == nil {
		return nil, errors.New("links: a clock is required")
	}
	if h := cfg.JoinHost; h != "" && (strings.ContainsAny(h, "/?#@\\ \t\r\n") || strings.Contains(h, "://")) {
		return nil, fmt.Errorf("links: join host %q", h)
	}
	return &Service{cfg: cfg}, nil
}

func (s *Service) web(link string) string {
	if s.cfg.JoinHost == "" {
		return ""
	}
	return objects.WebLink(s.cfg.JoinHost, link)
}

// ContactLink returns the link of the node's current public card (objects.md §10.2).
func (s *Service) ContactLink(ctx context.Context) (*Link, error) {
	if s.cfg.Cards == nil {
		return nil, ErrNoContactLink
	}
	ic, err := s.cfg.Cards.Current(ctx, objects.ScopePublic)
	if err != nil {
		if errors.Is(err, cards.ErrNoPublicCard) {
			return nil, fmt.Errorf("%w: %v", ErrNoContactLink, err)
		}
		return nil, err
	}
	url, err := objects.EncodeLink(ic.Envelope, nil)
	if err != nil {
		return nil, err
	}
	return &Link{URL: url, Web: s.web(url), Expires: time.Unix(int64(ic.NotAfter), 0)}, nil
}

// Invite mints an invitation of a hidden node and returns its link (objects.md §10.3). The secret
// leaves the access manager only inside the link.
func (s *Service) Invite(ctx context.Context, opt access.MintOptions) (*Invitation, error) {
	if s.cfg.Inviter == nil {
		return nil, ErrNoInviter
	}
	m, err := s.cfg.Inviter.Mint(ctx, opt)
	if err != nil {
		return nil, err
	}
	for i := range m.Secret {
		m.Secret[i] = 0
	}
	return &Invitation{PassID: m.PassID, URL: m.Link, Web: s.web(m.Link), Expires: m.LinkExpires, MaxEntries: m.MaxEntries}, nil
}

// Open parses and verifies a link offline at the service clock.
func (s *Service) Open(link string) (*Preview, error) { return Open(link, s.cfg.Clock.Now()) }

// Open parses and verifies a link offline at now: no network, no DHT (objects.md §10.1: the link is
// complete). Surrounding white space is ignored; the v1 slash form is accepted.
func Open(link string, now time.Time) (*Preview, error) {
	p, _, err := open(link, now)
	return p, err
}

func open(text string, now time.Time) (*Preview, *objects.Link, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil, fmt.Errorf("%w: empty", ErrNotLink)
	}
	l, err := objects.ParseLink(text)
	switch {
	case errors.Is(err, objects.ErrOtherVersion):
		return nil, nil, ErrOtherVersion
	case err != nil:
		return nil, nil, fmt.Errorf("%w: %v", ErrNotLink, err)
	}
	c, err := l.Verify(now)
	if err != nil {
		// Tell a card that is bad by itself from a good card in the wrong kind of link.
		_, cerr := objects.VerifyCard(l.Card, now)
		switch {
		case cerr == nil:
			return nil, nil, fmt.Errorf("%w: %v", ErrMismatch, err)
		case errors.Is(cerr, objects.ErrExpired):
			return nil, nil, ErrExpired
		default:
			return nil, nil, fmt.Errorf("%w: card: %v", ErrNotLink, cerr)
		}
	}
	kind := KindContact
	if l.Secret != nil {
		kind = KindInvitation
	}
	return &Preview{
		Kind: kind, Node: c.NodeID, Device: c.DeviceID,
		NotAfter: time.Unix(int64(c.Env.NotAfter), 0), Card: c,
	}, l, nil
}

// Redeem joins by a link: it opens the link, then starts a session to the device of its card, with
// the access secret for an invitation. The node that minted the invitation redeems it in the first
// complete session (access.Manager.Redeem); the joiner receives the personal pass and the peer card
// over that session.
func (s *Service) Redeem(ctx context.Context, link string) (node.Peer, error) {
	pv, l, err := open(link, s.cfg.Clock.Now())
	if err != nil {
		return node.Peer{}, err
	}
	if s.cfg.Dialer == nil {
		return node.Peer{}, ErrNoDialer
	}
	secret := l.Secret
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	device := node.DeviceID(pv.Device)
	peer, _, err := s.cfg.Dialer.Session(ctx, device, node.DialOpt{Card: pv.Card, AccessSecret: secret})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return node.Peer{}, err
		}
		// A refusal of the route policy is a clear answer, not a silent close: it must not read as
		// "revoked".
		if errors.Is(err, node.ErrNoDomesticEntry) || errors.Is(err, node.ErrRouteUnsupported) {
			return node.Peer{}, err
		}
		if pv.Kind == KindInvitation {
			return node.Peer{}, fmt.Errorf("%w: %v", ErrRevoked, err)
		}
		return node.Peer{}, err
	}
	if peer.Device != device || peer.Node != node.NodeID(pv.Node) {
		return node.Peer{}, errors.New("links: the session is not with the device of the link")
	}
	return peer, nil
}

// UserMessage is the Russian text of an error for the UI, in a neutral form. It never repeats the
// internal error text.
func UserMessage(err error) string {
	switch {
	case errors.Is(err, ErrOtherVersion):
		return "Ссылка создана другой версией EchoFog"
	case errors.Is(err, ErrRevoked):
		return "Ссылка отозвана или больше не действует"
	case errors.Is(err, ErrNotLink):
		return "Это не ссылка EchoFog"
	case errors.Is(err, ErrMismatch):
		return "Ссылка повреждена: данные в ней не совпадают"
	case errors.Is(err, ErrExpired):
		return "Ссылка недействительна: истёк срок её действия"
	case errors.Is(err, ErrNoContactLink):
		return "У этого узла нет публичной ссылки"
	case errors.Is(err, ErrNoInviter):
		return "Этот узел не создаёт приглашения"
	default:
		return "Не удалось использовать ссылку"
	}
}
