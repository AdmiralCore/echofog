// Package tuntest has fake drivers for the tun package: a firewall whose rules outlive the
// "process" that installed them (as persistent WFP filters do), a tunnel that can be killed and a
// DNS guard that records what it was asked. Real WFP and TUN are not available to tests.
package tuntest

import (
	"context"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/tun"
)

// Journal records driver calls in order, across fakes.
type Journal struct {
	mu sync.Mutex
	l  []string
}

// Add appends an entry; a nil Journal ignores it.
func (j *Journal) Add(s string) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.l = append(j.l, s)
	j.mu.Unlock()
}

// Entries returns what was recorded.
func (j *Journal) Entries() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.l...)
}

// Kernel is the firewall state of the fake computer. It survives a new Firewall handle, like the
// BFE survives a process.
type Kernel struct {
	mu    sync.Mutex
	rules []tun.Rule
}

// Rules returns the installed rules.
func (k *Kernel) Rules() []tun.Rule {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]tun.Rule(nil), k.rules...)
}

// Firewall is one process's handle to the Kernel.
type Firewall struct {
	K *Kernel
	// FailInstall and FailRemove inject errors; Unprivileged makes Install return ErrNoPrivilege.
	FailInstall, FailRemove error
	Unprivileged            bool
	Installs, Removes       int
	J                       *Journal
}

func (f *Firewall) Install(_ context.Context, rules []tun.Rule) error {
	f.K.mu.Lock()
	defer f.K.mu.Unlock()
	f.Installs++
	f.J.Add("fw.install")
	if f.Unprivileged {
		return tun.ErrNoPrivilege
	}
	if f.FailInstall != nil {
		return f.FailInstall
	}
	f.K.rules = append([]tun.Rule(nil), rules...)
	return nil
}

func (f *Firewall) Remove(context.Context) error {
	f.K.mu.Lock()
	defer f.K.mu.Unlock()
	f.Removes++
	f.J.Add("fw.remove")
	if f.FailRemove != nil {
		return f.FailRemove
	}
	f.K.rules = nil
	return nil
}

func (f *Firewall) Installed(context.Context) (bool, error) {
	f.K.mu.Lock()
	defer f.K.mu.Unlock()
	return len(f.K.rules) > 0, nil
}

// Tunnel is a fake TUN device with a helper that can be killed.
type Tunnel struct {
	mu       sync.Mutex
	up       bool
	events   chan tun.TunEvent
	FailUp   error
	UpCalls  int
	Downs    int
	LastConf tun.TunConfig
	J        *Journal
}

// NewTunnel returns a tunnel that is down.
func NewTunnel() *Tunnel { return &Tunnel{events: make(chan tun.TunEvent, 4)} }

func (t *Tunnel) Up(_ context.Context, cfg tun.TunConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.UpCalls++
	t.J.Add("tun.up")
	if t.FailUp != nil {
		return t.FailUp
	}
	t.up, t.LastConf = true, cfg
	return nil
}

func (t *Tunnel) Down(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Downs++
	t.J.Add("tun.down")
	t.up = false
	return nil
}

func (t *Tunnel) Events() <-chan tun.TunEvent { return t.events }

// IsUp reports whether the device is up.
func (t *Tunnel) IsUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.up
}

// Kill is the helper being killed or the adapter removed: the device is gone and an event says so.
func (t *Tunnel) Kill(err error) {
	t.mu.Lock()
	t.up = false
	t.mu.Unlock()
	select {
	case t.events <- tun.TunEvent{Err: err}:
	default: // nobody is listening: a lost event, as when the watcher is gone
	}
}

// DNS records the protection calls in order.
type DNS struct {
	mu                 sync.Mutex
	Protected          bool
	FailProtect        error
	Protects, Restores int
	Last               tun.DNSConfig
	J                  *Journal
}

func (d *DNS) Protect(_ context.Context, cfg tun.DNSConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Protects++
	d.J.Add("dns.protect")
	if d.FailProtect != nil {
		return d.FailProtect
	}
	d.Protected, d.Last = true, cfg
	return nil
}

func (d *DNS) Restore(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Restores++
	d.J.Add("dns.restore")
	d.Protected = false
	return nil
}

// IsProtected reports whether DNS is pinned.
func (d *DNS) IsProtected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.Protected
}
