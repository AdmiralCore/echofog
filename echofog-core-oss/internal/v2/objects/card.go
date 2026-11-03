package objects

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Scope is NodeCard.scope (objects.md §3.2).
type Scope uint32

const (
	ScopePublic Scope = 0
	ScopePeer   Scope = 1
	ScopeInvite Scope = 2
)

func (s Scope) String() string {
	switch s {
	case ScopePublic:
		return "public"
	case ScopePeer:
		return "peer"
	case ScopeInvite:
		return "invite"
	}
	return "unknown"
}

// Node roles and card flags (registries.md §11).
const (
	RoleUser   = 0
	RoleServer = 1

	FlagUnlisted     = 1 << 0
	FlagRelay        = 1 << 1
	FlagDHTServer    = 1 << 2
	FlagMailbox      = 1 << 3
	FlagInviteScoped = 1 << 4
	FlagExit         = 1 << 5
	FlagDomesticOnly = 1 << 6

	// AddrFlagBootstrap is Addr.flags bit 0: the address accepts XX handshakes.
	AddrFlagBootstrap = 1 << 0
)

// L0 suites (registries.md §6.1).
const (
	SuiteNoise3       = 1
	SuiteReality2QUIC = 2
	SuiteReality2TCP  = 3
	SuiteWS2          = 4
)

// MinProtocolVersion is the lowest proto_ver of a v2 card (versioning.md §1).
const MinProtocolVersion = 200

// Processing limits of the card's repeated fields (objects.md §3.1).
const (
	maxAddrs     = 16
	maxMaskKeys  = 8
	maxServices  = 16
	maxFeatures  = 64
	maxShop      = 3
	maxRelease   = 32
	maxHost      = 253
	maxURL       = 512
	maxSNI       = 253
	inviteIDLen  = 8
	maskKeySlack = 7 * day // MaskKey.not_after ≤ card not_after + 7 days
)

func isV2Suite(s uint32) bool { return s >= SuiteNoise3 && s <= SuiteWS2 }

func isCamouflaged(s uint32) bool {
	return s == SuiteReality2QUIC || s == SuiteReality2TCP || s == SuiteWS2
}

// Card is a verified node card.
type Card struct {
	Env  *Envelope
	Body *pb.NodeCard
	// DeviceKey is the envelope signer; DeviceID = SHA-256(DeviceKey).
	DeviceKey keys.AKey
	DeviceID  [32]byte
	// Account is NodeCard.account; NodeID = SHA-256(Account).
	Account keys.AKey
	NodeID  [32]byte
	// Devices is the embedded account-signed device list that lists DeviceKey.
	Devices *DeviceList
	// Scope is NodeCard.scope as received; values above 2 are unknown (see Publishable).
	Scope Scope
	// HsKey is the handshake key (x25519 akey).
	HsKey keys.AKey
}

// VerifyCard verifies a node card: the envelope (objects.md §2.2), the embedded device list, which
// must be signed by the card's account key and list the signing device, and the card rules of §3.
func VerifyCard(b []byte, now time.Time) (*Card, error) {
	return verifyCardAt(b, &now)
}

func verifyCardAt(b []byte, now *time.Time) (*Card, error) {
	e, err := verifyAt(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelCard); err != nil {
		return nil, err
	}
	body := &pb.NodeCard{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	c := &Card{Env: e, Body: body, DeviceKey: e.Signer, DeviceID: e.Signer.ID(), Scope: Scope(body.GetScope())}
	if e.Signer.Alg() != keys.Ed25519 {
		return nil, invalid("card signer: %s key", e.Signer.Alg())
	}

	// Lifetime: 7 days, 30 days for invite-scoped cards (D12).
	if now != nil {
		max := uint64(CardMaxLifetime)
		if c.Scope == ScopeInvite {
			max = InviteCardMaxLifetime
		}
		t := unix(*now)
		if e.NotAfter > t && e.NotAfter-t > max+ClockSkew {
			return nil, ErrLifetime
		}
	}

	if body.GetProtoVer() < MinProtocolVersion {
		return nil, invalid("proto_ver %d", body.GetProtoVer())
	}
	if c.HsKey, err = keys.ParseKind(body.GetHsKey(), keys.X25519); err != nil {
		return nil, invalid("hs_key: %v", err)
	}
	if ph := body.GetPrevHs(); ph != nil {
		if _, err := keys.ParseKind(ph.GetKey(), keys.X25519); err != nil {
			return nil, invalid("prev_hs: %v", err)
		}
	}

	// Account and device list (D35).
	if c.Account, err = keys.ParseKind(body.GetAccount(), keys.Ed25519); err != nil {
		return nil, invalid("account: %v", err)
	}
	if err := keys.CheckEd25519Public(c.Account.Key()); err != nil {
		return nil, invalid("account: %v", err)
	}
	c.NodeID = c.Account.ID()
	if len(body.GetDevices()) == 0 {
		return nil, invalid("devices: missing")
	}
	dl, err := verifyDeviceListAt(body.GetDevices(), now)
	if err != nil {
		return nil, invalid("devices: %v", err)
	}
	if !dl.Account.Equal(c.Account) {
		return nil, invalid("devices: signed by another key than the card's account")
	}
	if !dl.Contains(c.DeviceKey) {
		return nil, invalid("devices: the signing device is not in the account's device list")
	}
	c.Devices = dl

	if err := checkScope(c); err != nil {
		return nil, err
	}
	if err := checkAddrs(c); err != nil {
		return nil, err
	}

	// Roles and offers.
	flags := body.GetFlags()
	// A server is dialed through a camouflaged suite across borders. Invitation cards are exempt:
	// node-card.json's invitation card is a server's noise3-only card (spec issue, see doc.go).
	if body.GetRole() == RoleServer && flags&FlagDomesticOnly == 0 && c.Scope != ScopeInvite {
		ok := false
		for _, a := range body.GetAddrs() {
			if isCamouflaged(a.GetSuite()) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, invalid("server card without a camouflaged address")
		}
	}
	// Flag exit says the offer is live, so it needs an offer. An offer without the flag is kept but
	// not live: node-card.json's invitation card carries one without the flag (spec issue, doc.go).
	if flags&FlagExit != 0 && body.GetExit() == nil {
		return nil, invalid("flag exit without an exit offer")
	}
	if x := body.GetExit(); x != nil {
		if r := x.GetRegion(); r != "" && !isRegion(r) {
			return nil, invalid("exit region %q", r)
		}
	}
	for _, s := range body.GetShop() {
		if err := ValidateShopLink(s); err != nil {
			return nil, err
		}
	}
	if len(body.GetRelease()) > maxRelease {
		return nil, invalid("release longer than %d bytes", maxRelease)
	}
	if h := body.GetPrekeysHash(); len(h) != 0 && len(h) != 32 {
		return nil, invalid("prekeys_hash of %d bytes", len(h))
	}
	return c, nil
}

func isRegion(r string) bool {
	return len(r) == 2 && r[0] >= 'A' && r[0] <= 'Z' && r[1] >= 'A' && r[1] <= 'Z'
}

func checkScope(c *Card) error {
	flags := c.Body.GetFlags()
	inv := c.Body.GetInviteId()
	switch c.Scope {
	case ScopePublic:
		if flags&(FlagUnlisted|FlagInviteScoped) != 0 {
			return invalid("public card with flag unlisted or invite-scoped")
		}
	case ScopeInvite:
		if len(inv) != inviteIDLen {
			return invalid("invite card: invite_id of %d bytes", len(inv))
		}
		if flags&(FlagUnlisted|FlagInviteScoped) != FlagUnlisted|FlagInviteScoped {
			return invalid("invite card without flags unlisted and invite-scoped")
		}
		return nil
	}
	if len(inv) != 0 {
		return invalid("invite_id outside an invite card")
	}
	if flags&FlagInviteScoped != 0 {
		return invalid("flag invite-scoped outside an invite card")
	}
	return nil
}

func checkAddrs(c *Card) error {
	body := c.Body
	masks := body.GetMaskKeys()
	limit := c.Env.NotAfter + maskKeySlack
	if limit < c.Env.NotAfter {
		limit = ^uint64(0)
	}
	for i, m := range masks {
		if _, err := keys.ParseKind(m.GetKey(), keys.X25519); err != nil {
			return invalid("mask key %d: %v", i+1, err)
		}
		if m.GetNotAfter() > limit {
			return invalid("mask key %d: not_after more than 7 days after the card's", i+1)
		}
	}
	used := make([]bool, len(masks))
	for i, a := range body.GetAddrs() {
		s := a.GetSuite()
		if c.Scope == ScopePublic && s == SuiteNoise3 {
			return invalid("noise3 address in a public card")
		}
		if a.GetPort() > 65535 {
			return invalid("address %d: port %d", i, a.GetPort())
		}
		if len(a.GetHost()) > maxHost || len(a.GetUrl()) > maxURL {
			return invalid("address %d: host or url too long", i)
		}
		if m := a.GetMask(); m != 0 {
			if int(m) > len(masks) {
				return invalid("address %d: mask index %d out of range", i, m)
			}
			used[m-1] = true
		} else if isV2Suite(s) {
			return invalid("address %d: no masking key", i)
		}
		if p := a.GetParams(); len(p) != 0 {
			sp := &pb.SuiteParams{}
			if err := unmarshalCanonical(p, sp); err != nil {
				return invalid("address %d params: %v", i, err)
			}
			if len(sp.GetSni()) > maxSNI {
				return invalid("address %d: sni too long", i)
			}
		}
		if !isV2Suite(s) {
			continue // unknown suites: no further rules
		}
		if s == SuiteWS2 {
			if err := checkWSURL(a.GetUrl()); err != nil {
				return invalid("address %d: %v", i, err)
			}
			continue
		}
		if a.GetHost() == "" || a.GetPort() == 0 {
			return invalid("address %d: host and port required", i)
		}
	}
	for i, u := range used {
		if !u {
			return invalid("mask key %d is not used by any address", i+1)
		}
	}
	return nil
}

func checkWSURL(s string) error {
	if !strings.HasPrefix(s, "wss://") {
		return errors.New("ws2 url is not wss://")
	}
	if err := checkURLChars(s, "ws2 url"); err != nil {
		return err
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("bad ws2 url")
	}
	return nil
}

// Role returns the card's role; unknown values are treated as user (registries.md §11).
func (c *Card) Role() uint32 {
	if c.Body.GetRole() == RoleServer {
		return RoleServer
	}
	return RoleUser
}

// ExitOffer returns the card's live exit offer: the offer when flag exit is set, otherwise nil.
func (c *Card) ExitOffer() *pb.ExitOffer {
	if c.Body.GetFlags()&FlagExit == 0 {
		return nil
	}
	return c.Body.GetExit()
}

// Publishable reports whether the card may be stored in the DHT and passed to third parties (PEX,
// NODES): public scope and not unlisted. Peer, invite and unknown scopes are never forwarded.
func (c *Card) Publishable() bool {
	return c.Scope == ScopePublic && c.Body.GetFlags()&(FlagUnlisted|FlagInviteScoped) == 0
}

// MaskKeys returns the processed masking keys (at most 8).
func (c *Card) MaskKeys() []*pb.MaskKey {
	m := c.Body.GetMaskKeys()
	if len(m) > maxMaskKeys {
		m = m[:maxMaskKeys]
	}
	return m
}

// Addrs returns the processed addresses: the first 16, without addresses whose masking key lies
// beyond the processed keys.
func (c *Card) Addrs() []*pb.Addr {
	a := c.Body.GetAddrs()
	if len(a) > maxAddrs {
		a = a[:maxAddrs]
	}
	out := make([]*pb.Addr, 0, len(a))
	for _, x := range a {
		if x.GetMask() > maxMaskKeys {
			continue
		}
		out = append(out, x)
	}
	return out
}

// Services returns the processed service offers (at most 16).
func (c *Card) Services() []*pb.ServiceOffer {
	s := c.Body.GetServices()
	if len(s) > maxServices {
		s = s[:maxServices]
	}
	return s
}

// Features returns the processed feature IDs (at most 64).
func (c *Card) Features() []uint32 {
	f := c.Body.GetFeatures()
	if len(f) > maxFeatures {
		f = f[:maxFeatures]
	}
	return f
}

// Shop returns the processed shop links (at most 3).
func (c *Card) Shop() []string {
	s := c.Body.GetShop()
	if len(s) > maxShop {
		s = s[:maxShop]
	}
	return s
}

// SuiteParams parses an address's params (already checked by VerifyCard).
func SuiteParams(a *pb.Addr) *pb.SuiteParams {
	sp := &pb.SuiteParams{}
	if len(a.GetParams()) != 0 {
		_ = unmarshalCanonical(a.GetParams(), sp)
	}
	return sp
}

// SealCard encodes and signs a card with the device key and checks the result against every card
// rule that does not depend on the clock.
func SealCard(device *keys.Signer, seq, notAfter uint64, body *pb.NodeCard) ([]byte, error) {
	b, err := Marshal(body)
	if err != nil {
		return nil, err
	}
	env, err := Seal(LabelCard, device, seq, notAfter, b)
	if err != nil {
		return nil, err
	}
	if _, err := verifyCardAt(env, nil); err != nil {
		return nil, err
	}
	return env, nil
}
