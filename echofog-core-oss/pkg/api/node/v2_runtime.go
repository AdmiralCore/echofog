package node

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network"
	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/app"
	"github.com/echofog/echofog-core/internal/v2/bootstrap"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/chat"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/convsync"
	"github.com/echofog/echofog-core/internal/v2/files"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/links"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// v2Hooks replace the network edges of the v2 stack. Production leaves them nil; tests put two
// stacks of one process on in-memory carriers.
type v2Hooks struct {
	// Listen returns the carrier listeners the runtime accepts on and the listeners its cards
	// advertise. Nil: the node's own settings decide (v2Settings).
	Listen func(dev *identity.Device) ([]carrier.Listener, []cards.Listener)
	// Dialer wraps the runtime for the link service. Nil: the runtime dials.
	Dialer func(rt *node.Runtime) links.Dialer
	// AccessListener builds the listener of an invitation slot. Nil: the node's own.
	AccessListener node.AccessListenerFactory
	// Roles are the roles of the runtime (a desktop has none); tests give two stacks the DHT server
	// role so they find each other's records without a network.
	Roles []node.Role
	// ChatDir replaces the DHT as the place accounts are looked up (two stacks of one process).
	ChatDir chat.Directory
}

// v2Settings is <profile>/v2/node.json: where this desktop listens, which address it advertises and
// the bootstrap list. All of it is optional: a desktop without a listener is a client behind its
// NAT and is reached through the network (DHT, relays, hole punching).
type v2Settings struct {
	// Listen is the UDP address to bind ("0.0.0.0:4430"); Advertise the host written into the cards.
	Listen    string `json:"listen,omitempty"`
	Advertise string `json:"advertise,omitempty"`
	// Bootstrap is the path of a bootstrap list (T5.5); relative paths are inside the v2 directory.
	// The build carries no addresses of its own.
	Bootstrap string `json:"bootstrap,omitempty"`
}

// clockProxy is the route of the clock's CDN check: the same SOCKS5 proxy or VPN exit the
// app's own lookups use (T11.6a). allowed is false in anonymous mode with no route out: the
// check stays off rather than show the address.
func (n *NodeApi) clockProxy() (proxy *url.URL, allowed bool) {
	p := network.OutboundProxy()
	if p == "" && n.node != nil && n.node.Cfg != nil && n.node.Cfg.IsV2() {
		p = n.node.Cfg.Network.Proxy
	}
	if p != "" {
		return &url.URL{Scheme: "socks5", Host: p}, true
	}
	n.vpn.mu.Lock()
	srv := n.vpn.server
	n.vpn.mu.Unlock()
	if srv != nil {
		if a := srv.Addr(); a != nil {
			return &url.URL{Scheme: "socks5", Host: a.String()}, true
		}
	}
	if n.node != nil && n.node.AnonMode() != "" {
		return nil, false
	}
	return nil, true
}

const (
	v2SettingsFile  = "node.json"
	v2DefaultList   = "bootstrap.txt"
	v2DeviceNS      = "v2/desktop"
	v2DeviceKey     = "device"
	v2DeviceListSeq = "devlist-seq"
	v2AppNS         = "v2/app"
	v2Version       = "0.3.0"
	maxV2Settings   = 64 << 10
)

// v2Stack is the running protocol 2 runtime of this facade.
type v2Stack struct {
	st      *store.Store
	account *identity.Account
	device  *identity.Device
	ids     *store.IDs
	rt      *node.Runtime
	ap      *app.App
	calls   *app.Calls
	mode    app.Mode

	ctx      context.Context
	cancel   context.CancelFunc // stops the goroutines of the stack (the bootstrap, the publication)
	wg       sync.WaitGroup
	ingested atomic.Int32
}

var (
	errV2NotV2    = errors.New("v2 runtime: the profile runs protocol 1")
	errV2NoListen = errors.New("v2 runtime: hidden mode needs a listen address in the v2 settings")
)

// startV2 builds and starts the v2 runtime and attaches it to the facade (protocol 2 only).
func (n *NodeApi) startV2() error {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	return n.startV2Locked()
}

func (n *NodeApi) startV2Locked() error {
	if n.node == nil || n.node.Cfg == nil || !n.node.Cfg.IsV2() {
		return errV2NotV2
	}
	if n.v2s != nil {
		return nil
	}
	s, err := n.buildV2()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(n.node.Ctx)
	s.ctx, s.cancel = ctx, cancel
	if err := s.ap.Start(ctx); err != nil {
		cancel()
		s.close()
		return fmt.Errorf("v2 runtime: %w", err)
	}
	n.v2s = s
	n.AttachV2(s.ap)
	n.AttachV2Messenger(s.ap)
	if s.calls != nil {
		n.AttachV2Calls(s.calls)
		n.AttachV2GroupCalls(s.ap)
	}
	s.runBootstrap(ctx, n.v2Dir(), n.Logger)
	return nil
}

// stopV2 stops the v2 runtime and releases everything it holds.
func (n *NodeApi) stopV2() {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	n.stopV2Locked()
}

func (n *NodeApi) stopV2Locked() {
	s := n.v2s
	if s == nil {
		return
	}
	n.v2s = nil
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = s.ap.Stop(ctx)
	s.wg.Wait()
	s.close()
}

// restartV2 rebuilds the runtime from the saved settings (a new access mode applies this way).
func (n *NodeApi) restartV2() error {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	if n.v2s == nil {
		return nil
	}
	prev := n.v2s.mode
	n.stopV2Locked()
	err := n.startV2Locked()
	if err != nil {
		// the new mode did not start: go back to the old one, so the facade is not left without a network
		n.v2force = prev
		if err2 := n.startV2Locked(); err2 != nil {
			n.Logger.Error("v2 runtime: the old mode did not start either", "err", err2)
		}
		n.v2force = ""
	}
	return err
}

func (s *v2Stack) close() {
	if s.ids != nil {
		s.ids.Close()
		s.ids = nil
	}
	if s.st != nil {
		_ = s.st.Close()
		s.st = nil
	}
	if s.device != nil {
		s.device.Destroy()
		s.device = nil
	}
	if s.account != nil {
		s.account.Destroy()
		s.account = nil
	}
}

// v2App is the application layer of the running v2 runtime, nil when none runs.
func (n *NodeApi) v2App() *app.App {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	if n.v2s == nil {
		return nil
	}
	return n.v2s.ap
}

// V2Running reports whether a v2 runtime is up.
func (n *NodeApi) V2Running() bool {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	return n.v2s != nil
}

// v2Ingested counts the peer cards the app layer took from a knock answer (tests).
func (n *NodeApi) v2Ingested() int {
	n.v2l.Lock()
	defer n.v2l.Unlock()
	if n.v2s == nil {
		return 0
	}
	return int(n.v2s.ingested.Load())
}

func (n *NodeApi) v2Dir() string {
	dir, err := n.node.V2DataDir()
	if err != nil {
		return ""
	}
	return dir
}

// ---- building the stack ------------------------------------------------------------------------------------

func loadV2Settings(dir string) (v2Settings, error) {
	var s v2Settings
	raw, err := os.ReadFile(filepath.Join(dir, v2SettingsFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(raw) > maxV2Settings {
		return s, errors.New("v2 settings: file too large")
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("v2 settings: %w", err)
	}
	return s, nil
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

// deviceKeys loads the device secrets from the sealed store, or makes and keeps them: they are
// random, never derived from the account (K1), and a new install is a new device.
func deviceKeys(kv store.KV) (*identity.Device, error) {
	raw, err := kv.Get([]byte(v2DeviceKey))
	if err == nil && len(raw) == 96 {
		return identity.DeviceFromKeys(raw[:32], raw[32:64], raw[64:])
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	dev, err := identity.NewDevice()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 96)
	err = dev.WithSeed(func(b []byte) error { buf = append(buf, b...); return nil })
	if err == nil {
		err = dev.Handshake().WithPrivate(func(b []byte) error { buf = append(buf, b...); return nil })
	}
	if err == nil {
		err = dev.Masking().WithPrivate(func(b []byte) error { buf = append(buf, b...); return nil })
	}
	if err == nil && len(buf) == 96 {
		err = kv.Put([]byte(v2DeviceKey), buf)
	}
	for i := range buf {
		buf[i] = 0
	}
	if err != nil {
		dev.Destroy()
		return nil, err
	}
	return dev, nil
}

// nextDeviceListSeq returns the sequence of the next sealed device list and keeps it.
func nextDeviceListSeq(kv store.KV) (uint64, error) {
	var seq uint64
	if raw, err := kv.Get([]byte(v2DeviceListSeq)); err == nil {
		seq, _ = strconv.ParseUint(string(raw), 10, 64)
	}
	seq++
	return seq, kv.Put([]byte(v2DeviceListSeq), []byte(strconv.FormatUint(seq, 10)))
}

// rtRef is the runtime, set once node.New returns: the services below are built before it exists and
// reach it through this reference.
type rtRef struct {
	mu sync.RWMutex
	rt *node.Runtime
}

func (r *rtRef) get() *node.Runtime   { r.mu.RLock(); defer r.mu.RUnlock(); return r.rt }
func (r *rtRef) set(rt *node.Runtime) { r.mu.Lock(); r.rt = rt; r.mu.Unlock() }

// lazyChat sends through the chat service of the runtime once it runs (the service exists after Start).
type lazyChat struct{ r *rtRef }

func (l lazyChat) Send(ctx context.Context, to [32]byte, m *chatpb.PersonalMessageProto) ([16]byte, error) {
	rt := l.r.get()
	if rt == nil || rt.Chat() == nil {
		return [16]byte{}, errors.New("chat is not running")
	}
	return rt.Chat().Send(ctx, to, m)
}

// lazyFiles adds shares to the files/2 server of the runtime once it runs.
type lazyFiles struct{ r *rtRef }

var errNoFiles = errors.New("files are not running")

func (l lazyFiles) Add(s files.Share) error {
	if rt := l.r.get(); rt != nil && rt.Files() != nil {
		return rt.Files().Add(s)
	}
	return errNoFiles
}

func (l lazyFiles) Remove(fid [16]byte) {
	if rt := l.r.get(); rt != nil && rt.Files() != nil {
		rt.Files().Remove(fid)
	}
}

// v2Directory finds the account key behind a node ID: the card book first (every session and every
// knock answer puts the device list there), then the DHT.
type v2Directory struct {
	r    *rtRef
	self [32]byte
	pub  [32]byte
}

func accountPub(a []byte) ([32]byte, bool) {
	var pub [32]byte
	if len(a) != 1+len(pub) {
		return pub, false
	}
	copy(pub[:], a[1:])
	return pub, true
}

func (d v2Directory) Pub(ctx context.Context, nd [32]byte) ([32]byte, error) {
	if nd == d.self {
		return d.pub, nil
	}
	rt := d.r.get()
	if rt == nil {
		return [32]byte{}, errors.New("runtime is not running")
	}
	if dl := rt.Cards().Devices(node.NodeID(nd)); dl != nil {
		if pub, ok := accountPub(dl.Account); ok {
			return pub, nil
		}
	}
	if cl := rt.DHT(); cl != nil {
		res, err := cl.Resolve(ctx, nd)
		if err != nil {
			return [32]byte{}, err
		}
		if res.Devices != nil {
			if pub, ok := accountPub(res.Devices.Account); ok {
				return pub, nil
			}
		}
	}
	return [32]byte{}, errors.New("account key is unknown")
}

// v2CallSigner signs call signals with the device key.
type v2CallSigner struct{ dev *identity.Device }

func (c v2CallSigner) CreateSignature(data []byte) ([]byte, error) { return c.dev.Sign(data) }

func (c v2CallSigner) GetMyPubKey() types.PeerPublicKey {
	var k types.PeerPublicKey
	copy(k[:], c.dev.Public()[1:])
	return k
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (n *NodeApi) buildV2() (*v2Stack, error) {
	dir, err := n.node.V2DataDir()
	if err != nil {
		return nil, err
	}
	set, err := loadV2Settings(dir)
	if err != nil {
		return nil, err
	}
	key, chain, err := n.node.V2Master()
	if err != nil {
		return nil, err
	}
	account, err := identity.FromMaster(key, chain, identity.DefaultAccount)
	zero(key)
	zero(chain)
	if err != nil {
		return nil, err
	}
	s := &v2Stack{account: account}
	ok := false
	defer func() {
		if !ok {
			s.close()
		}
	}()

	prof := node.DefaultProfile(node.PlatformDesktop)
	prof.Roles = n.v2h.Roles
	var cardFlags uint32
	for _, r := range prof.Roles {
		switch r {
		case node.RoleRelay:
			cardFlags |= objects.FlagRelay
		case node.RoleDHTServer:
			cardFlags |= objects.FlagDHTServer
		case node.RoleMailbox:
			cardFlags |= objects.FlagMailbox
		}
	}
	s.st, err = store.Open(store.Options{Dir: store.Path(dir), Key: account.StorageKey(), MemCapMB: prof.MemCapMB, Log: n.Logger})
	if err != nil {
		return nil, fmt.Errorf("open the v2 store: %w", err)
	}
	if s.device, err = deviceKeys(s.st.Store(v2DeviceNS)); err != nil {
		return nil, fmt.Errorf("device keys: %w", err)
	}
	seq, err := nextDeviceListSeq(s.st.Store(v2DeviceNS))
	if err != nil {
		return nil, err
	}
	devices, err := account.SealSingleDeviceList(s.device, seq, time.Now())
	if err != nil {
		return nil, fmt.Errorf("device list: %w", err)
	}
	clkCfg := clock.Config{}
	if proxy, allowed := n.clockProxy(); allowed {
		clkCfg = clock.StartupConfig(proxy, ownHTTPTimeout)
	}
	clk, err := clock.New(clkCfg)
	if err != nil {
		return nil, err
	}

	// the mode saved by the last SetMode decides how the cards are built
	appKV := s.st.Store(v2AppNS)
	s.mode = app.ModePublic
	if v, err := appKV.Get([]byte("mode")); err == nil && app.Mode(v) == app.ModeHidden {
		s.mode = app.ModeHidden
	}
	if n.v2force != "" {
		// a restart that failed goes back to the mode that ran before it, and saves it again
		s.mode = n.v2force
		_ = appKV.Put([]byte("mode"), []byte(s.mode))
	}

	// listeners: the hook (tests), else the settings, else none (a client behind its NAT)
	var carriers []carrier.Listener
	var cardLs []cards.Listener
	var sources []node.ListenerSource
	var local []netip.Addr
	shapeProfile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		return nil, err
	}
	var maskPriv []byte
	if err := s.device.Masking().WithPrivate(func(b []byte) error { maskPriv = append([]byte(nil), b...); return nil }); err != nil {
		return nil, err
	}
	defer zero(maskPriv)
	switch {
	case n.v2h.Listen != nil:
		carriers, cardLs = n.v2h.Listen(s.device)
	case set.Listen != "":
		host := strings.TrimSpace(set.Advertise)
		ap, err := netip.ParseAddrPort(set.Listen)
		if err != nil {
			return nil, fmt.Errorf("v2 settings: listen: %w", err)
		}
		if host == "" {
			return nil, errors.New("v2 settings: listen needs advertise (the host the cards name)")
		}
		cardLs = append(cardLs, cards.Listener{Suite: objects.SuiteNoise3, Host: host, Port: uint32(ap.Port()),
			Mask: s.device.Masking().Public(), Shape: &pb.ShapeProfile{Id: shape.IDDefaultUDP}})
		var mk noise3.MaskKey
		copy(mk.Private[:], maskPriv)
		sources = append(sources, node.Noise3Listener{Addr: set.Listen, Masks: []noise3.MaskKey{mk}, Profile: shapeProfile,
			ReplayKV: s.st.Store("v2/carrier/noise3/replay"), LowPower: prof.LowPower})
		if a, err := netip.ParseAddr(host); err == nil {
			local = append(local, a.Unmap())
		}
	}
	visibility := cards.Visible
	if s.mode == app.ModeHidden {
		visibility = cards.Hidden
	}
	builder, err := cards.New(cards.Config{
		Account: account.Public(), Device: s.device, HsKey: s.device.Handshake().Public(),
		Role: uint32(objects.RoleUser), Flags: cardFlags, Listeners: cardLs, Devices: devices,
		Release: "desktop/" + v2Version, Visibility: visibility, Clock: clk, KV: cardsKV{s.st.Store("v2/node/cards")},
	})
	if err != nil {
		return nil, fmt.Errorf("card builder: %w", err)
	}

	ref := &rtRef{}
	var appMu sync.RWMutex
	var ap *app.App
	getApp := func() *app.App { appMu.RLock(); defer appMu.RUnlock(); return ap }
	chatStates := app.NewChatStates()

	ncfg := node.Config{
		Account: account, Device: s.device, Cards: builder, Store: s.st, Clock: clk, Log: n.Logger,
		Profile: prof, Listeners: carriers, ListenerSources: sources, ExitShapeProfile: shapeProfile,
		LocalInfo: carrier.LocalInfo{Addrs: local},
		Chat:      true,
		ChatDir:   n.v2h.ChatDir,
		ChatEvents: chat.Events{
			Message: func(in chat.Incoming) {
				if a := getApp(); a != nil {
					a.OnIncoming(context.Background(), in.From, in.Message)
				}
			},
			Status: func(id [16]byte, st chat.State) { chatStates.OnStatus(id, st) },
			Control: func(from [32]byte, c *chatpb.Control) {
				if a := getApp(); a != nil {
					a.OnControl(from, c)
				}
			},
		},
		Files: true,
		FilesOnDone: func(fid [16]byte, nd [32]byte) {
			if a := getApp(); a != nil {
				a.FileServed(fid, nd)
			}
		},
		MailboxRelays: func() [][32]byte { return mailboxRelays(ref.get()) },
		ChatContacts:  func() [][32]byte { return contactNodes(getApp()) },
	}
	// the access manager keeps the passes of the people who came in by an invitation; it needs the
	// listener the invitations are answered on (the first noise3 one). A hidden node cannot work without it.
	var mgr *access.Manager
	var invite *cards.Listener
	for i := range cardLs {
		if cardLs[i].Suite == objects.SuiteNoise3 {
			invite = &cardLs[i]
			break
		}
	}
	if invite == nil && s.mode == app.ModeHidden {
		return nil, errV2NoListen
	}
	if invite != nil {
		mgr, err = access.New(access.Config{KV: s.st.Store(access.Namespace), Cards: builder,
			ContactsKey: s.device.Masking(), Listeners: []cards.Listener{*invite}})
		if err != nil {
			return nil, fmt.Errorf("access manager: %w", err)
		}
		ncfg.Access = mgr
		if n.v2h.AccessListener != nil {
			ncfg.AccessListener = n.v2h.AccessListener
		} else {
			bind := set.Listen
			ncfg.AccessListener = func(slot int, ks []node.AccessMaskKey) (carrier.Listener, error) {
				masks := make([]noise3.MaskKey, 0, len(ks))
				for _, k := range ks {
					var mk noise3.MaskKey
					copy(mk.Private[:], k.Private)
					mk.NotAfter = k.NotAfter
					masks = append(masks, mk)
				}
				return node.Noise3Listener{Addr: bind, Masks: masks, Profile: shapeProfile, LowPower: prof.LowPower,
					ReplayKV: s.st.Store("v2/carrier/noise3/access/" + strconv.Itoa(slot))}.Listen(context.Background())
			}
		}
	}
	rt, err := node.New(ncfg)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	ref.set(rt)
	s.rt = rt

	var dialer links.Dialer = rt
	if n.v2h.Dialer != nil {
		dialer = n.v2h.Dialer(rt)
	}
	lcfg := links.Config{Cards: builder, Dialer: dialer, Clock: rt.Clock()}
	if mgr != nil {
		lcfg.Inviter = mgr
	}
	svc, err := links.New(lcfg)
	if err != nil {
		return nil, fmt.Errorf("links: %w", err)
	}
	ln := v2Links{Service: svc, r: ref, kick: s.kickPublish}

	// the conversation engine, the groups and the app layer over them
	pub, _ := accountPub(account.Public())
	sign := account.Sign
	if s.ids, err = store.NewIDs(account.StorageKey()); err != nil {
		return nil, err
	}
	book, err := app.NewGroupBook(app.BookConfig{Pub: pub, Sign: sign, Store: s.st.Store("v2/app/groups"), Clock: rt.Clock()})
	if err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	directory := v2Directory{r: ref, self: account.NodeID(), pub: pub}
	eng, err := convsync.New(convsync.Config{
		Pub: pub, Sign: sign, Store: s.st.Store("v2/app/convs"), IDs: s.ids,
		Rules: book.Rules(), Groups: book.Conv, Members: book.Members, SharedGroups: book.SharedGroups,
		Net: chatStates.Wrap(lazyChat{ref}), Dir: directory, Now: rt.Clock().Now,
	})
	if err != nil {
		return nil, fmt.Errorf("conversations: %w", err)
	}
	downloads := n.node.DownloadDir()
	if downloads == "" {
		downloads = filepath.Join(dir, "files")
	}
	a, err := app.New(app.Config{
		Runtime: rt, Links: ln, Store: appKV, Clock: rt.Clock(), Self: account.NodeID(), Mode: s.mode, Version: v2Version,
		Chat: lazyChat{ref},
		OwnCard: func() ([]byte, bool) {
			ic, err := builder.Current(context.Background(), objects.ScopePeer)
			if err != nil {
				return nil, false
			}
			return ic.Envelope, true
		},
		IngestCard: func(_ [32]byte, envelope []byte) {
			if rt.IngestPeerCard(envelope) == nil {
				s.ingested.Add(1)
			}
		},
		Convs: eng, Groups: book, States: chatStates, Prefs: convsync.NewPrefs(s.st.Store("v2/app/prefs")),
		PubOf: directory.Pub,
		Files: lazyFiles{ref},
		FileNet: app.FetchOver(func(ctx context.Context, nd [32]byte) (*mux.Stream, error) {
			return rt.OpenFiles(ctx, nd)
		}),
		FileDir: downloads,
	})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	appMu.Lock()
	ap = a
	appMu.Unlock()
	s.ap = a

	if c, err := app.NewCalls(app.CallsConfig{Host: rt, Signer: v2CallSigner{s.device}, Self: types.PeerID(s.device.ID())}); err == nil {
		s.calls = c
	} else if n.Logger != nil {
		n.Logger.Warn("v2 calls are off", "err", err)
	}
	ok = true
	return s, nil
}

// mailboxRelays lists the connected relays that keep queues (the mailbox role).
func mailboxRelays(rt *node.Runtime) [][32]byte {
	if rt == nil {
		return nil
	}
	var out [][32]byte
	for _, c := range rt.RelayCandidates() {
		if c.Connected && c.Flags&objects.FlagMailbox != 0 {
			out = append(out, [32]byte(c.Device))
		}
	}
	return out
}

// contactNodes lists the contacts' node IDs (the accounts told about a new queue).
func contactNodes(a *app.App) [][32]byte {
	if a == nil {
		return nil
	}
	cs, err := a.Contacts()
	if err != nil {
		return nil
	}
	var out [][32]byte
	for _, c := range cs {
		if b, err := hex.DecodeString(c.Node); err == nil && len(b) == 32 {
			out = append(out, [32]byte(b))
		}
	}
	return out
}

// runBootstrap dials the bootstrap list of the profile, if it has one, once and in the background.
func (s *v2Stack) runBootstrap(ctx context.Context, dir string, log *slog.Logger) {
	if dir == "" {
		return
	}
	set, _ := loadV2Settings(dir)
	path := set.Bootstrap
	if path == "" {
		path = v2DefaultList
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > bootstrap.MaxListBytes {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	entries, err := bootstrap.Parse(string(raw), time.Now())
	if err != nil || len(entries) == 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rep := bootstrap.New(s.rt, entries, s.rt.Clock().Now).Run(ctx)
		if log != nil {
			log.Info("v2 bootstrap finished", "entries", len(entries), "joined", rep.Joined, "failed", rep.Failed)
		}
	}()
}

// v2CheckMode refuses a mode the runtime cannot run in before it is saved: a hidden node needs the
// address its invitations are answered on.
func (n *NodeApi) v2CheckMode(m app.Mode) error {
	if m != app.ModeHidden || n.v2h.Listen != nil || !n.V2Running() {
		return nil
	}
	set, err := loadV2Settings(n.v2Dir())
	if err != nil || set.Listen == "" {
		return &v2UserError{msg: "Скрытый режим требует адреса для приглашений в настройках сети"}
	}
	return nil
}

// v2Links keeps the card of a link in the card book once the session with its owner is up: the
// account key behind a contact (and the device list it needs) is known from then on, without a
// lookup in the DHT.
type v2Links struct {
	*links.Service
	r    *rtRef
	kick func()
}

func (l v2Links) Redeem(ctx context.Context, link string) (node.Peer, error) {
	peer, err := l.Service.Redeem(ctx, link)
	if err != nil {
		return peer, err
	}
	if rt := l.r.get(); rt != nil {
		if pv, e := l.Service.Open(link); e == nil && pv.Card != nil && pv.Card.Env != nil {
			_ = rt.IngestPeerCard(pv.Card.Env.Raw)
		}
	}
	// the routing table has a peer now: the own records can be stored without waiting for the next minute
	if l.kick != nil {
		l.kick()
	}
	return peer, nil
}

// kickPublish stores the own DHT records now (a new peer is in the routing table).
func (s *v2Stack) kickPublish() {
	rt, ctx := s.rt, s.ctx
	if rt == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	cl := rt.DHT()
	if cl == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		cl.PublishNow()
		cl.Step(ctx)
	}()
}
