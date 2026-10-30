package names

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Status is what a lookup found.
type Status string

const (
	Verified Status = "verified"  // the record names an identity, is signed by it and names the current owner
	NoRecord Status = "no-record" // the name exists but holds no EchoFog record
	NotFound Status = "not-found" // nobody holds the name
	Stale    Status = "stale"     // the record was made for an earlier owner of the name
	Invalid  Status = "invalid"   // the record is unreadable, or not signed by the identity it names
)

// ErrUnsupported: the name's network is recognised but not looked up yet (.sol).
var ErrUnsupported = errors.New("names of this network cannot be looked up yet")

// Result is a lookup's answer. Only a Verified result may be offered as a contact; a Stale one
// still tells which identity the name used to point at.
type Result struct {
	Name     string
	Network  Network
	Status   Status
	Identity types.IdentityPublicKey // Verified and Stale: the identity the record names
	Owner    string                  // the name's current owner, as its network writes it
	Issued   time.Time               // when the record was signed
	Checked  time.Time               // when the chain was read
	Cached   bool                    // the chain could not be read: this is the last answer
	Problem  string                  // Invalid: why
}

// PeerID is the node the identity runs as.
func (r Result) PeerID() types.PeerID { return PeerID(r.Identity) }

// Resolver looks names up and keeps the answers for a while. The cache is in memory: a found name
// becomes a contact, stored by its key, so nothing has to be looked up again to reach it.
type Resolver struct {
	TON TONSource

	TTL         time.Duration // a found answer (Verified, Stale, NoRecord) is kept this long
	NegativeTTL time.Duration // NotFound and Invalid are kept this long
	MaxStale    time.Duration // when the chain cannot be read, an answer this old is still given
	Now         func() time.Time

	mu    sync.Mutex
	cache map[string]Result
}

// NewResolver reads TON names from ton (nil: TON names are unsupported too).
func NewResolver(ton TONSource) *Resolver {
	return &Resolver{
		TON:         ton,
		TTL:         time.Hour,
		NegativeTTL: 5 * time.Minute,
		MaxStale:    7 * 24 * time.Hour,
		Now:         time.Now,
		cache:       map[string]Result{},
	}
}

// Resolve looks name up. An error means the chain could not be read and nothing recent is known;
// what the chain says, including "nobody holds this name", is in the Result.
func (r *Resolver) Resolve(ctx context.Context, name string) (Result, error) {
	name, network, err := Normalize(name)
	if err != nil {
		return Result{}, err
	}
	now := r.Now()
	r.mu.Lock()
	old, ok := r.cache[name]
	r.mu.Unlock()
	if ok && now.Sub(old.Checked) < r.ttl(old.Status) {
		return old, nil
	}

	res, err := r.lookup(ctx, name, network)
	if err != nil {
		if ok && !errors.Is(err, ErrUnsupported) && now.Sub(old.Checked) < r.MaxStale {
			old.Cached = true
			return old, nil
		}
		return Result{}, err
	}
	res.Checked = now
	r.mu.Lock()
	r.cache[name] = res
	r.mu.Unlock()
	return res, nil
}

// Forget drops a name's answer, so the next lookup reads the chain (a "check again" button).
func (r *Resolver) Forget(name string) {
	if n, _, err := Normalize(name); err == nil {
		r.mu.Lock()
		delete(r.cache, n)
		r.mu.Unlock()
	}
}

func (r *Resolver) ttl(s Status) time.Duration {
	if s == NotFound || s == Invalid {
		return r.NegativeTTL
	}
	return r.TTL
}

func (r *Resolver) lookup(ctx context.Context, name string, network Network) (Result, error) {
	if (network != TON && network != Telegram) || r.TON == nil {
		return Result{}, ErrUnsupported
	}
	e, err := r.TON.LookupTON(ctx, name)
	if err != nil {
		return Result{}, err
	}
	res := Result{Name: name, Network: network}
	if e.Owner != nil {
		res.Owner = e.Owner.String()
	}
	switch {
	case !e.Exists:
		res.Status = NotFound
		return res, nil
	case e.Broken != "":
		res.Status, res.Problem = Invalid, e.Broken
		return res, nil
	case e.Record == nil:
		res.Status = NoRecord
		return res, nil
	}
	b, err := readTONRecord(e.Record)
	if err == nil {
		err = b.Verify(name)
	}
	if err != nil {
		res.Status, res.Problem = Invalid, err.Error()
		return res, nil
	}
	res.Identity, res.Issued = b.Identity, b.Issued
	if e.Owner == nil || !bytes.Equal(b.Owner, TONOwner(e.Owner)) {
		res.Status = Stale
		return res, nil
	}
	res.Status = Verified
	return res, nil
}
