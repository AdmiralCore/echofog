package dht

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Red-commit stub of the lookup client (T5.2): the API the tests are written against, with no
// behaviour. The implementation replaces every body in the next commit.

var errNotImplemented = errors.New("dht: not implemented")

// MaxQueries is the query budget of one path.
const MaxQueries = 64

var (
	ErrNotFound     = errors.New("dht: not found")
	ErrInconclusive = errors.New("dht: lookup inconclusive")
	ErrRollback     = errors.New("dht: older than a record seen before")
)

// RemoteError is an ERROR frame of a peer.
type RemoteError struct{ Code uint32 }

func (e *RemoteError) Error() string { return "dht: remote error" }

// Transport sends the dht/2 requests of one lookup.
type Transport interface {
	FindNode(ctx context.Context, to *objects.Card, target [32]byte) ([][]byte, error)
	FindValue(ctx context.Context, to *objects.Card, key [32]byte) ([]byte, [][]byte, error)
	Store(ctx context.Context, to *objects.Card, record []byte) (uint32, error)
}

// ClientConfig wires a Client.
type ClientConfig struct {
	Self      DeviceID
	Table     *Table
	Transport Transport
	Memory    store.KV
	Now       func() time.Time
	Rand      *rand.Rand
	OnQuery   func(path int, to DeviceID)
}

// Client is the lookup and publishing side of dht/2.
type Client struct{}

// NewClient builds a client.
func NewClient(ClientConfig) (*Client, error) { return &Client{}, nil }

// PutResult counts the STORED statuses of one Put.
type PutResult struct{ Stored, Stale, Refused, Quota, Invalid, Failed int }

// Resolved is the answer of Resolve.
type Resolved struct {
	Devices *objects.DeviceList
	Cards   []*objects.Card
	Prekeys []*objects.Prekeys
}

func (c *Client) FindNode(context.Context, [32]byte) ([]*objects.Card, error) {
	return nil, errNotImplemented
}
func (c *Client) Get(context.Context, string, [32]byte) (*objects.Envelope, error) {
	return nil, errNotImplemented
}
func (c *Client) Put(context.Context, []byte) (PutResult, error) {
	return PutResult{}, errNotImplemented
}
func (c *Client) Publish([]byte) error { return errNotImplemented }
func (c *Client) Step(context.Context) {}
func (c *Client) Resolve(context.Context, [32]byte) (*Resolved, error) {
	return nil, errNotImplemented
}

// Replicas is the number of replicas of a label (registries.md §9).
func Replicas(string) int { return 0 }

// RequiresDisjointPaths reports whether a label needs three disjoint paths (dht.md §4.2).
func RequiresDisjointPaths(string) bool { return false }

func halfLifeDelay(remaining time.Duration, _ *rand.Rand) time.Duration { return remaining }

func acceptRecord(string, [32]byte, []byte, time.Time) (*objects.Envelope, error) {
	return nil, errNotImplemented
}

func DecodeValue(uint64, uint64, []byte) ([]byte, [][]byte, error) {
	return nil, nil, errNotImplemented
}
func DecodeNodes(uint64, uint64, []byte) ([][]byte, error) { return nil, errNotImplemented }
func DecodeStored(uint64, uint64, []byte) (uint32, error)  { return 0, errNotImplemented }
