package vpnmarket

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/pkg/api/types"
)

const (
	AdvertVersion = 1
	// AdvertTTL is how long a published advert stays valid; nodes republish well before it ends
	AdvertTTL         = 12 * time.Minute
	RepublishInterval = 4 * time.Minute

	MaxFreeTestKB   = 1024
	MaxNeighbors    = 8
	MaxPricePerMB   = 1_000_000_000
	MaxUploadKbps   = 100_000_000
	maxClockSkew    = 60 * time.Second
	advertKeyPrefix = "echofog/vpn-exit/v1/"
)

// Advert is a signed offer of a VPN exit node. Clients verify the signature themselves and never trust the DHT layer
type Advert struct {
	Version    int      `json:"v"`
	PeerID     string   `json:"peer"`
	PubKey     []byte   `json:"pub"`
	Enabled    bool     `json:"enabled"`
	PricePerMB int64    `json:"price_per_mb"`
	UploadKbps int      `json:"upload_kbps"`
	FreeTestKB int      `json:"free_test_kb"`
	Neighbors  []string `json:"neighbors,omitempty"`
	IssuedAt   int64    `json:"issued_at"`
	ExpiresAt  int64    `json:"expires_at"`
	Sig        []byte   `json:"sig,omitempty"`
}

// Signer produces an ed25519 signature with the node identity
type Signer interface {
	CreateSignature(data []byte) ([]byte, error)
}

var (
	ErrAdvertInvalid = errors.New("invalid exit advert")
	ErrAdvertExpired = errors.New("exit advert expired")
)

// Key is the DHT key under which a node's advert lives
func Key(peer types.PeerID) []byte {
	h := sha256.Sum256([]byte(advertKeyPrefix + hex.EncodeToString(peer[:])))
	return h[:]
}

func (a *Advert) signingBytes() ([]byte, error) {
	cp := *a
	cp.Sig = nil
	return json.Marshal(&cp)
}

// Sign fills the signature field
func (a *Advert) Sign(s Signer) error {
	raw, err := a.signingBytes()
	if err != nil {
		return err
	}
	a.Sig, err = s.CreateSignature(raw)
	return err
}

// Encode serialises the advert for the DHT
func (a *Advert) Encode() ([]byte, error) { return json.Marshal(a) }

// Decode parses and fully verifies an advert received from the network
func Decode(raw []byte, now time.Time) (*Advert, error) {
	if len(raw) == 0 || len(raw) > 4096 {
		return nil, ErrAdvertInvalid
	}
	var a Advert
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAdvertInvalid, err)
	}
	if err := a.Verify(now); err != nil {
		return nil, err
	}
	return &a, nil
}

// ID returns the typed peer ID of the advertiser
func (a *Advert) ID() (types.PeerID, error) {
	var id types.PeerID
	raw, err := hex.DecodeString(a.PeerID)
	if err != nil || len(raw) != len(id) {
		return id, ErrAdvertInvalid
	}
	copy(id[:], raw)
	return id, nil
}

// Verify checks structure, bounds, key ownership and signature
func (a *Advert) Verify(now time.Time) error {
	if a.Version != AdvertVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrAdvertInvalid, a.Version)
	}
	if len(a.PubKey) != ed25519.PublicKeySize || len(a.Sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: bad key or signature size", ErrAdvertInvalid)
	}
	id, err := a.ID()
	if err != nil {
		return err
	}
	var pub types.PeerPublicKey
	copy(pub[:], a.PubKey)
	if types.PeerPubKeyToID(pub) != id {
		return fmt.Errorf("%w: peer id does not match public key", ErrAdvertInvalid)
	}
	if a.PricePerMB < 0 || a.PricePerMB > MaxPricePerMB || a.UploadKbps < 0 || a.UploadKbps > MaxUploadKbps ||
		a.FreeTestKB < 0 || a.FreeTestKB > MaxFreeTestKB || len(a.Neighbors) > MaxNeighbors {
		return fmt.Errorf("%w: value out of range", ErrAdvertInvalid)
	}
	for _, n := range a.Neighbors {
		if raw, err := hex.DecodeString(n); err != nil || len(raw) != 32 {
			return fmt.Errorf("%w: bad neighbor id", ErrAdvertInvalid)
		}
	}
	issued, expires := time.Unix(a.IssuedAt, 0), time.Unix(a.ExpiresAt, 0)
	if issued.After(now.Add(maxClockSkew)) || !expires.After(issued) || expires.Sub(issued) > 30*time.Minute {
		return fmt.Errorf("%w: bad validity window", ErrAdvertInvalid)
	}
	if !expires.After(now) {
		return ErrAdvertExpired
	}
	raw, err := a.signingBytes()
	if err != nil {
		return err
	}
	if !ed25519.Verify(a.PubKey, raw, a.Sig) {
		return fmt.Errorf("%w: signature check failed", ErrAdvertInvalid)
	}
	return nil
}
