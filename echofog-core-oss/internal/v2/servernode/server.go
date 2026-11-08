package servernode

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// Files of a server profile. The identity file keeps the recovery phrase and the device secrets
// (mode 0600, never logged); the store keeps sessions, cards and passes.
const (
	identityFileName = "server-identity.json"
	configFileName   = "server.json"
	tokenFileName    = "operator.token"
	addrFileName     = "operator.addr"
	fileVersion      = 1
)

type identityFile struct {
	Version     int    `json:"version"`
	Mnemonic    string `json:"mnemonic"`
	DeviceSeed  string `json:"device_seed"`
	HsPrivate   string `json:"hs_private"`
	MaskPrivate string `json:"mask_private"`

	NodeID   string `json:"node_id"`
	DeviceID string `json:"device_id"`

	CreatedAt          int64  `json:"created_at"`
	DeviceListSeq      uint64 `json:"device_list_seq"`
	DeviceListNotAfter uint64 `json:"device_list_not_after"`
}

// writePrivate replaces path with data, whole or not at all: the data goes to a private temporary
// file in the same directory, is flushed, and is renamed over the target. A crash or a reader
// never sees an empty or half-written file (T12.4a acceptance M2).
func writePrivate(path string, data []byte) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Clean(dir)); err == nil { // make the rename durable; not possible everywhere
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	return b, nil
}

// Init creates the profile in dataDir: identity (recovery phrase returned once), store, server.json
// and the operator token. It refuses a directory that already holds a profile.
func Init(dataDir string, cfg Config) (string, error) { return InitLang(dataDir, cfg, "en") }

// InitLang is Init with the comments of server.json in the given language ("ru" or "en").
func InitLang(dataDir string, cfg Config, lang string) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if cfg.Roles == nil {
		cfg.Roles = []string{} // saved as [], not null: "no roles" is a choice
	}
	if _, err := os.Stat(filepath.Join(dataDir, identityFileName)); err == nil {
		return "", fmt.Errorf("%s already holds a server profile", dataDir)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	phrase, err := identity.NewMnemonic(12)
	if err != nil {
		return "", err
	}
	account, err := identity.FromMnemonic(phrase, "")
	if err != nil {
		return "", err
	}
	defer account.Destroy()
	device, err := identity.NewDevice()
	if err != nil {
		return "", err
	}
	defer device.Destroy()

	var seed, hs, mask []byte
	if err := device.WithSeed(func(b []byte) error { seed = append([]byte(nil), b...); return nil }); err != nil {
		return "", err
	}
	if err := device.Handshake().WithPrivate(func(b []byte) error { hs = append([]byte(nil), b...); return nil }); err != nil {
		return "", err
	}
	if err := device.Masking().WithPrivate(func(b []byte) error { mask = append([]byte(nil), b...); return nil }); err != nil {
		return "", err
	}
	now := time.Now()
	nodeID, deviceID := account.NodeID(), device.ID()
	idf := &identityFile{
		Version: fileVersion, Mnemonic: phrase,
		DeviceSeed: hex.EncodeToString(seed), HsPrivate: hex.EncodeToString(hs), MaskPrivate: hex.EncodeToString(mask),
		NodeID: hex.EncodeToString(nodeID[:]), DeviceID: hex.EncodeToString(deviceID[:]),
		CreatedAt: now.Unix(), DeviceListSeq: 1, DeviceListNotAfter: uint64(now.Add(identity.DeviceListLifetime).Unix()),
	}
	// The store first: a profile whose store cannot be created is not a profile.
	st, err := store.Open(store.Options{Dir: store.Path(dataDir), Key: account.StorageKey(), MemCapMB: node.DefaultProfile(node.PlatformServer).MemCapMB})
	if err != nil {
		return "", fmt.Errorf("create the v2 store: %w", err)
	}
	if err := st.Close(); err != nil {
		return "", err
	}
	if err := saveIdentity(dataDir, idf); err != nil {
		return "", err
	}
	raw, err := RenderConfig(cfg, lang)
	if err != nil {
		return "", err
	}
	if err := writePrivate(filepath.Join(dataDir, configFileName), raw); err != nil {
		return "", err
	}
	if _, err := NewTokenFile(filepath.Join(dataDir, tokenFileName)); err != nil {
		return "", err
	}
	return phrase, nil
}

func saveIdentity(dataDir string, f *identityFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dataDir, identityFileName), raw)
}

func loadIdentity(dataDir string) (*identityFile, error) {
	raw, err := readLimited(filepath.Join(dataDir, identityFileName), 64<<10)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is not a server profile: run `echofog-node v2 init` first", dataDir)
		}
		return nil, err
	}
	var f identityFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("identity file: version %d, want %d", f.Version, fileVersion)
	}
	return &f, nil
}

func decodeKey(text, what string) ([]byte, error) {
	b, err := hex.DecodeString(text)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("identity file: %s: want 64 hex characters", what)
	}
	return b, nil
}

func deriveIdentity(f *identityFile) (*identity.Account, *identity.Device, error) {
	account, err := identity.FromMnemonic(f.Mnemonic, "")
	if err != nil {
		return nil, nil, fmt.Errorf("identity: %w", err)
	}
	seed, err1 := decodeKey(f.DeviceSeed, "device seed")
	hs, err2 := decodeKey(f.HsPrivate, "hs key")
	mask, err3 := decodeKey(f.MaskPrivate, "mask key")
	if err := errors.Join(err1, err2, err3); err != nil {
		account.Destroy()
		return nil, nil, err
	}
	device, err := identity.DeviceFromKeys(seed, hs, mask)
	if err != nil {
		account.Destroy()
		return nil, nil, fmt.Errorf("identity: %w", err)
	}
	n, d := account.NodeID(), device.ID()
	if f.NodeID != hex.EncodeToString(n[:]) || f.DeviceID != hex.EncodeToString(d[:]) {
		account.Destroy()
		device.Destroy()
		return nil, nil, errors.New("identity file: ids do not match the keys")
	}
	return account, device, nil
}

func sealDeviceList(account *identity.Account, device *identity.Device, idf *identityFile, now time.Time) ([]byte, bool, error) {
	added := time.Unix(idf.CreatedAt, 0).UTC()
	entry := []identity.DeviceEntry{{Key: device.Public(), Added: added, Primary: true}}
	if idf.DeviceListNotAfter > uint64(now.Add(24*time.Hour).Unix()) {
		raw, err := account.SealDeviceList(idf.DeviceListSeq, idf.DeviceListNotAfter, entry)
		return raw, false, err
	}
	seq := idf.DeviceListSeq + 1
	notAfter := now.Add(identity.DeviceListLifetime)
	raw, err := account.SealDeviceList(seq, uint64(notAfter.Unix()), entry)
	if err != nil {
		return nil, false, err
	}
	idf.DeviceListSeq, idf.DeviceListNotAfter = seq, uint64(notAfter.Unix())
	return raw, true, nil
}

// Server is an open profile with its runtime and operator API.
type Server struct {
	dir     string
	cfg     *Config
	log     *slog.Logger
	account *identity.Account
	device  *identity.Device
	store   *store.Store
	builder *cards.CardBuilder
	access  *access.Manager
	rt      *node.Runtime

	mu       sync.Mutex
	warnings map[string]string
	listener net.Listener
	http     *http.Server
	started  bool
	closed   bool
	startAt  time.Time
}

func rolesOf(names []string) []node.Role {
	var out []node.Role
	for _, n := range names {
		switch n {
		case "relay":
			out = append(out, node.RoleRelay)
		case "exit":
			out = append(out, node.RoleExit)
		case "mailbox":
			out = append(out, node.RoleMailbox)
		case "dht-server":
			out = append(out, node.RoleDHTServer)
		}
	}
	return out
}

func serviceOffers(names []string) []*pb.ServiceOffer {
	ids := map[string]uint64{"relay": service.Relay, "exit": service.Exit, "mailbox": service.Mailbox, "dht-server": service.DHT}
	var out []*pb.ServiceOffer
	for _, n := range names {
		sp, ok := service.Builtin(ids[n])
		if !ok {
			continue
		}
		o := &pb.ServiceOffer{Id: uint32(sp.ID)}
		for v := range sp.Versions {
			o.Versions = append(o.Versions, uint32(v))
		}
		sort.Slice(o.Versions, func(i, j int) bool { return o.Versions[i] < o.Versions[j] })
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

func roleAndFlags(p node.Profile) (uint32, uint32) {
	var flags uint32
	for _, r := range p.Roles {
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
	return objects.RoleServer, flags
}

// freeExit is the exit offer of a server that serves without payment.
type freeExit struct{}

func (freeExit) ExitOffer() *pb.ExitOffer { return &pb.ExitOffer{} }

type cardsKV struct{ kv store.KV }

func (k cardsKV) Get(_ context.Context, key []byte) ([]byte, bool, error) {
	v, err := k.kv.Get(key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	return v, err == nil, err
}

func (k cardsKV) Put(_ context.Context, key, value []byte) error { return k.kv.Put(key, value) }

// coverHost is the host part of the configured cover ("example.org:443" -> "example.org").
func coverHost(cover string) string {
	host, _, err := net.SplitHostPort(cover)
	if err != nil {
		return cover
	}
	return host
}

// ws2Path derives the unguessable ws2 upgrade path from the profile's masking key: it is stable
// across restarts and never appears anywhere but the card and the listener.
func ws2Path(maskPriv []byte) string {
	sum := sha256.Sum256(append(append([]byte(nil), maskPriv...), []byte("echofog/ws2/path")...))
	return "/" + hex.EncodeToString(sum[:16])
}

// suiteShape returns the shape profile a card advertises for a suite.
func suiteShape(suite string) *pb.ShapeProfile {
	if udpSuite(suite) {
		return &pb.ShapeProfile{Id: shape.IDDefaultUDP}
	}
	return &pb.ShapeProfile{Id: shape.IDDefaultTLS}
}

// cardListener builds the card entry of one configured listener. The camouflaged suites carry the
// cover's SNI; ws2 carries its wss URL and no address.
func cardListener(l ListenSpec, mask []byte, cover, path string) (cards.Listener, error) {
	host := coverHost(cover)
	cl := cards.Listener{Suite: suiteNumber(l.Suite), Mask: mask, Shape: suiteShape(l.Suite)}
	if l.Suite == "ws2" {
		if host == "" {
			return cards.Listener{}, errors.New("ws2 needs a cover domain")
		}
		cl.URL = "wss://" + host + path
		cl.SNI = host
		return cl, nil
	}
	_, port, err := net.SplitHostPort(l.Bind)
	if err != nil {
		return cards.Listener{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return cards.Listener{}, err
	}
	adv, err := netip.ParseAddr(l.Advertise)
	if err != nil {
		return cards.Listener{}, err
	}
	cl.Host = adv.Unmap().String()
	cl.Port = uint32(p)
	if l.Suite != "noise3" {
		if host == "" {
			return cards.Listener{}, fmt.Errorf("%s needs a cover domain", l.Suite)
		}
		cl.SNI = host
	}
	return cl, nil
}

func suiteNumber(name string) uint32 {
	switch name {
	case "reality2-quic":
		return objects.SuiteReality2QUIC
	case "reality2-tcp":
		return objects.SuiteReality2TCP
	case "ws2":
		return objects.SuiteWS2
	}
	return objects.SuiteNoise3
}

// loadCoverCert reads the operator's certificate bundle: a PEM chain and its private key in one
// file (the same shape selfcover stores). The doctor watches its expiry.
func loadCoverCert(path string) (*tls.Certificate, error) {
	raw, err := readLimited(path, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("cover_cert: %w", err)
	}
	cert, err := tls.X509KeyPair(raw, raw)
	if err != nil {
		return nil, fmt.Errorf("cover_cert: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("cover_cert: %w", err)
	}
	cert.Leaf = leaf
	return &cert, nil
}

// realityBinds returns the TCP and UDP binds of the reality2 pair that shares bind. Either is
// empty when that transport is not configured.
func realityBinds(listens []ListenSpec, bind string) (tcp, udp string) {
	for _, l := range listens {
		if l.Bind != bind {
			continue
		}
		switch l.Suite {
		case "reality2-tcp":
			tcp = l.Bind
		case "reality2-quic":
			udp = l.Bind
		}
	}
	return tcp, udp
}

// realityMask parses one X25519 masking private key for reality2.
func realityMask(priv []byte) (reality2.MaskKey, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return reality2.MaskKey{}, fmt.Errorf("masking key: %w", err)
	}
	return reality2.MaskKey{Private: k}, nil
}

// tokenAddr is the exact host:port a card advertises for a listener; tokens bind to it.
func tokenAddr(l ListenSpec) string {
	_, port, err := net.SplitHostPort(l.Bind)
	if err != nil {
		return l.Bind
	}
	return net.JoinHostPort(l.Advertise, port)
}

// ws2TokenAddr is the address a ws2 token binds to: the CDN-facing host and 443.
func ws2TokenAddr(cover string) string {
	host := coverHost(cover)
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, "443")
}

// Open opens the profile of dataDir and builds the runtime without starting it.
func Open(ctx context.Context, dataDir string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	raw, err := readLimited(filepath.Join(dataDir, configFileName), MaxConfigSize)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is not a server profile: run `echofog-node v2 init` first", dataDir)
		}
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	idf, err := loadIdentity(dataDir)
	if err != nil {
		return nil, err
	}
	account, device, err := deriveIdentity(idf)
	if err != nil {
		return nil, err
	}
	s := &Server{dir: dataDir, cfg: cfg, log: log, account: account, device: device, warnings: map[string]string{}}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close(context.Background())
		}
	}()

	profile := node.DefaultProfile(node.PlatformServer)
	profile.Roles = rolesOf(cfg.Roles)

	now := time.Now()
	devices, changed, err := sealDeviceList(account, device, idf, now)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := saveIdentity(dataDir, idf); err != nil {
			return nil, err
		}
	}
	st, err := store.Open(store.Options{Dir: store.Path(dataDir), Key: account.StorageKey(), MemCapMB: profile.MemCapMB, Log: log})
	if err != nil {
		return nil, fmt.Errorf("open the v2 store: %w", err)
	}
	s.store = st
	clk, err := clock.New(clock.Config{})
	if err != nil {
		return nil, err
	}

	maskPub := device.Masking().Public()
	maskPriv, err := decodeKey(idf.MaskPrivate, "mask key")
	if err != nil {
		return nil, err
	}
	var coverCert *tls.Certificate
	if cfg.CoverCert != "" {
		if coverCert, err = loadCoverCert(cfg.CoverCert); err != nil {
			return nil, err
		}
	}
	path := ws2Path(maskPriv)
	var cardListeners []cards.Listener
	for _, l := range cfg.Listen {
		cl, err := cardListener(l, maskPub, cfg.Cover, path)
		if err != nil {
			return nil, err
		}
		cardListeners = append(cardListeners, cl)
	}
	role, flags := roleAndFlags(profile)
	var exitOffer cards.ExitSource
	if slices.Contains(cfg.Roles, "exit") {
		exitOffer = freeExit{} // the price and payments are T10.3
	}
	builder, err := cards.New(cards.Config{
		Exit:    exitOffer,
		Account: account.Public(), Device: device, HsKey: device.Handshake().Public(),
		Role: role, Flags: flags, Services: serviceOffers(cfg.Roles), Listeners: cardListeners, Devices: devices,
		Release: "echofog-node/v2", Visibility: cards.Visible, Clock: clk, KV: cardsKV{st.Store("v2/node/cards")},
	})
	if err != nil {
		return nil, fmt.Errorf("card builder: %w", err)
	}
	s.builder = builder

	shapeProfile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		return nil, err
	}
	tlsShape, err := shape.Builtin(shape.IDDefaultTLS)
	if err != nil {
		return nil, err
	}

	var sources []node.ListenerSource
	var local []netip.Addr
	seenReality := map[string]bool{}
	for _, l := range cfg.Listen {
		if a, err := netip.ParseAddr(l.Advertise); err == nil {
			local = append(local, a.Unmap())
		}
		switch l.Suite {
		case "noise3":
			var mk noise3.MaskKey
			copy(mk.Private[:], maskPriv)
			sources = append(sources, node.Noise3Listener{Addr: l.Bind, Masks: []noise3.MaskKey{mk}, Profile: shapeProfile,
				ReplayKV: st.Store("v2/carrier/noise3/replay"), LowPower: profile.LowPower})
		case "reality2-quic", "reality2-tcp":
			if seenReality[l.Bind] {
				continue // the pair shares one cover node and one certificate
			}
			seenReality[l.Bind] = true
			tcpBind, udpBind := realityBinds(cfg.Listen, l.Bind)
			mk, err := realityMask(maskPriv)
			if err != nil {
				return nil, err
			}
			src := node.Reality2Listener{
				Domain: coverHost(cfg.Cover), SiteDir: cfg.SiteDir, Cert: coverCert,
				CertKV: st.Store("v2/reality2/self/cert"), ReplayKV: st.Store("v2/reality2/replay"),
				TCPAddr: tcpBind, UDPAddr: udpBind,
				Masks: []reality2.MaskKey{mk}, Addrs: []string{tokenAddr(l)},
				Shape: tlsShape, LowPower: profile.LowPower,
			}
			if coverCert == nil {
				src.HTTPAddr = ":80" // http-01; tls-alpn-01 works on the cover port itself
			}
			sources = append(sources, src)
		case "ws2":
			mk, err := realityMask(maskPriv)
			if err != nil {
				return nil, err
			}
			sources = append(sources, node.WS2Listener{
				Addr: l.Bind, Path: path, Domain: coverHost(cfg.Cover), SiteDir: cfg.SiteDir,
				Masks: []reality2.MaskKey{mk}, Addrs: []string{ws2TokenAddr(cfg.Cover)},
				ReplayKV: st.Store("v2/ws2/replay"), Shape: tlsShape, LowPower: profile.LowPower,
			})
		}
	}

	ncfg := node.Config{
		Account: account, Device: device, Cards: builder, Store: st, Clock: clk, Log: log,
		Events:           node.EventFunc(s.onEvent),
		Profile:          profile,
		ListenerSources:  sources,
		ExitShapeProfile: shapeProfile,
		LocalInfo:        carrier.LocalInfo{Addrs: local},
		// HTTP stays nil: no CDN clock check from a server until a uTLS client is wired (T11.6).
	}
	if cfg.InviteListen != nil {
		il, err := cardListener(*cfg.InviteListen, nil, cfg.Cover, path)
		if err != nil {
			return nil, err
		}
		mgr, err := access.New(access.Config{KV: st.Store(access.Namespace), Cards: builder,
			ContactsKey: device.Masking(), Listeners: []cards.Listener{il}})
		if err != nil {
			return nil, fmt.Errorf("access manager: %w", err)
		}
		s.access = mgr
		ncfg.Access = mgr
		bind := cfg.InviteListen.Bind
		suite := cfg.InviteListen.Suite
		advertise := tokenAddr(*cfg.InviteListen)
		ncfg.AccessListener = func(slot int, ks []node.AccessMaskKey) (carrier.Listener, error) {
			replayKV := st.Store("v2/reality2/access/" + strconv.Itoa(slot))
			masks := make([]reality2.MaskKey, 0, len(ks))
			for _, k := range ks {
				mk, err := realityMask(k.Private)
				if err != nil {
					return nil, err
				}
				masks = append(masks, mk)
			}
			switch suite {
			case "noise3":
				n3 := make([]noise3.MaskKey, 0, len(ks))
				for _, k := range ks {
					var mk noise3.MaskKey
					copy(mk.Private[:], k.Private)
					mk.NotAfter = k.NotAfter
					n3 = append(n3, mk)
				}
				return node.Noise3Listener{Addr: bind, Masks: n3, Profile: shapeProfile, LowPower: profile.LowPower,
					ReplayKV: st.Store("v2/carrier/noise3/access/" + strconv.Itoa(slot))}.Listen(context.Background())
			case "reality2-quic", "reality2-tcp":
				src := node.Reality2Listener{
					Domain: coverHost(cfg.Cover), SiteDir: cfg.SiteDir, Cert: coverCert,
					CertKV: st.Store("v2/reality2/self/access/" + strconv.Itoa(slot)), ReplayKV: replayKV,
					Masks: masks, Addrs: []string{advertise}, Shape: tlsShape, LowPower: profile.LowPower,
				}
				if suite == "reality2-tcp" {
					src.TCPAddr = bind
				} else {
					src.UDPAddr = bind
				}
				if coverCert == nil {
					src.HTTPAddr = ":80"
				}
				return src.Listen(context.Background())
			case "ws2":
				return node.WS2Listener{Addr: bind, Path: path, Domain: coverHost(cfg.Cover), SiteDir: cfg.SiteDir,
					Masks: masks, Addrs: []string{ws2TokenAddr(cfg.Cover)}, ReplayKV: replayKV,
					Shape: tlsShape, LowPower: profile.LowPower}.Listen(context.Background())
			}
			return nil, fmt.Errorf("%w: invitation suite %q", ErrConfig, suite)
		}
	}
	rt, err := node.New(ncfg)
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	s.rt = rt
	ok = true
	return s, nil
}

// onEvent keeps the operator warnings: the latest event of each kind that needs attention.
func (s *Server) onEvent(e node.Event) {
	var key string
	switch e.Kind {
	case node.EventNoCover:
		key = "no-cover"
	case node.EventClockWarning:
		key = "clock-warning"
	case node.EventMemoryProtection:
		key = "memory-protection"
	default:
		return
	}
	s.mu.Lock()
	s.warnings[key] = key + ": " + e.Message
	s.mu.Unlock()
	s.log.Warn("operator notice", "kind", key)
}

// Start starts the runtime and the operator API.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return errors.New("servernode: already started or closed")
	}
	s.started = true
	s.startAt = time.Now()
	s.mu.Unlock()

	tok, err := NewTokenFile(filepath.Join(s.dir, tokenFileName))
	if err != nil {
		return err
	}
	if err := s.rt.Start(ctx); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.cfg.Operator.Addr)
	if err != nil {
		return fmt.Errorf("operator API: %w", err)
	}
	h := NewOperator(OperatorOptions{Backend: s, Token: tok, Log: s.log})
	srv := &http.Server{
		Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError),
	}
	s.mu.Lock()
	s.listener, s.http = ln, srv
	s.mu.Unlock()
	if err := writePrivate(filepath.Join(s.dir, addrFileName), []byte(ln.Addr().String()+"\n")); err != nil {
		return err
	}
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// OperatorAddr is the address the operator API listens on.
func (s *Server) OperatorAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// PeerCard returns the node's current peer-scope card envelope.
func (s *Server) PeerCard(ctx context.Context) ([]byte, error) {
	ic, err := s.builder.Current(ctx, objects.ScopePeer)
	if err != nil {
		return nil, err
	}
	return ic.Envelope, nil
}

// Close stops everything and wipes the keys.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.http
	s.mu.Unlock()
	var errs []error
	if srv != nil {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		errs = append(errs, srv.Shutdown(c))
		cancel()
		_ = os.Remove(filepath.Join(s.dir, addrFileName))
	}
	if s.rt != nil {
		errs = append(errs, s.rt.Stop(ctx))
	}
	if s.access != nil {
		s.access.Close()
	}
	if s.store != nil {
		errs = append(errs, s.store.Close())
	}
	if s.account != nil {
		s.account.Destroy()
	}
	if s.device != nil {
		s.device.Destroy()
	}
	return errors.Join(errs...)
}

// ---- Backend

// Status implements Backend.
func (s *Server) Status() StatusInfo {
	nid, did := s.account.NodeID(), s.device.ID()
	st := StatusInfo{NodeID: hex.EncodeToString(nid[:]), DeviceID: hex.EncodeToString(did[:]), Roles: s.cfg.Roles, Version: BuildVersion}
	s.mu.Lock()
	if !s.startAt.IsZero() {
		st.UptimeSeconds = int64(time.Since(s.startAt).Seconds())
	}
	s.mu.Unlock()
	if s.rt != nil {
		st.Sessions = len(s.rt.Status().Sessions)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.access != nil {
		if infos, err := s.rt.AccessList(ctx); err == nil {
			st.Invites = len(infos)
		}
	}
	_, perr := s.builder.Current(ctx, objects.ScopePublic)
	st.PublicCard = perr == nil
	if perr == nil {
		// The public link carries the public card only (no access secret, D37): it is what the
		// operator shows on the site and in "v2 status".
		if ic, cerr := s.builder.Current(ctx, objects.ScopePublic); cerr == nil {
			if link, lerr := objects.EncodeLink(ic.Envelope, nil); lerr == nil {
				st.PublicLink = link
			}
		}
	}
	s.mu.Lock()
	for _, w := range s.warnings {
		st.Warnings = append(st.Warnings, w)
	}
	s.mu.Unlock()
	sort.Strings(st.Warnings)
	for _, l := range s.cfg.Listen {
		st.Listen = append(st.Listen, l.Suite+" "+l.Advertise+" ("+l.Bind+")")
	}
	return st
}

// MintInvite implements Backend.
func (s *Server) MintInvite(ctx context.Context, opt access.MintOptions) (*access.Minted, error) {
	if s.access == nil {
		return nil, ErrNoInvites
	}
	return s.rt.MintInvitation(ctx, opt)
}

// ListAccess implements Backend.
func (s *Server) ListAccess(ctx context.Context) ([]access.Info, error) {
	if s.access == nil {
		return nil, ErrNoInvites
	}
	return s.rt.AccessList(ctx)
}

// ExtendAccess implements Backend.
func (s *Server) ExtendAccess(ctx context.Context, id access.PassID, d time.Duration) (access.Info, error) {
	if s.access == nil {
		return access.Info{}, ErrNoInvites
	}
	return s.rt.ExtendAccess(ctx, id, d)
}

// RotateKeys implements Backend.
func (s *Server) RotateKeys(ctx context.Context) (*access.Rotation, error) {
	if s.access == nil {
		return nil, ErrNoInvites
	}
	return s.rt.RotateAccessKeys(ctx)
}
