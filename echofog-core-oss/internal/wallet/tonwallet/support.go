package tonwallet

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
)

// Voluntary project support. A share of what the user pays to proxy nodes is set aside for the
// project. It is visible, adjustable and can be switched off with one click: it is a tip, never a
// condition of service. Tips are not sent with every payment: a jetton transfer costs the sender
// roughly 0.02-0.04 TON in network fees, which would exceed a few cents of tip, so they pile up
// and go out as one transfer once the pile is worth the fee.
const (
	DefaultSupportPercent = 5
	MaxSupportPercent     = 20
)

// Thresholds at which an accrued tip is worth sending (fee is well under 15% of it).
var (
	supportFlushUSDT = big.NewInt(1_000_000)   // 1 USDT
	supportFlushTON  = big.NewInt(250_000_000) // 0.25 TON
)

// SupportComment tags tip transfers on-chain so the project can recognise them.
const SupportComment = "echofog support"

var ErrBadSupportPercent = errors.New("support share must be between 0 and 20 percent")

type supportState struct {
	Percent     *int   `json:"percent,omitempty"` // nil: never chosen, use the default
	PendingTON  string `json:"pending_ton"`       // nanoTON
	PendingUSDT string `json:"pending_usdt"`      // 1e-6 USDT
}

// Support tracks the user's tip preference and the tips not yet sent. A zero address disables the
// feature entirely (no project address configured).
type Support struct {
	mu      sync.Mutex
	path    string
	address string
	st      supportState
}

// LoadSupport reads the preference from dir ("" keeps it in memory only).
func LoadSupport(dir, address string) *Support {
	s := &Support{address: address}
	if dir != "" {
		s.path = filepath.Join(dir, "support.json")
		if raw, err := os.ReadFile(s.path); err == nil {
			_ = json.Unmarshal(raw, &s.st)
		}
	}
	return s
}

// Available reports whether a project address is configured.
func (s *Support) Available() bool { return s != nil && s.address != "" }

func (s *Support) Address() string { return s.address }

// Percent is the share in percent, 0 when off or unavailable.
func (s *Support) Percent() int {
	if !s.Available() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.percentLocked()
}

func (s *Support) percentLocked() int {
	if s.st.Percent == nil {
		return DefaultSupportPercent
	}
	return *s.st.Percent
}

// SetPercent changes the share; 0 turns it off.
func (s *Support) SetPercent(p int) error {
	if p < 0 || p > MaxSupportPercent {
		return ErrBadSupportPercent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Percent = &p
	return s.saveLocked()
}

func pendingOf(v string) *big.Int {
	n, ok := new(big.Int).SetString(v, 10)
	if !ok || n.Sign() < 0 {
		return new(big.Int)
	}
	return n
}

// Pending returns the tip set aside but not yet sent, for "TON" or "USDT".
func (s *Support) Pending(asset string) *big.Int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if asset == "TON" {
		return pendingOf(s.st.PendingTON)
	}
	return pendingOf(s.st.PendingUSDT)
}

// Accrue sets aside the tip for a payment of paid (smallest units) and returns it. The payment
// itself is never reduced: the tip is on top, paid later in a batch.
func (s *Support) Accrue(asset string, paid *big.Int) (*big.Int, error) {
	tip := new(big.Int)
	if !s.Available() || paid == nil || paid.Sign() <= 0 {
		return tip, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.percentLocked()
	if p == 0 {
		return tip, nil
	}
	tip.Mul(paid, big.NewInt(int64(p)))
	tip.Quo(tip, big.NewInt(100))
	if tip.Sign() == 0 {
		return tip, nil
	}
	if asset == "TON" {
		s.st.PendingTON = new(big.Int).Add(pendingOf(s.st.PendingTON), tip).String()
	} else {
		s.st.PendingUSDT = new(big.Int).Add(pendingOf(s.st.PendingUSDT), tip).String()
	}
	return tip, s.saveLocked()
}

// Due returns the amount to send now, or nil while the pile is still too small.
func (s *Support) Due(asset string) *big.Int {
	if !s.Available() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, min := pendingOf(s.st.PendingUSDT), supportFlushUSDT
	if asset == "TON" {
		cur, min = pendingOf(s.st.PendingTON), supportFlushTON
	}
	if cur.Cmp(min) < 0 {
		return nil
	}
	return cur
}

// Settled removes a sent amount from the pile.
func (s *Support) Settled(asset string, sent *big.Int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if asset == "TON" {
		s.st.PendingTON = clampSub(pendingOf(s.st.PendingTON), sent).String()
	} else {
		s.st.PendingUSDT = clampSub(pendingOf(s.st.PendingUSDT), sent).String()
	}
	return s.saveLocked()
}

func clampSub(a, b *big.Int) *big.Int {
	r := new(big.Int).Sub(a, b)
	if r.Sign() < 0 {
		return new(big.Int)
	}
	return r
}

func (s *Support) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // atomic: a crash cannot leave half a file
}
