package main

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/carrier/reality2"
	"github.com/echofog/echofog-core/internal/v2/clock"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/node"
	"github.com/echofog/echofog-core/internal/v2/node/cards"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"github.com/echofog/echofog-core/internal/v2/shape"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// app is one open profile: identity, store, card builder and (after Start) the runtime.
type app struct {
	data    string
	idf     *identityFile
	nf      *nodeFile
	profile node.Profile
	account *identity.Account
	device  *identity.Device
	store   *store.Store
	clock   *clock.Clock
	builder *cards.CardBuilder
	rt      *node.Runtime
	log     *slog.Logger
}

// openApp opens a profile and builds the runtime, without starting it. The caller starts the
// runtime and closes the app. The listener specs are the ones `run` was given; `card` passes the
// persisted ones when the node itself is not running.
func openApp(ctx context.Context, dataDir string, listeners []listenSpec, log *slog.Logger) (*app, error) {
	idf, err := loadIdentityFile(dataDir)
	if err != nil {
		return nil, err
	}
	nf, err := loadNodeFile(dataDir)
	if err != nil {
		return nil, err
	}
	platform, err := platformOf(nf.Profile)
	if err != nil {
		return nil, err
	}
	account, device, err := deriveIdentity(idf)
	if err != nil {
		return nil, err
	}
	a := &app{data: dataDir, idf: idf, nf: nf, profile: node.DefaultProfile(platform), account: account, device: device, log: log}
	cleanup := true
	defer func() {
		if cleanup {
			a.Close(context.Background())
		}
	}()

	now := time.Now()
	devices, changed, err := sealDeviceList(account, device, idf, now)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := saveIdentityFile(dataDir, idf); err != nil {
			return nil, err
		}
	}

	st, err := store.Open(store.Options{
		Dir:      store.Path(dataDir),
		Key:      account.StorageKey(),
		MemCapMB: a.profile.MemCapMB,
		Log:      log,
	})
	if err != nil {
		return nil, fmt.Errorf("open the v2 store: %w", err)
	}
	a.store = st

	clk, err := clock.New(clock.StartupConfig(nil, 30*time.Second))
	if err != nil {
		return nil, err
	}
	a.clock = clk

	maskPub := device.Masking().Public()
	maskPriv, err := decode32(idf.MaskPrivate, "mask key")
	if err != nil {
		return nil, err
	}
	coverHost := efv2CoverHost(nf.Cover)
	wsPath := efv2WS2Path(maskPriv[:])
	needsCover := false
	cardListeners := make([]cards.Listener, 0, len(listeners))
	for _, l := range listeners {
		cl := cards.Listener{Suite: l.Suite, Mask: maskPub}
		switch l.Suite {
		case objects.SuiteNoise3:
			cl.Host, cl.Port = l.AdvertiseHost, l.Port
			cl.Shape = &pb.ShapeProfile{Id: shape.IDDefaultUDP}
		case objects.SuiteReality2QUIC, objects.SuiteReality2TCP:
			needsCover = true
			cl.Host, cl.Port, cl.SNI = l.AdvertiseHost, l.Port, coverHost
			cl.Shape = &pb.ShapeProfile{Id: shape.IDDefaultTLS}
		case objects.SuiteWS2:
			needsCover = true
			cl.URL = "wss://" + coverHost + wsPath
			cl.SNI = coverHost
			cl.Shape = &pb.ShapeProfile{Id: shape.IDDefaultTLS}
		default:
			return nil, fmt.Errorf("suite %s is not available in this build", suiteName(l.Suite))
		}
		cardListeners = append(cardListeners, cl)
	}
	if needsCover && (nf.Cover == "" || nf.SiteDir == "") {
		return nil, fmt.Errorf("--cover and --site-dir are required for reality2 and ws2 listeners")
	}
	var coverCert *tls.Certificate
	if nf.CoverCert != "" {
		if coverCert, err = loadEFv2Cert(nf.CoverCert); err != nil {
			return nil, err
		}
	}

	role, flags := roleAndFlags(a.profile)
	builder, err := cards.New(cards.Config{
		Account:    account.Public(),
		Device:     device,
		HsKey:      device.Handshake().Public(),
		Role:       role,
		Flags:      flags,
		Services:   echoOffer(),
		Listeners:  cardListeners,
		Devices:    devices,
		Release:    "efv2/T0.3",
		Visibility: cards.Visible,
		Clock:      clk,
		KV:         cardsKV{st.Store("v2/node/cards")},
	})
	if err != nil {
		return nil, fmt.Errorf("card builder: %w", err)
	}
	a.builder = builder

	var sources []node.ListenerSource
	shapeProfile, err := shape.Builtin(shape.IDDefaultUDP)
	if err != nil {
		return nil, err
	}
	tlsShape, err := shape.Builtin(shape.IDDefaultTLS)
	if err != nil {
		return nil, err
	}
	maskPriv, err = decode32(idf.MaskPrivate, "mask key")
	if err != nil {
		return nil, err
	}
	localAddrs := make([]netip.Addr, 0, len(listeners))
	seenReality := map[string]bool{}
	for _, l := range listeners {
		if ip, err := netip.ParseAddr(l.AdvertiseHost); err == nil {
			localAddrs = append(localAddrs, ip)
		}
		switch l.Suite {
		case objects.SuiteNoise3:
			sources = append(sources, node.Noise3Listener{
				Addr:     l.Bind,
				Masks:    []noise3.MaskKey{{Private: maskPriv}},
				Profile:  shapeProfile,
				ReplayKV: st.Store("v2/carrier/noise3/replay"),
				LowPower: a.profile.LowPower,
			})
		case objects.SuiteReality2QUIC, objects.SuiteReality2TCP:
			if seenReality[l.Bind] {
				continue
			}
			seenReality[l.Bind] = true
			mk, err := realityMask(maskPriv[:])
			if err != nil {
				return nil, err
			}
			cert := coverCert
			if cert == nil {
				if cert, err = node.SelfSignedCertificate(coverHost); err != nil {
					return nil, err
				}
			}
			tcpBind, udpBind := realityBinds(listeners, l.Bind)
			sources = append(sources, node.Reality2Listener{
				Domain: coverHost, SiteDir: nf.SiteDir, Cert: cert,
				CertKV: st.Store("v2/reality2/self/cert"), ReplayKV: st.Store("v2/reality2/replay"),
				TCPAddr: tcpBind, UDPAddr: udpBind,
				Masks: []reality2.MaskKey{mk}, Addrs: []string{efv2TokenAddr(l)},
				Shape: tlsShape, LowPower: a.profile.LowPower,
			})
		case objects.SuiteWS2:
			mk, err := realityMask(maskPriv[:])
			if err != nil {
				return nil, err
			}
			sources = append(sources, node.WS2Listener{
				Addr: l.Bind, Path: wsPath, Domain: coverHost, SiteDir: nf.SiteDir,
				Masks: []reality2.MaskKey{mk}, Addrs: []string{efv2WS2TokenAddr(nf.Cover)},
				ReplayKV: st.Store("v2/ws2/replay"), Shape: tlsShape, LowPower: a.profile.LowPower,
			})
		}
	}
	if len(localAddrs) == 0 {
		localAddrs = append(localAddrs, netip.MustParseAddr("127.0.0.1"))
	}

	rt, err := node.New(node.Config{
		Account:          account,
		Device:           device,
		Cards:            builder,
		Store:            st,
		Clock:            clk,
		Log:              log,
		Profile:          a.profile,
		ListenerSources:  sources,
		ExitShapeProfile: shapeProfile,
		LocalInfo:        carrier.LocalInfo{Addrs: localAddrs},
		Country:          labCountry,
	})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	a.rt = rt
	registerEcho(rt)
	cleanup = false
	return a, nil
}

func (a *app) Close(ctx context.Context) {
	if a == nil {
		return
	}
	if a.rt != nil {
		_ = a.rt.Stop(ctx)
		a.rt = nil
	}
	if a.store != nil {
		_ = a.store.Close()
		a.store = nil
	}
	if a.account != nil {
		a.account.Destroy()
		a.account = nil
	}
	if a.device != nil {
		a.device.Destroy()
		a.device = nil
	}
}

// WriteCards writes the public (when the node has one) and peer cards next to the store, so
// `card` can read them while `run` holds the database lock. The JSON is the same format the
// `card` subcommand prints.
func (a *app) WriteCards(ctx context.Context) error {
	dir := filepath.Join(a.data, cardsDirName)
	for _, scope := range []objects.Scope{objects.ScopePublic, objects.ScopePeer} {
		ic, err := a.builder.Current(ctx, scope)
		if errors.Is(err, cards.ErrNoPublicCard) {
			_ = removeFile(filepath.Join(dir, scope.String()+".json"))
			continue
		}
		if err != nil {
			return err
		}
		c, err := objects.VerifyCard(ic.Envelope, a.clock.Now())
		if err != nil {
			return fmt.Errorf("verify own %s card: %w", scope, err)
		}
		if err := writeCardFile(filepath.Join(dir, scope.String()+".json"), newCardDoc(ic, c)); err != nil {
			return err
		}
	}
	return nil
}

// CurrentCard returns the current card of a scope and its JSON form.
func (a *app) CurrentCard(ctx context.Context, scope objects.Scope) (*cards.IssuedCard, *objects.Card, error) {
	ic, err := a.builder.Current(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	c, err := objects.VerifyCard(ic.Envelope, a.clock.Now())
	if err != nil {
		return nil, nil, err
	}
	return ic, c, nil
}

// sessionStatus returns the status entry of one device, if the session is live.
func (a *app) sessionStatus(device node.DeviceID) (node.SessionStatus, bool) {
	for _, s := range a.rt.Status().Sessions {
		if s.Device == device {
			return s, true
		}
	}
	return node.SessionStatus{}, false
}

// labCountry treats every address as one country. It is lab-only: the real D21 plan uses the
// offline GeoIP database (T2.7) and an unknown country counts as a border.
func labCountry(netip.Addr) (string, bool) { return "lab", true }

func roleAndFlags(p node.Profile) (uint32, uint32) {
	role := uint32(objects.RoleUser)
	if p.Platform == node.PlatformServer {
		role = objects.RoleServer
	}
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
	return role, flags
}

func sealDeviceList(account *identity.Account, device *identity.Device, idf *identityFile, now time.Time) ([]byte, bool, error) {
	added := now
	if idf.CreatedAt > 0 {
		added = time.Unix(idf.CreatedAt, 0).UTC()
	}
	if idf.DeviceListNotAfter > uint64(now.Add(24*time.Hour).Unix()) {
		raw, err := account.SealDeviceList(idf.DeviceListSeq, idf.DeviceListNotAfter, []identity.DeviceEntry{{
			Key: device.Public(), Added: added, Primary: true,
		}})
		return raw, false, err
	}
	seq := idf.DeviceListSeq + 1
	if seq == 0 {
		seq = 1
	}
	notAfter := now.Add(identity.DeviceListLifetime)
	raw, err := account.SealDeviceList(seq, uint64(notAfter.Unix()), []identity.DeviceEntry{{
		Key: device.Public(), Added: added, Primary: true,
	}})
	if err != nil {
		return nil, false, err
	}
	idf.DeviceListSeq = seq
	idf.DeviceListNotAfter = uint64(notAfter.Unix())
	return raw, true, nil
}

func decode32(text, what string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(text)
	if err != nil || len(b) != len(out) {
		return out, fmt.Errorf("%s: want 64 hex characters", what)
	}
	copy(out[:], b)
	return out, nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func echoOffer() []*pb.ServiceOffer {
	return []*pb.ServiceOffer{{Id: uint32(echoSpec.ID), Versions: []uint32{uint32(echoVersion)}}}
}

// cardsKV adapts the store namespace to the card builder's context-aware KV.
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

// efv2CoverHost is the host part of the lab cover ("lab.example:443" -> "lab.example").
func efv2CoverHost(cover string) string {
	if host, _, err := net.SplitHostPort(cover); err == nil {
		return host
	}
	return cover
}

// efv2WS2Path derives the unguessable ws2 path from the device masking key, the same rule the
// server node uses.
func efv2WS2Path(priv []byte) string {
	sum := sha256.Sum256(append(append([]byte(nil), priv...), []byte("echofog/ws2/path")...))
	return "/" + hex.EncodeToString(sum[:16])
}

func realityMask(priv []byte) (reality2.MaskKey, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return reality2.MaskKey{}, fmt.Errorf("masking key: %w", err)
	}
	return reality2.MaskKey{Private: k}, nil
}

func realityBinds(listeners []listenSpec, bind string) (tcp, udp string) {
	for _, l := range listeners {
		if l.Bind != bind {
			continue
		}
		switch l.Suite {
		case objects.SuiteReality2TCP:
			tcp = l.Bind
		case objects.SuiteReality2QUIC:
			udp = l.Bind
		}
	}
	return tcp, udp
}

func efv2TokenAddr(l listenSpec) string {
	return net.JoinHostPort(l.AdvertiseHost, strconv.Itoa(int(l.Port)))
}

func efv2WS2TokenAddr(cover string) string {
	host := efv2CoverHost(cover)
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, "443")
}

func loadEFv2Cert(path string) (*tls.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cover cert: %w", err)
	}
	cert, err := tls.X509KeyPair(raw, raw)
	if err != nil {
		return nil, fmt.Errorf("cover cert: %w", err)
	}
	return &cert, nil
}
