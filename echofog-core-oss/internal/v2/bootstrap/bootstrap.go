// Package bootstrap reads bootstrap lists and joins the network through them (T5.5).
//
// A list has one entry per line; empty lines and lines starting with '#' are ignored:
//
//	echofog://join?v=2&card=...                              the whole signed card (contact link)
//	noise3://HOST:PORT?mask=HEX64&device=HEX64               an address and the device ID that must answer
//	reality2-quic://HOST:PORT?mask=HEX64&sni=NAME[&device=]  the same for a camouflaged suite
//	noise3://HOST:PORT?mask=HEX64                            a bare address: no device ID, nothing is trusted
//
// The build has no baked-in addresses (OSS policy): the list comes from the user, a file or a flag.
// An invitation link is refused: its access secret must not sit in a shared list.
//
// What an entry gives the node (handshake.md section 4.2, wire.md section 1):
//   - a card entry is dialed with XK like any card and, once the session is authenticated, enters the
//     routing table through the normal session path;
//   - a device entry is dialed with XX and the expected device ID; only a result that the handshake
//     checked against that ID is used, and the node then joins through its card;
//   - a bare address is dialed with XX under a throwaway identity. The card it returns is only a
//     candidate: the service has no access to the routing table, the card book or the peer store, so
//     it cannot enter any of them as verified.
package bootstrap

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Limits of a list.
const (
	MaxListBytes = 64 << 10
	MaxEntries   = 64
	MaxLineBytes = 8 << 10

	entryTimeout = 30 * time.Second
	workers      = 4
)

// ErrList reports a bad bootstrap list.
var ErrList = errors.New("bootstrap: bad list")

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
	Kind Kind
	// Card is set for KindCard.
	Card *objects.Card
	// Device is set for KindDevice.
	Device [32]byte
	// Addr and Mask are set for KindDevice and KindAddr.
	Addr *pb.Addr
	Mask keys.AKey
}

// Host is what the service needs from the runtime. *node.Runtime satisfies it.
type Host interface {
	Session(ctx context.Context, peer node.DeviceID, opt node.DialOpt) (node.Peer, *mux.Session, error)
	DialBootstrap(ctx context.Context, t node.BootstrapTarget, opt node.DialOpt) (*node.BootstrapResult, error)
}

// Parse parses a bootstrap list at now. An error names the line.
func Parse(text string, now time.Time) ([]Entry, error) {
	if len(text) > MaxListBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrList, MaxListBytes)
	}
	var out []Entry
	seen := make(map[string]struct{})
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > MaxLineBytes {
			return nil, fmt.Errorf("%w: line %d is too long", ErrList, i+1)
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		e, err := parseLine(line, now)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrList, i+1, err)
		}
		if len(out) == MaxEntries {
			return nil, fmt.Errorf("%w: more than %d entries", ErrList, MaxEntries)
		}
		out = append(out, e)
	}
	return out, nil
}

func parseLine(line string, now time.Time) (Entry, error) {
	if strings.HasPrefix(line, "echofog:") {
		p, err := links.Open(line, now)
		if err != nil {
			return Entry{}, errors.New(links.UserMessage(err))
		}
		if p.Kind != links.KindContact {
			return Entry{}, errors.New("an invitation link does not belong in a bootstrap list")
		}
		return Entry{Kind: KindCard, Card: p.Card}, nil
	}
	return parseAddress(line)
}

var suiteByName = map[string]uint32{
	"noise3":        objects.SuiteNoise3,
	"reality2-quic": objects.SuiteReality2QUIC,
	"reality2-tcp":  objects.SuiteReality2TCP,
}

func parseAddress(line string) (Entry, error) {
	u, err := url.Parse(line)
	if err != nil || u.Scheme == "" {
		return Entry{}, errors.New("not a bootstrap entry")
	}
	suite, ok := suiteByName[u.Scheme]
	if !ok {
		return Entry{}, fmt.Errorf("unknown suite %q", u.Scheme)
	}
	host := u.Hostname()
	port, perr := strconv.ParseUint(u.Port(), 10, 16)
	if host == "" || perr != nil || port == 0 || len(host) > 253 {
		return Entry{}, errors.New("bad host or port")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.Fragment != "" {
		return Entry{}, errors.New("unexpected parts in the address")
	}
	q := u.Query()
	mask, err := hexN(q.Get("mask"), 32)
	if err != nil {
		return Entry{}, fmt.Errorf("mask: %v", err)
	}
	a := &pb.Addr{Suite: suite, Host: host, Port: uint32(port), Mask: 1, Flags: objects.AddrFlagBootstrap}
	if suite != objects.SuiteNoise3 {
		sni := q.Get("sni")
		if sni == "" || len(sni) > 253 {
			return Entry{}, errors.New("sni is required")
		}
		a.Params, err = objects.Marshal(&pb.SuiteParams{Sni: sni})
		if err != nil {
			return Entry{}, err
		}
	}
	e := Entry{Kind: KindAddr, Addr: a, Mask: keys.New(keys.X25519, mask)}
	if d := q.Get("device"); d != "" {
		id, err := hexN(d, 32)
		if err != nil {
			return Entry{}, fmt.Errorf("device: %v", err)
		}
		e.Kind = KindDevice
		copy(e.Device[:], id)
	}
	return e, nil
}

func hexN(s string, n int) ([]byte, error) {
	if len(s) != 2*n {
		return nil, fmt.Errorf("want %d hex digits", 2*n)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("not hex")
	}
	return b, nil
}

// Candidate is a card learned through a bare address: nothing in it is trusted. It may be used
// only as a lead to verify through other, authenticated sessions.
type Candidate struct {
	Card *objects.Card
}

// Report is the outcome of one Run.
type Report struct {
	// Joined counts entries through which the node holds an authenticated session.
	Joined int
	// Candidates are the cards of bare addresses.
	Candidates []Candidate
	// Failed counts entries that gave nothing.
	Failed int
}

// Service joins the network through a list.
type Service struct {
	host    Host
	entries []Entry
	log     *slog.Logger
}

// New returns a service over host.
func New(host Host, entries []Entry, _ func() time.Time) *Service {
	return &Service{host: host, entries: entries, log: slog.Default()}
}

// Run dials every entry once, at most four at a time, each under a 30 s limit.
func (s *Service) Run(ctx context.Context) Report {
	var (
		mu  sync.Mutex
		rep Report
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, workers)
loop:
	for _, e := range s.entries {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(e Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			ectx, cancel := context.WithTimeout(ctx, entryTimeout)
			defer cancel()
			cand, joined := s.one(ectx, e)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case joined:
				rep.Joined++
			case cand != nil:
				rep.Candidates = append(rep.Candidates, Candidate{Card: cand})
			default:
				rep.Failed++
			}
		}(e)
	}
	wg.Wait()
	return rep
}

// one dials one entry. It returns the candidate card of a bare address, or joined for an
// authenticated session.
func (s *Service) one(ctx context.Context, e Entry) (cand *objects.Card, joined bool) {
	switch e.Kind {
	case KindCard:
		return nil, s.join(ctx, e.Card)
	case KindDevice:
		dev := node.DeviceID(e.Device)
		res, err := s.host.DialBootstrap(ctx, node.BootstrapTarget{Addr: e.Addr, Mask: e.Mask, Expected: &dev}, node.DialOpt{})
		if err != nil || res == nil || !res.Verified || res.Card == nil || res.Card.DeviceID != e.Device {
			s.log.Debug("bootstrap: device entry gave nothing", "err", err)
			return nil, false
		}
		return nil, s.join(ctx, res.Card)
	case KindAddr:
		res, err := s.host.DialBootstrap(ctx, node.BootstrapTarget{Addr: e.Addr, Mask: e.Mask}, node.DialOpt{})
		if err != nil || res == nil || res.Card == nil {
			s.log.Debug("bootstrap: bare address gave nothing", "err", err)
			return nil, false
		}
		// Not verified, whatever the result says.
		return res.Card, false
	}
	return nil, false
}

func (s *Service) join(ctx context.Context, card *objects.Card) bool {
	if card == nil {
		return false
	}
	_, _, err := s.host.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{Card: card})
	if err != nil {
		s.log.Debug("bootstrap: session failed", "err", err)
		return false
	}
	return true
}
