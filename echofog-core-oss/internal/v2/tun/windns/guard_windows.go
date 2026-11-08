//go:build windows

// Package windns is the DNS guard of the Windows kill switch (T10.6b): while the mode runs, names
// are resolved through the tunnel and nowhere else.
package windns

import (
	"context"
	"errors"

	"github.com/echofog/echofog-core/internal/v2/tun"
	"golang.org/x/sys/windows/registry"
)

// Guard is the tun.DNSGuard of Windows.
type Guard struct {
	root      registry.Key
	policy    string // the DNS client policy key
	state     string // where the original values are kept until Restore
	runPS     func(ctx context.Context, script string) error
	flush     func()
	nrptLabel string
}

// New returns the guard over the real registry (HKLM), PowerShell and ipconfig. It needs
// administrator rights.
func New() *Guard { return &Guard{} }

func (g *Guard) Protect(ctx context.Context, cfg tun.DNSConfig) error {
	return errors.New("not implemented")
}

func (g *Guard) Restore(ctx context.Context) error { return errors.New("not implemented") }

var _ tun.DNSGuard = (*Guard)(nil)
