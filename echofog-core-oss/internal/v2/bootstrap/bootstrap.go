// Package bootstrap reads bootstrap lists and joins the network through them (T5.5).
package bootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Kind is the kind of a list entry.
type Kind uint8

const (
	// KindCard is a contact link: the whole signed card.
	KindCard Kind = iota + 1
	// KindDevice is an address with the device ID that must answer on it.
	KindDevice
	// KindAddr is a bare address.
	KindAddr
)

// Entry is one parsed line of a bootstrap list.
type Entry struct {
	Kind   Kind
	Card   *objects.Card
	Device [32]byte
	Addr   *pb.Addr
	Mask   []byte
}

// Host is what the service needs from the runtime. *node.Runtime satisfies it.
type Host interface {
	Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error)
	DialBootstrap(ctx context.Context, t node.BootstrapTarget, opt node.DialOpt) (*node.BootstrapResult, error)
}

// Parse parses a bootstrap list.
func Parse(text string, now time.Time) ([]Entry, error) {
	return nil, errors.New("bootstrap: Parse is not implemented")
}

// Candidate is a card learned through a bare address: nothing in it is trusted.
type Candidate struct {
	Card *objects.Card
}

// Report is the outcome of one Run.
type Report struct {
	Joined     int
	Candidates []Candidate
	Failed     int
}

// Service joins the network through a list.
type Service struct{}

// New returns a service over host.
func New(host Host, entries []Entry, now func() time.Time) *Service { return &Service{} }

// Run dials every entry once.
func (s *Service) Run(ctx context.Context) Report { return Report{} }
