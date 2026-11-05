// Package links is the application layer of v2 links (objects.md §10): the node's contact link, the
// invitation links of a hidden node, the offline preview of a link and joining by it.
package links

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Errors for the UI; UserMessage turns them into Russian texts.
var (
	ErrNotLink       = errors.New("links: not an EchoFog link")
	ErrOtherVersion  = errors.New("links: link made by another version")
	ErrMismatch      = errors.New("links: link and card do not match")
	ErrExpired       = errors.New("links: link expired")
	ErrRevoked       = errors.New("links: link revoked")
	ErrNoContactLink = errors.New("links: node has no contact link")
	ErrNoInviter     = errors.New("links: node cannot mint invitations")
	ErrNoDialer      = errors.New("links: no dialer")
)

var errNotImplemented = errors.New("links: not implemented")

// Kind is the type of a link.
type Kind uint8

const (
	KindContact Kind = iota + 1
	KindInvitation
)

// Preview is what a link shows before joining. It never carries the access secret.
type Preview struct {
	Kind     Kind
	Node     [32]byte
	Device   [32]byte
	NotAfter time.Time
	Card     *objects.Card
}

// Clock is the injected clock.
type Clock interface{ Now() time.Time }

// CardSource gives the node's current card of a scope. *cards.CardBuilder satisfies it.
type CardSource interface {
	Current(ctx context.Context, scope objects.Scope) (*cards.IssuedCard, error)
}

// Inviter mints invitations. *access.Manager satisfies it.
type Inviter interface {
	Mint(ctx context.Context, opt access.MintOptions) (*access.Minted, error)
}

// Dialer opens a session. *node.Runtime satisfies it.
type Dialer interface {
	Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error)
}

// Config configures a Service.
type Config struct {
	Cards    CardSource
	Inviter  Inviter
	Dialer   Dialer
	Clock    Clock
	JoinHost string
}

// Link is a link and its web form.
type Link struct {
	URL, Web string
	Expires  time.Time
}

// Invitation is a freshly minted invitation link. Its URL is a bearer secret: it is never printed
// by String or %v.
type Invitation struct {
	PassID     access.PassID
	URL, Web   string
	Expires    time.Time
	MaxEntries uint32
}

// Service is the link service of one node.
type Service struct{}

// New creates a Service.
func New(Config) (*Service, error) { return &Service{}, nil }

// ContactLink returns the link of the node's current public card.
func (s *Service) ContactLink(context.Context) (*Link, error) { return nil, errNotImplemented }

// Invite mints an invitation and returns its link.
func (s *Service) Invite(context.Context, access.MintOptions) (*Invitation, error) {
	return nil, errNotImplemented
}

// Open parses and verifies a link offline.
func (s *Service) Open(string) (*Preview, error) { return nil, errNotImplemented }

// Open parses and verifies a link offline at now.
func Open(string, time.Time) (*Preview, error) { return nil, errNotImplemented }

// Redeem joins by a link.
func (s *Service) Redeem(context.Context, string) (node.Peer, error) {
	return node.Peer{}, errNotImplemented
}

// UserMessage is the Russian text of an error for the UI.
func UserMessage(error) string { return "" }

// String is the text form of an invitation (stub).
func (i Invitation) String() string { return i.URL }
