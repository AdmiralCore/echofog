package reality2

import (
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Suite is this package's L0 suite number (registries.md §6.1).
const Suite = objects.SuiteReality2QUIC

// Register installs the suite's dialer and listener in reg. Either may be nil. Until T2.6b
// a listener only exists together with a measured-by-hand cover profile, so a caller that
// tries to register a listener without one has already failed in Listen.
func Register(reg *carrier.Registry, d *Dialer, l *Listener) error {
	if reg == nil {
		return errors.New("reality2: nil carrier registry")
	}
	if d == nil && l == nil {
		return fmt.Errorf("%w: nothing to register", ErrCard)
	}
	if d != nil {
		reg.RegisterDialer(Suite, d)
	}
	if l != nil {
		reg.RegisterListener(Suite, l)
	}
	return nil
}

// RegisterTCP installs the reality2-tcp dialer and listener in reg. Either may be nil.
func RegisterTCP(reg *carrier.Registry, d *TCPDialer, l *TCPListener) error {
	if reg == nil {
		return errors.New("reality2: nil carrier registry")
	}
	if d == nil && l == nil {
		return fmt.Errorf("%w: nothing to register", ErrCard)
	}
	if d != nil {
		reg.RegisterDialer(objects.SuiteReality2TCP, d)
	}
	if l != nil {
		reg.RegisterListener(objects.SuiteReality2TCP, l)
	}
	return nil
}
