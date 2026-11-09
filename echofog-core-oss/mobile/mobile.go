// Package mobile is the thin gomobile binding of the EchoFog v2 core (platforms.md §1.1). It is
// the only package a mobile app imports and exposes only types gomobile can bind: strings,
// numbers and a callback interface.
//
// This is the canary of T12.6: Start, Stop, Version, the Protect and LocalAddrs callbacks and a
// Dial that proves a reality2-quic session works inside a library that has no main package. The
// full API (events, domain commands, SecretStore, NetworkChanged, Background) is T12.1.
//
// The package reads no environment variables, no fixed paths and no current directory: the app
// passes the data directory and every flag in the configuration JSON.
package mobile

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// version is the canary binding version.
const version = "v2-canary-1"

// Version returns the version of the binding.
func Version() string { return version }

// Callbacks are implemented by the app (platforms.md §1.1, §2).
type Callbacks interface {
	// Protect is called with every peer socket descriptor before use, so the core's traffic
	// bypasses the app's own VpnService. A non-nil error refuses the socket.
	Protect(fd int) error
	// LocalAddrs returns the device's addresses, one IP literal per line. It replaces
	// net.Interfaces, which Android 11+ blocks.
	LocalAddrs() string
}

// config is the JSON the app passes to Start.
type config struct {
	// DataDir is the writable directory for the store. Required.
	DataDir string `json:"data_dir"`
	// Mnemonic is the BIP-39 recovery phrase; empty creates a throwaway account.
	Mnemonic string `json:"mnemonic"`
	// Platform is android (default) or ios.
	Platform string `json:"platform"`
	// LowPower, Metered and MemCapMB override the profile (platforms.md §2).
	LowPower bool `json:"low_power"`
	Metered  bool `json:"metered"`
	MemCapMB int  `json:"mem_cap_mb"`
}

// instance is one running node.
type instance struct {
	rt      *node.Runtime
	store   *store.Store
	account *identity.Account
	device  *identity.Device
	clock   *clock.Clock
	cards   *cards.CardBuilder
}

// countryOf is the GeoIP lookup of the carrier plan. The canary has no database yet (T2.7 data
// reaches the binding with T12.1), so every address counts as across a border, which keeps
// noise3 off; tests that need it replace the function.
var countryOf = func(netip.Addr) (string, bool) { return "", false }

var (
	mu  sync.Mutex
	cur *instance
)

// Start parses configJSON and starts the node. Only one node runs at a time.
func Start(configJSON string, cb Callbacks) error {
	mu.Lock()
	defer mu.Unlock()
	if cur != nil {
		return errors.New("mobile: already started")
	}
	var cfg config
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return fmt.Errorf("mobile: config: %w", err)
	}
	inst, err := open(cfg, cb, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.rt.Start(ctx); err != nil {
		inst.close()
		return fmt.Errorf("mobile: start: %w", err)
	}
	cur = inst
	return nil
}

// Stop stops the node and closes the store. It is safe to call when nothing runs.
func Stop() error {
	mu.Lock()
	defer mu.Unlock()
	if cur == nil {
		return nil
	}
	inst := cur
	cur = nil
	return inst.stop()
}

func (i *instance) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := i.rt.Stop(ctx)
	i.close()
	return err
}

// Dial opens a session to the node named by a card and returns the result as JSON. cardJSON is
// {"envelope": "<base64 signed card>"}. It is the canary's proof that the carrier works inside
// the library.
func Dial(cardJSON string) (string, error) {
	mu.Lock()
	inst := cur
	mu.Unlock()
	if inst == nil {
		return "", errors.New("mobile: not started")
	}
	return inst.dial(cardJSON)
}

func (i *instance) dial(cardJSON string) (string, error) {
	var doc struct {
		Envelope string `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(cardJSON), &doc); err != nil {
		return "", fmt.Errorf("mobile: card: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(doc.Envelope)
	if err != nil {
		return "", fmt.Errorf("mobile: card envelope: %w", err)
	}
	card, err := objects.VerifyCard(raw, i.clock.Now())
	if err != nil {
		return "", fmt.Errorf("mobile: card: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer, sess, err := i.rt.Session(ctx, node.DeviceID(card.DeviceID), node.DialOpt{Card: card})
	if err != nil {
		return "", err
	}
	rtt, err := sess.Ping(ctx)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]any{
		"peer_device_id": hex.EncodeToString(peer.Device[:]),
		"rtt_ms":         float64(rtt) / float64(time.Millisecond),
	})
	return string(b), err
}

// hostListeners supplies the listeners of a node once its device keys exist. It is for the host
// side of tests; Start passes nil (a mobile node runs no public listener).
type hostListeners func(*identity.Device) ([]cards.Listener, []node.ListenerSource, error)

// open builds a node without starting it.
func open(cfg config, cb Callbacks, host hostListeners) (inst *instance, err error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("mobile: data_dir is required")
	}
	platform := node.Platform(cfg.Platform)
	switch platform {
	case "":
		platform = node.PlatformAndroid
	case node.PlatformAndroid, node.PlatformIOS, node.PlatformServer, node.PlatformDesktop:
	default:
		return nil, fmt.Errorf("mobile: unknown platform %q", cfg.Platform)
	}
	prof := node.DefaultProfile(platform)
	prof.LowPower, prof.Metered = cfg.LowPower, cfg.Metered
	if cfg.MemCapMB > 0 {
		prof.MemCapMB = cfg.MemCapMB
	}
	var local []netip.Addr
	if cb != nil {
		prof.Protect = func(fd uintptr) error { return cb.Protect(int(fd)) }
		prof.LocalAddrs = func() []netip.Addr { return parseAddrs(cb.LocalAddrs()) }
		local = prof.LocalAddrs()
	}

	inst = &instance{}
	defer func() {
		if err != nil {
			inst.close()
			inst = nil
		}
	}()

	phrase := cfg.Mnemonic
	if phrase == "" {
		if phrase, err = identity.NewMnemonic(12); err != nil {
			return nil, err
		}
	}
	if inst.account, err = identity.FromMnemonic(phrase, ""); err != nil {
		return nil, fmt.Errorf("mobile: identity: %w", err)
	}
	if inst.device, err = identity.NewDevice(); err != nil {
		return nil, fmt.Errorf("mobile: device: %w", err)
	}
	var listeners []cards.Listener
	var sources []node.ListenerSource
	if host != nil {
		if listeners, sources, err = host(inst.device); err != nil {
			return nil, err
		}
	}
	devices, err := inst.account.SealSingleDeviceList(inst.device, 1, time.Now())
	if err != nil {
		return nil, fmt.Errorf("mobile: device list: %w", err)
	}
	if inst.store, err = store.Open(store.Options{
		Dir:      store.Path(cfg.DataDir),
		Key:      inst.account.StorageKey(),
		MemCapMB: prof.MemCapMB,
	}); err != nil {
		return nil, fmt.Errorf("mobile: store: %w", err)
	}
	if inst.clock, err = clock.New(clock.StartupConfig(nil, 30*time.Second)); err != nil {
		return nil, err
	}
	role := uint32(objects.RoleUser)
	if platform == node.PlatformServer {
		role = objects.RoleServer
	}
	var flags uint32
	for _, r := range prof.Roles {
		switch r {
		case node.RoleRelay:
			flags |= objects.FlagRelay
		case node.RoleDHTServer:
			flags |= objects.FlagDHTServer
		case node.RoleMailbox:
			flags |= objects.FlagMailbox
		case node.RoleExit:
			flags |= objects.FlagExit
		}
	}
	builder, err := cards.New(cards.Config{
		Account:    inst.account.Public(),
		Device:     inst.device,
		HsKey:      inst.device.Handshake().Public(),
		Role:       role,
		Flags:      flags,
		Listeners:  listeners,
		Devices:    devices,
		Release:    "mobile/" + version,
		Visibility: cards.Visible,
		Clock:      inst.clock,
		KV:         cardsKV{inst.store.Store("v2/node/cards")},
	})
	if err != nil {
		return nil, fmt.Errorf("mobile: card builder: %w", err)
	}
	inst.cards = builder
	if inst.rt, err = node.New(node.Config{
		Account:         inst.account,
		Device:          inst.device,
		Cards:           builder,
		Store:           inst.store,
		Clock:           inst.clock,
		Profile:         prof,
		ListenerSources: sources,
		LocalInfo:       carrier.LocalInfo{Addrs: local},
		Country:         countryOf,
	}); err != nil {
		return nil, fmt.Errorf("mobile: node: %w", err)
	}
	return inst, nil
}

func (i *instance) close() {
	if i == nil {
		return
	}
	if i.store != nil {
		_ = i.store.Close()
		i.store = nil
	}
	if i.account != nil {
		i.account.Destroy()
		i.account = nil
	}
	if i.device != nil {
		i.device.Destroy()
		i.device = nil
	}
}

func parseAddrs(text string) []netip.Addr {
	var out []netip.Addr
	for _, line := range strings.Split(text, "\n") {
		if a, err := netip.ParseAddr(strings.TrimSpace(line)); err == nil {
			out = append(out, a)
		}
	}
	return out
}

// cardsKV adapts a store namespace to the card builder's context-aware KV.
type cardsKV struct{ kv store.KV }

func (k cardsKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	v, err := k.kv.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

func (k cardsKV) Put(_ context.Context, key, value []byte) error { return k.kv.Put(key, value) }
