// Package convsync carries the conversation log of v1 (internal/services/convlog) over chat/2: the
// signed events and the version-vector sync travel as the content of end-to-end messages
// (PersonalMessageProto.conv_event and conv_sync, services.md 4.4), to the accounts of a direct chat or
// of a group, whatever device they use. The log decides everything about a conversation (order,
// membership, rights at the point of an event); this package only moves its bytes.
//
// Accounts are named by their Ed25519 public key (the log's author) and addressed by the node ID of
// that key (SHA-256 of its akey); the chat service picks the device.
package convsync

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/services/convlog"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Limits of the transport.
const (
	// MeshLimit is the group size up to which an author sends an event to every member.
	MeshLimit = 50
	// MaxFanout is how many members the author of an event in a larger group sends it to.
	MaxFanout = 8
	// EventBurst is how many events one peer may hand us in an instant; more are dropped and come with
	// the sync.
	EventBurst = 200
)

// StubText is what the app shows for an event of a kind this build does not know.
const StubText = "Сообщение из новой версии приложения — обновите приложение"

// Net sends a message to an account; *chat.Service does (Send), tests use a queue.
type Net interface {
	Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) error
}

// Directory finds the public key of the account behind a node ID.
type Directory interface {
	Pub(ctx context.Context, node [32]byte) ([32]byte, error)
}

// Config configures an Engine.
type Config struct {
	// Pub is the account's Ed25519 public key; Sign signs with its private key.
	Pub  [32]byte
	Sign func([]byte) ([]byte, error)
	// Store is the namespace of the log; IDs blinds identifiers in its keys.
	Store store.KV
	IDs   *store.IDs
	// Rules tells the log about groups (founder, roles); nil means direct chats only.
	Rules convlog.GroupRules
	Net   Net
	Dir   Directory
	// Groups resolves a conversation id a peer names to a group we are in.
	Groups func(convlog.ConvID) (convlog.Conv, bool)
	// Members lists the public keys of a group's members.
	Members func(convlog.Conv) [][32]byte
	// SharedGroups lists the groups we and a peer are both in.
	SharedGroups func(peer [32]byte) []convlog.Conv
	Now          func() time.Time
}

// Engine moves the events of one account's conversation log.
type Engine struct{}

var errNotImplemented = errors.New("convsync: not implemented")

// New opens the log.
func New(cfg Config) (*Engine, error) { return nil, errNotImplemented }

// Log is the conversation log.
func (e *Engine) Log() *convlog.Log { return nil }

// NodeOf is the node ID of an account key.
func NodeOf(pub [32]byte) [32]byte { return [32]byte{} }

// DM is the direct conversation with an account.
func (e *Engine) DM(peer [32]byte) convlog.Conv { return convlog.Conv{} }

// Publish sends an event this account appended to the others of the conversation: all of them, or at
// most MaxFanout of a large group (the rest get it from sync and from the ones that pass it on).
func (e *Engine) Publish(ctx context.Context, c convlog.Conv, raw []byte) error {
	return errNotImplemented
}

// Receive takes a message from the account behind node: a log event or a sync message. It reports
// whether the message was one of those.
func (e *Engine) Receive(ctx context.Context, from [32]byte, pm *chatpb.PersonalMessageProto) bool {
	return false
}

// Hello opens a sync with a peer for the conversations both are in.
func (e *Engine) Hello(ctx context.Context, peer [32]byte) error { return errNotImplemented }

// Stub is an event of a kind this build does not know: stored, synced and passed on, shown as a stub.
type Stub struct {
	ID     convlog.EventID
	Author [32]byte
	Kind   uint32
	TsMs   int64
	Text   string
}

// Stubs lists the events of c whose kind this build does not know, in log order.
func (e *Engine) Stubs(c convlog.Conv) []Stub { return nil }
