package cards

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"google.golang.org/protobuf/proto"
)

// managedFlags are set by the builder, never by Config.Flags.
const managedFlags = objects.FlagUnlisted | objects.FlagInviteScoped | objects.FlagExit | objects.FlagCluster

// callerFlags are the card flags the caller may choose.
const callerFlags = objects.FlagRelay | objects.FlagDHTServer | objects.FlagMailbox | objects.FlagDomesticOnly

func isCamouflaged(suite uint32) bool {
	switch suite {
	case objects.SuiteReality2QUIC, objects.SuiteReality2TCP, objects.SuiteWS2:
		return true
	}
	return false
}

func isV2Suite(suite uint32) bool {
	return suite >= objects.SuiteNoise3 && suite <= objects.SuiteWS2
}

// prepareConfig validates the material and normalises the repeated fields. It mutates cfg.
func prepareConfig(cfg *Config, now time.Time) error {
	if cfg.Visibility > Anonymous {
		return fmt.Errorf("%w: visibility %d", ErrConfig, cfg.Visibility)
	}
	if cfg.Role > objects.RoleServer {
		return fmt.Errorf("%w: role %d", ErrConfig, cfg.Role)
	}
	cfg.Flags &= callerFlags

	if cfg.Exit != nil {
		if err := validateExitOffer(cfg.Exit.ExitOffer()); err != nil {
			return err
		}
	}

	if cfg.Device == nil {
		return fmt.Errorf("%w: no device", ErrConfig)
	}
	devicePub, err := parseDeviceKey(cfg.Device)
	if err != nil {
		return err
	}

	account, err := keys.ParseKind(cfg.Account, keys.Ed25519)
	if err != nil {
		return fmt.Errorf("%w: account: %v", ErrConfig, err)
	}
	if err := keys.CheckEd25519Public(account.Key()); err != nil {
		return fmt.Errorf("%w: account: %v", ErrConfig, err)
	}
	cfg.Account = clone(account)

	if cfg.HsKey, err = keys.ParseKind(cfg.HsKey, keys.X25519); err != nil {
		return fmt.Errorf("%w: hs_key: %v", ErrConfig, err)
	}
	if cfg.PrevHs != nil {
		pk, err := keys.ParseKind(cfg.PrevHs.GetKey(), keys.X25519)
		if err != nil {
			return fmt.Errorf("%w: prev_hs: %v", ErrConfig, err)
		}
		cfg.PrevHs = &pb.HsKey{Key: clone(pk), NotAfter: cfg.PrevHs.GetNotAfter()}
	}

	if len(cfg.Devices) == 0 {
		return fmt.Errorf("%w: no device list", ErrConfig)
	}
	if err := checkDeviceList(cfg.Devices, cfg.Account, devicePub, now); err != nil {
		return err
	}
	cfg.Devices = clone(cfg.Devices)

	if n := len(cfg.PrekeysHash); n != 0 && n != 32 {
		return fmt.Errorf("%w: prekeys_hash of %d bytes", ErrConfig, n)
	}
	cfg.PrekeysHash = clone(cfg.PrekeysHash)

	if len(cfg.Release) > maxRelease {
		return fmt.Errorf("%w: release of %d bytes, limit %d", ErrConfig, len(cfg.Release), maxRelease)
	}
	if len(cfg.Shop) > maxShop {
		return fmt.Errorf("%w: %d shop links, limit %d", ErrConfig, len(cfg.Shop), maxShop)
	}
	for _, s := range cfg.Shop {
		if err := objects.ValidateShopLink(s); err != nil {
			return fmt.Errorf("%w: %v", ErrConfig, err)
		}
	}
	cfg.Shop = cloneStrings(cfg.Shop)

	if len(cfg.Listeners) > maxAddrs {
		return fmt.Errorf("%w: %d listeners, limit %d", ErrConfig, len(cfg.Listeners), maxAddrs)
	}
	masks := make(map[string]struct{})
	for i, l := range cfg.Listeners {
		if err := validateListener(l); err != nil {
			return fmt.Errorf("%w: listener %d: %v", ErrConfig, i+1, err)
		}
		masks[string(l.Mask)] = struct{}{}
	}
	if len(masks) > maxMaskKeys {
		return fmt.Errorf("%w: %d masking keys, limit %d", ErrConfig, len(masks), maxMaskKeys)
	}
	cfg.Listeners = cloneListeners(cfg.Listeners)

	cfg.Features = append([]uint32(nil), cfg.Features...)
	slices.Sort(cfg.Features)
	cfg.Features = slices.Compact(cfg.Features)
	if len(cfg.Features) > maxFeatures {
		return fmt.Errorf("%w: %d features, limit %d", ErrConfig, len(cfg.Features), maxFeatures)
	}

	services, err := normaliseServices(cfg.Services)
	if err != nil {
		return err
	}
	cfg.Services = services

	cfg.L1Suites = append([]uint32(nil), cfg.L1Suites...)
	for _, s := range cfg.L1Suites {
		if s > 0xffff {
			return fmt.Errorf("%w: L1 suite %#x", ErrConfig, s)
		}
	}
	if len(cfg.L1Suites) == 0 {
		if cfg.Visibility == Visible {
			cfg.L1Suites = []uint32{1, 2} // XK, XX
		} else {
			cfg.L1Suites = []uint32{1} // hidden nodes do not accept XX (D22)
		}
	}
	return nil
}

func normaliseServices(in []*pb.ServiceOffer) ([]*pb.ServiceOffer, error) {
	if len(in) > maxServices {
		return nil, fmt.Errorf("%w: %d services, limit %d", ErrConfig, len(in), maxServices)
	}
	out := make([]*pb.ServiceOffer, 0, len(in))
	seen := make(map[uint32]struct{}, len(in))
	for i, s := range in {
		if s == nil {
			return nil, fmt.Errorf("%w: service %d is nil", ErrConfig, i)
		}
		if _, dup := seen[s.GetId()]; dup {
			return nil, fmt.Errorf("%w: duplicate service %d", ErrConfig, s.GetId())
		}
		seen[s.GetId()] = struct{}{}
		versions := append([]uint32(nil), s.GetVersions()...)
		slices.Sort(versions)
		versions = slices.Compact(versions)
		out = append(out, &pb.ServiceOffer{Id: s.GetId(), Versions: versions})
	}
	slices.SortFunc(out, func(a, b *pb.ServiceOffer) int {
		switch {
		case a.GetId() < b.GetId():
			return -1
		case a.GetId() > b.GetId():
			return 1
		}
		return 0
	})
	return out, nil
}

func validateListener(l Listener) error {
	if !isV2Suite(l.Suite) {
		return fmt.Errorf("suite %d", l.Suite)
	}
	if _, err := keys.ParseKind(l.Mask, keys.X25519); err != nil {
		return fmt.Errorf("masking key: %v", err)
	}
	if len(l.SNI) > maxSNI {
		return fmt.Errorf("sni of %d bytes", len(l.SNI))
	}
	if l.Suite == objects.SuiteWS2 {
		if l.URL == "" {
			return fmt.Errorf("ws2 listener without url")
		}
		if len(l.URL) > maxURL {
			return fmt.Errorf("url of %d bytes", len(l.URL))
		}
		return nil
	}
	if l.Host == "" || l.Port == 0 || l.Port > 65535 {
		return fmt.Errorf("host and port required")
	}
	if len(l.Host) > maxHost {
		return fmt.Errorf("host of %d bytes", len(l.Host))
	}
	return nil
}

// validateExtraListeners checks the listeners an invitation adds to the node's own set.
func validateExtraListeners(base, extra []Listener) error {
	if len(base)+len(extra) > maxAddrs {
		return fmt.Errorf("%w: more than %d addresses", ErrConfig, maxAddrs)
	}
	masks := make(map[string]struct{}, len(base)+len(extra))
	for _, l := range base {
		masks[string(l.Mask)] = struct{}{}
	}
	for i, l := range extra {
		if err := validateListener(l); err != nil {
			return fmt.Errorf("%w: invitation listener %d: %v", ErrConfig, i+1, err)
		}
		masks[string(l.Mask)] = struct{}{}
	}
	if len(masks) > maxMaskKeys {
		return fmt.Errorf("%w: more than %d masking keys", ErrConfig, maxMaskKeys)
	}
	return nil
}

func validateExitOffer(o *pb.ExitOffer) error {
	if o == nil {
		return nil
	}
	r := o.GetRegion()
	if r != "" && (len(r) != 2 || r[0] < 'A' || r[0] > 'Z' || r[1] < 'A' || r[1] > 'Z') {
		return fmt.Errorf("%w: exit region %q", ErrConfig, r)
	}
	return nil
}

func checkDeviceList(raw []byte, account, device keys.AKey, now time.Time) error {
	dl, err := objects.VerifyDeviceList(raw, now)
	if err != nil {
		return fmt.Errorf("%w: device list: %v", ErrConfig, err)
	}
	if !dl.Account.Equal(account) {
		return fmt.Errorf("%w: device list is signed by another account", ErrConfig)
	}
	if !dl.Contains(device) {
		return fmt.Errorf("%w: device list does not list the card signer", ErrConfig)
	}
	return nil
}

func parseDeviceKey(d Device) (keys.AKey, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: no device", ErrConfig)
	}
	pub, err := keys.ParseKind(d.Public(), keys.Ed25519)
	if err != nil {
		return nil, fmt.Errorf("%w: device key: %v", ErrConfig, err)
	}
	if err := keys.CheckEd25519Public(pub.Key()); err != nil {
		return nil, fmt.Errorf("%w: device key: %v", ErrConfig, err)
	}
	return pub, nil
}

// flagsAndOfferLocked computes the card flags and the live exit offer for one scope.
func (b *CardBuilder) flagsAndOfferLocked(scope objects.Scope) (uint32, *pb.ExitOffer, error) {
	flags := b.cfg.Flags &^ managedFlags
	var offer *pb.ExitOffer
	if b.cfg.Exit != nil {
		if o := b.cfg.Exit.ExitOffer(); o != nil {
			if err := validateExitOffer(o); err != nil {
				return 0, nil, err
			}
			offer = proto.Clone(o).(*pb.ExitOffer)
			flags |= objects.FlagExit
		}
	}
	switch scope {
	case objects.ScopeInvite:
		flags |= objects.FlagUnlisted | objects.FlagInviteScoped
	case objects.ScopePeer:
		if b.cfg.Visibility != Visible {
			flags |= objects.FlagUnlisted
		}
	}
	return flags, offer, nil
}

// buildBodyLocked assembles the card body. publicOnly drops the noise3 addresses and the masking
// keys only they use (objects.md §3.2, wire.md §2.1).
func (b *CardBuilder) buildBodyLocked(scope objects.Scope, inviteID []byte, listeners []Listener, publicOnly bool, maskNA func(Listener) (uint64, error)) (*pb.NodeCard, error) {
	flags, offer, err := b.flagsAndOfferLocked(scope)
	if err != nil {
		return nil, err
	}
	addrs, masks, err := buildAddrs(listeners, publicOnly, maskNA)
	if err != nil {
		return nil, err
	}
	return &pb.NodeCard{
		ProtoVer:    objects.MinProtocolVersion,
		HsKey:       clone(b.cfg.HsKey),
		L1Suites:    append([]uint32(nil), b.cfg.L1Suites...),
		Addrs:       addrs,
		MaskKeys:    masks,
		Role:        b.cfg.Role,
		Flags:       flags,
		Services:    cloneServices(b.cfg.Services),
		Features:    append([]uint32(nil), b.cfg.Features...),
		Exit:        offer,
		Shop:        cloneStrings(b.cfg.Shop),
		Release:     b.cfg.Release,
		Account:     clone(b.cfg.Account),
		PrekeysHash: clone(b.cfg.PrekeysHash),
		PrevHs:      cloneHsKey(b.cfg.PrevHs),
		Scope:       uint32(scope),
		InviteId:    clone(inviteID),
		Devices:     clone(b.cfg.Devices),
	}, nil
}

// buildAddrs turns listeners into card addresses, assigning one mask index per distinct key.
func buildAddrs(listeners []Listener, publicOnly bool, maskNA func(Listener) (uint64, error)) ([]*pb.Addr, []*pb.MaskKey, error) {
	index := make(map[string]uint32)
	var order []keys.AKey
	var notAfter []uint64

	addrs := make([]*pb.Addr, 0, len(listeners))
	for _, l := range listeners {
		if publicOnly && !isCamouflaged(l.Suite) {
			continue
		}
		if len(addrs) == maxAddrs {
			return nil, nil, fmt.Errorf("%w: more than %d addresses", ErrConfig, maxAddrs)
		}
		a, err := addrFromListener(l)
		if err != nil {
			return nil, nil, err
		}
		k := string(l.Mask)
		i, ok := index[k]
		if !ok {
			if len(order) == maxMaskKeys {
				return nil, nil, fmt.Errorf("%w: more than %d masking keys", ErrConfig, maxMaskKeys)
			}
			order = append(order, clone(l.Mask))
			notAfter = append(notAfter, 0)
			i = uint32(len(order))
			index[k] = i
		}
		na, err := maskNA(l)
		if err != nil {
			return nil, nil, err
		}
		if na != 0 && (notAfter[i-1] == 0 || na < notAfter[i-1]) {
			notAfter[i-1] = na
		}
		a.Mask = i
		addrs = append(addrs, a)
	}

	masks := make([]*pb.MaskKey, 0, len(order))
	for i, k := range order {
		masks = append(masks, &pb.MaskKey{Key: clone(k), NotAfter: notAfter[i]})
	}
	return addrs, masks, nil
}

func addrFromListener(l Listener) (*pb.Addr, error) {
	a := &pb.Addr{Suite: l.Suite}
	if l.Bootstrap {
		a.Flags |= objects.AddrFlagBootstrap
	}
	if l.Suite == objects.SuiteWS2 {
		a.Url = l.URL
	} else {
		a.Host = l.Host
		a.Port = l.Port
	}
	sp := &pb.SuiteParams{Sni: l.SNI}
	if l.Shape != nil {
		sp.Shape = proto.Clone(l.Shape).(*pb.ShapeProfile)
	}
	if sp.GetSni() != "" || sp.GetShape() != nil {
		raw, err := objects.Marshal(sp)
		if err != nil {
			return nil, fmt.Errorf("%w: suite params: %v", ErrConfig, err)
		}
		a.Params = raw
	}
	return a, nil
}

// fingerprintLocked is a stable digest of the card material. Mask expiry is taken as configured,
// not as capped for a particular card, so an unchanged configuration keeps the same digest.
func (b *CardBuilder) fingerprintLocked() ([32]byte, error) {
	body, err := b.buildBodyLocked(objects.ScopePeer, nil, b.cfg.Listeners, false, func(l Listener) (uint64, error) {
		return l.MaskNotAfter, nil
	})
	if err != nil {
		return [32]byte{}, err
	}
	raw, err := objects.Marshal(body)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: fingerprint: %v", ErrConfig, err)
	}
	pub, err := parseDeviceKey(b.cfg.Device)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	h.Write(raw)
	h.Write(pub)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func cloneConfig(cfg Config) Config {
	out := cfg
	out.Account = clone(cfg.Account)
	out.HsKey = clone(cfg.HsKey)
	out.PrevHs = cloneHsKey(cfg.PrevHs)
	out.L1Suites = append([]uint32(nil), cfg.L1Suites...)
	out.Features = append([]uint32(nil), cfg.Features...)
	out.Services = cloneServices(cfg.Services)
	out.Listeners = cloneListeners(cfg.Listeners)
	out.Devices = clone(cfg.Devices)
	out.PrekeysHash = clone(cfg.PrekeysHash)
	out.Shop = cloneStrings(cfg.Shop)
	return out
}

func cloneListeners(in []Listener) []Listener {
	if in == nil {
		return nil
	}
	out := make([]Listener, len(in))
	for i, l := range in {
		out[i] = l
		out[i].Mask = clone(l.Mask)
		if l.Shape != nil {
			out[i].Shape = proto.Clone(l.Shape).(*pb.ShapeProfile)
		}
	}
	return out
}

func cloneServices(in []*pb.ServiceOffer) []*pb.ServiceOffer {
	if in == nil {
		return nil
	}
	out := make([]*pb.ServiceOffer, len(in))
	for i, s := range in {
		if s == nil {
			continue
		}
		out[i] = proto.Clone(s).(*pb.ServiceOffer)
	}
	return out
}

func cloneHsKey(k *pb.HsKey) *pb.HsKey {
	if k == nil {
		return nil
	}
	return &pb.HsKey{Key: clone(k.GetKey()), NotAfter: k.GetNotAfter()}
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}
