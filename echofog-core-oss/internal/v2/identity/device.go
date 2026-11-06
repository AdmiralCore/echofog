package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/secret"
)

// x25519KeySize is the size of an X25519 private or public key.
const x25519KeySize = 32

// KeyPair is an X25519 private key and its public key: the Noise static key hs and the masking
// keys are key pairs of this shape (README.md §5.1).
type KeyPair struct {
	priv *secret.Value
	pub  keys.AKey
}

// NewKeyPair makes a random X25519 key pair.
func NewKeyPair() (*KeyPair, error) {
	priv := make([]byte, x25519KeySize)
	if _, err := rand.Read(priv); err != nil {
		return nil, err
	}
	defer secret.Wipe(priv)
	return KeyPairFromPrivate(priv)
}

// KeyPairFromPrivate rebuilds a key pair from its 32-byte private key, for loading it from the
// store.
func KeyPairFromPrivate(priv []byte) (*KeyPair, error) {
	if len(priv) != x25519KeySize {
		return nil, fmt.Errorf("%w: x25519 private key is %d bytes", ErrKeyLength, len(priv))
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("identity: x25519 private key: %w", err)
	}
	return &KeyPair{
		priv: secret.New(priv),
		pub:  keys.New(keys.X25519, key.PublicKey().Bytes()),
	}, nil
}

// Public returns the algorithm-tagged public key. The caller may keep the copy.
func (k *KeyPair) Public() keys.AKey { return append(keys.AKey(nil), k.pub...) }

// WithPrivate runs fn with the private key. The bytes are valid only for the call.
func (k *KeyPair) WithPrivate(fn func([]byte) error) error { return k.priv.Use(fn) }

// Destroy wipes the private key.
func (k *KeyPair) Destroy() { k.priv.Destroy() }

// Device is one installation of an account: a random device signing key, the Noise static key hs
// and the contacts masking key (README.md §5.1, K1: none of them is derived from the account).
type Device struct {
	seed *secret.Value // Ed25519 seed of the device key
	pub  keys.AKey
	id   [32]byte
	hs   *KeyPair
	mask *KeyPair
}

// NewDevice makes a new random device. Restoring the same mnemonic again makes a different device.
func NewDevice() (*Device, error) {
	seed, err := secret.NewRandom(ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	var pub keys.AKey
	if err := seed.Use(func(b []byte) error {
		signer, err := keys.NewEd25519Signer(b)
		if err != nil {
			return err
		}
		pub = signer.Public()
		return nil
	}); err != nil {
		seed.Destroy()
		return nil, err
	}

	hs, err := NewKeyPair()
	if err != nil {
		seed.Destroy()
		return nil, err
	}
	mask, err := NewKeyPair()
	if err != nil {
		seed.Destroy()
		hs.Destroy()
		return nil, err
	}
	return &Device{
		seed: seed,
		pub:  append(keys.AKey(nil), pub...),
		id:   pub.ID(),
		hs:   hs,
		mask: mask,
	}, nil
}

// DeviceFromKeys rebuilds a device from the material the store keeps: the Ed25519 seed of the
// device key and the X25519 private keys of hs and of the contacts masking key.
func DeviceFromKeys(seed, hsPriv, maskPriv []byte) (*Device, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: device seed is %d bytes", ErrKeyLength, len(seed))
	}
	signer, err := keys.NewEd25519Signer(seed)
	if err != nil {
		return nil, err
	}
	hs, err := KeyPairFromPrivate(hsPriv)
	if err != nil {
		return nil, err
	}
	mask, err := KeyPairFromPrivate(maskPriv)
	if err != nil {
		hs.Destroy()
		return nil, err
	}
	pub := signer.Public()
	return &Device{
		seed: secret.New(seed),
		pub:  append(keys.AKey(nil), pub...),
		id:   pub.ID(),
		hs:   hs,
		mask: mask,
	}, nil
}

// Public returns the algorithm-tagged device key. The caller may keep the copy.
func (d *Device) Public() keys.AKey { return append(keys.AKey(nil), d.pub...) }

// ID returns SHA-256 of the algorithm-tagged device key (README.md §5.2).
func (d *Device) ID() [32]byte { return d.id }

// Handshake returns the Noise static key hs.
func (d *Device) Handshake() *KeyPair { return d.hs }

// Masking returns the contacts masking key. Keys of single invitations and REALITY listeners are
// made by the packages that own them (access, carrier).
func (d *Device) Masking() *KeyPair { return d.mask }

// WithSigner runs fn with a signer for the device key. The signer lives only for the call.
func (d *Device) WithSigner(fn func(*keys.Signer) error) error {
	if fn == nil {
		return nil
	}
	return d.seed.Use(func(seed []byte) error {
		signer, err := keys.NewEd25519Signer(seed)
		if err != nil {
			return err
		}
		return fn(signer)
	})
}

// Sign signs msg with the device key.
func (d *Device) Sign(msg []byte) ([]byte, error) {
	var sig []byte
	err := d.WithSigner(func(s *keys.Signer) error {
		sig = s.Sign(msg)
		return nil
	})
	return sig, err
}

// WithSeed runs fn with the Ed25519 seed of the device key, so the store can seal it.
func (d *Device) WithSeed(fn func([]byte) error) error { return d.seed.Use(fn) }

// Destroy wipes the device secrets.
func (d *Device) Destroy() {
	d.seed.Destroy()
	d.hs.Destroy()
	d.mask.Destroy()
}

// Throwaway is a fresh identity with a random account and one random device, used where nothing
// about the real identity may be linked: an XX handshake to a bare address (handshake.md §4.2) and
// a client identity per exit (services.md §7). It is never derived from the mnemonic and cannot be
// restored.
type Throwaway struct {
	Account *Account
	Device  *Device
}

// NewThrowaway makes a throwaway identity: a fresh account key, device key and handshake static
// key, all never reused.
func NewThrowaway() (*Throwaway, error) {
	// A random 64-byte seed has no mnemonic behind it and can never be restored.
	masterSeed := make([]byte, 64)
	if _, err := rand.Read(masterSeed); err != nil {
		return nil, err
	}
	defer secret.Wipe(masterSeed)

	account, err := newAccount(masterSeed, DefaultAccount)
	if err != nil {
		return nil, err
	}
	device, err := NewDevice()
	if err != nil {
		account.Destroy()
		return nil, err
	}
	return &Throwaway{Account: account, Device: device}, nil
}

// ExitIdentity is a per-exit client identity (services.md §7): the same throwaway shape, kept
// locally per exit so the exit sees a stable identity that is still not the messenger one.
type ExitIdentity = Throwaway

// NewExitIdentity makes a fresh throwaway identity for one exit.
func NewExitIdentity() (*ExitIdentity, error) { return NewThrowaway() }

// DeviceList signs the one-device list of the throwaway identity (objects.md §6).
func (t *Throwaway) DeviceList(seq uint64, now time.Time) ([]byte, error) {
	return t.Account.SealSingleDeviceList(t.Device, seq, now)
}

// Destroy wipes the identity.
func (t *Throwaway) Destroy() {
	if t.Account != nil {
		t.Account.Destroy()
	}
	if t.Device != nil {
		t.Device.Destroy()
	}
}

// ThrowawaySeeds is the secret material of one per-exit client identity, as the store keeps it
// (services.md §7: "stored locally, never derived from the mnemonic"). It is random, so it only
// ever restores the same exit identity, never the messenger one.
type ThrowawaySeeds struct {
	Account   [32]byte
	Device    [32]byte
	Handshake [32]byte
	Mask      [32]byte
}

// NewThrowawaySeeds draws fresh random seeds.
func NewThrowawaySeeds() (ThrowawaySeeds, error) { return ThrowawaySeeds{}, errNotImplemented }

// ThrowawayFromSeeds rebuilds a throwaway identity from its seeds.
func ThrowawayFromSeeds(ThrowawaySeeds) (*Throwaway, error) { return nil, errNotImplemented }

var errNotImplemented = errors.New("identity: not implemented")
