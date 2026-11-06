package chat

import (
	"context"
	"errors"
	"time"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Crypto is the key agreement and ratchet of chat/2; *chatcrypto.Crypto satisfies it.
type Crypto interface {
	Start(peer *objects.Prekeys, dl *objects.DeviceList, first []byte) ([]byte, error)
	Accept(sender *objects.Prekeys, dl *objects.DeviceList, init []byte) ([]byte, error)
	Encrypt(device [32]byte, plain []byte) ([]byte, error)
	Decrypt(device [32]byte, msg []byte) ([]byte, error)
	Peer(device [32]byte) ([32]byte, bool)
	OpenEnvelope(self [32]byte, env []byte) ([]byte, error)
}

// Directory finds the devices and prekeys of an account; *dht.Client satisfies it.
type Directory interface {
	Resolve(ctx context.Context, node [32]byte) (*dht.Resolved, error)
}

// Link sends a DELIVER to a device over a session and returns the DELIVERED status. An error means
// the device could not be reached (no session, timeout): the message then goes to a mailbox.
type Link interface {
	Deliver(ctx context.Context, device [32]byte, envelope []byte) (uint32, error)
}

// Mail deposits an envelope for an account that is away (mailbox/2, T7.4). Until T7.4 a fake.
type Mail interface {
	Deposit(ctx context.Context, node [32]byte, envelope []byte) error
}

// State is where an outgoing message is (H17).
type State uint8

const (
	// StateWaiting: waiting for a connection.
	StateWaiting State = iota + 1
	// StateMailbox: handed to a mailbox; not yet confirmed by the recipient.
	StateMailbox
	// StateDelivered: the recipient confirmed it.
	StateDelivered
	// StateFailed: it cannot be sent (too large).
	StateFailed
)

// Incoming is a received message.
type Incoming struct {
	ID      [16]byte
	From    [32]byte // account node ID
	Device  [32]byte
	Message *chatpb.PersonalMessageProto
}

// Events receives what happens; any field may be nil.
type Events struct {
	Message func(Incoming)
	Status  func(id [16]byte, st State)
	// Control is a knock, a reset or a receipt that was not a delivery confirmation of ours.
	Control func(from [32]byte, c *chatpb.Control)
}

// Self is the local identity.
type Self struct {
	Node      [32]byte
	Device    [32]byte
	Account   keys.AKey // akey of the account key (Inner.sender)
	DeviceKey keys.AKey // akey of the device key (Inner.device)
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Self   Self
	Crypto Crypto
	Dir    Directory
	Link   Link
	Mail   Mail
	Store  store.KV
	Now    func() time.Time
	Events Events
	// NoJitter turns the ±20 % of the retry pauses off (tests, AD13).
	NoJitter bool
}

// Service is the delivery side of chat/2: outbox, receiving, deduplication and recovery.
type Service struct{}

var errNotImplemented = errors.New("chat: not implemented")

// NewService builds a Service and loads the outbox from the store.
func NewService(cfg ServiceConfig) (*Service, error) { return nil, errNotImplemented }

// Send queues a message for the account to and returns its ID. The message is stored before this
// returns, so it survives a restart.
func (s *Service) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	return [16]byte{}, errNotImplemented
}

// Status reports where an outgoing message is.
func (s *Service) Status(id [16]byte) (State, bool) { return 0, false }

// Tick works on the outbox items that are due. The runtime calls it on a timer.
func (s *Service) Tick(ctx context.Context) error { return errNotImplemented }

// Receive handles one incoming envelope (from a DELIVER or from a mailbox) and returns the DELIVERED
// status.
func (s *Service) Receive(ctx context.Context, envelope []byte) uint32 { return chatpb.StatusRefused }
