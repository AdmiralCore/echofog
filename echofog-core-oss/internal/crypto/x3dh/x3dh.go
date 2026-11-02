package x3dh

import (
	"bytes"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"math/big"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// Version 2: the identity keys are bound into every message, the initiator's identity comes from
// its signed bundle, and each KDF has its own label. Sessions of version 1 are not compatible.
const (
	DOMAIN_SEPARATION = "FOG_X3DH_V2"
	infoInitChains    = "FOG_RATCHET_INIT_V2"
	infoRoot          = "FOG_RATCHET_ROOT_V2"
	// infoPQXDH is the KDF info of a hybrid session (Signal's PQXDH with X25519, SHA-256 and
	// ML-KEM-768). The KEM key id and the hash of the ciphertext follow it, so the secret is bound
	// to the exact KEM key and ciphertext.
	infoPQXDH = "FOG_PQXDH_X25519_SHA-256_ML-KEM-768_V1"
)

var (
	ErrKEMKeyNotFound = errors.New("the KEM prekey the init packet names is not ours or is gone")
	ErrBadKEMInit     = errors.New("malformed KEM part of the init packet")
	ErrBadKEMKey      = errors.New("the bundle's KEM prekey cannot be used")
	// ErrPQDowngrade: a classic init from a node that has PQXDH, while we offer a KEM prekey. It
	// used a bundle of ours without one: stripped on the way, or one signed before we had a KEM
	// prekey (an old copy replayed, or cached). It has to start again from a fresh bundle.
	ErrPQDowngrade = errors.New("classic init from a PQXDH node to a node with a KEM prekey")
)

var (
	ErrDHComputation      = errors.New("dh computation failed")
	ErrHKDFFailed         = errors.New("hkdf derivation failed")
	ErrEnclaveOpenFailed  = errors.New("failed to open enclave")
	ErrOPKNotFound        = errors.New("one-time prekey not found")
	ErrEphemeralGenFailed = errors.New("failed to generate ephemeral key")
	ErrIdentityMismatch   = errors.New("the init packet names another identity key than the sender's bundle")
	ErrInitWithoutMessage = errors.New("the init packet carries no message")
	ErrInitRejected       = errors.New("no prekey of ours opens this init packet")
)

type X3DHEngine struct {
	bundle       *keymanager.PublicBundle
	st           storage.Storage
	accntManager *keymanager.AccountManager
}

func NewX3DHEngine(
	myBundle *keymanager.PublicBundle,
	st storage.Storage,
	ac *keymanager.AccountManager,
) *X3DHEngine {
	return &X3DHEngine{
		bundle:       myBundle,
		st:           st,
		accntManager: ac,
	}
}

type X3DHParameters struct {
	MyIdentityPriv  *memguard.Enclave
	MySignedPrePriv *memguard.Enclave
	MyEphemeralPriv *memguard.Enclave

	RemoteIdentityPub  [32]byte
	RemoteSignedPrePub [32]byte
	RemoteOneTimePub   *[32]byte

	// KEM is the post-quantum part (PQXDH); nil for classic X3DH.
	KEM *KEMInput
}

// KEMInput is the ML-KEM-768 part of a PQXDH secret: the shared secret, and the key id and
// ciphertext it came from (both bound into the KDF).
type KEMInput struct {
	SharedSecret []byte
	KeyID        [32]byte
	Ciphertext   []byte
}

func (k *KEMInput) wipe() {
	if k != nil {
		memguard.WipeBytes(k.SharedSecret)
	}
}

// associatedData binds a session to both identities, initiator first (X3DH: AD = IK_A || IK_B).
func associatedData(initiatorIK, responderIK [32]byte) []byte {
	return append(append([]byte(nil), initiatorIK[:]...), responderIK[:]...)
}

// X3DHInit starts a session with the owner of peerBundle, which the caller has verified. When the
// bundle offers one-time prekeys, one of them is picked at random. When it offers a KEM prekey the
// session is PQXDH: an ML-KEM-768 secret is mixed into the X3DH one; otherwise it is classic X3DH,
// as with a node of v0.2.0.
func (x *X3DHEngine) X3DHInit(peerBundle keymanager.PublicBundle) (*X3DHSession, types.X3DHInit, error) {
	var kem *KEMInput
	if peerBundle.HasKEM() {
		ek, err := mlkem.NewEncapsulationKey768(peerBundle.KEMPreKey)
		if err != nil {
			return nil, types.X3DHInit{}, ErrBadKEMKey
		}
		ss, ct := ek.Encapsulate()
		kem = &KEMInput{SharedSecret: ss, KeyID: keymanager.KEMKeyID(peerBundle.KEMPreKey), Ciphertext: ct}
		defer kem.wipe()
	}

	idPrivEnc, _, err := x.accntManager.GetEncryptionKey()
	if err != nil {
		return nil, types.X3DHInit{}, err
	}

	empPrivEnc, empPub, err := generateSecureEphemeral()
	if err != nil {
		return nil, types.X3DHInit{}, err
	}

	var remoteOPK *[32]byte
	if n := len(peerBundle.OneTimeKeys); n > 0 {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
		if err != nil {
			return nil, types.X3DHInit{}, err
		}
		k := peerBundle.OneTimeKeys[i.Int64()]
		remoteOPK = &k
	}

	params := X3DHParameters{
		MyIdentityPriv:     idPrivEnc,
		MyEphemeralPriv:    empPrivEnc,
		RemoteIdentityPub:  peerBundle.IdentityKey,
		RemoteSignedPrePub: peerBundle.SignedPreKey,
		RemoteOneTimePub:   remoteOPK,
		KEM:                kem,
	}

	rootKeyEnc, err := CalculateX3DH(params)
	if err != nil {
		return nil, types.X3DHInit{}, err
	}

	initPacket := types.X3DHInit{
		EphemeralKey:    empPub,
		IdentityKey:     x.bundle.IdentityKey,
		OneTimeKeyID:    remoteOPK,
		BundleCreatedAt: peerBundle.CreatedAt,
	}
	if kem != nil {
		id := kem.KeyID
		initPacket.KEMCiphertext, initPacket.KEMKeyID = kem.Ciphertext, &id
	}

	x3dhSession, err := NewX3DHSession(
		rootKeyEnc,
		empPrivEnc,
		peerBundle.PeerID,
		peerBundle.SignedPreKey,
		true,
	)
	if err != nil {
		return nil, types.X3DHInit{}, err
	}
	x3dhSession.AD = associatedData(x.bundle.IdentityKey, peerBundle.IdentityKey)
	x3dhSession.InitEK = empPub
	x3dhSession.PQ = kem != nil
	return x3dhSession, initPacket, nil
}

// responderKEM reads the KEM part of an init packet from the owner of sender (its signed card): the
// shared secret for a PQXDH init, nil for a classic one.
//
// Once we offer a KEM prekey, a classic init is taken only from a node without PQXDH (v0.2.0). A
// node with it uses the KEM prekey of every bundle that has one, so a classic init from it means it
// used a bundle of ours without one: stripped on the way, or one we signed before our first KEM
// prekey, replayed by whoever kept it or still in its cache (bundles are valid for 30 days). Which
// node sent it, the init and the card say, and both travel in the envelope the sender signed, so
// nobody on the way can make it look like v0.2.0: the init names the time of the bundle it used (a
// field v0.2.0 does not know), the card carries a KEM prekey.
func (x *X3DHEngine) responderKEM(init types.X3DHInit, sender *keymanager.PublicBundle) (*KEMInput, error) {
	if len(init.KEMCiphertext) == 0 && init.KEMKeyID == nil {
		if x.accntManager.KEMSince() == 0 {
			return nil, nil // we never offered a KEM prekey
		}
		if init.BundleCreatedAt != 0 || (sender != nil && sender.HasKEM()) {
			return nil, ErrPQDowngrade
		}
		return nil, nil // v0.2.0: it cannot read a KEM prekey
	}
	if len(init.KEMCiphertext) != mlkem.CiphertextSize768 || init.KEMKeyID == nil {
		return nil, ErrBadKEMInit
	}
	seedEnc, err := x.accntManager.OpenKEMPreKey(*init.KEMKeyID)
	if err != nil {
		return nil, ErrKEMKeyNotFound
	}
	seed, err := seedEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}
	defer seed.Destroy()
	dk, err := mlkem.NewDecapsulationKey768(seed.Bytes())
	if err != nil {
		return nil, ErrKEMKeyNotFound
	}
	// a ciphertext made for another key gives another secret (implicit rejection): the message of
	// the init then fails to open
	ss, err := dk.Decapsulate(init.KEMCiphertext)
	if err != nil {
		return nil, ErrBadKEMInit
	}
	return &KEMInput{SharedSecret: ss, KeyID: *init.KEMKeyID, Ciphertext: init.KEMCiphertext}, nil
}

// X3DHReceive answers init with the current signed prekey, without checking the message it carries
// and without using up the one-time prekey. Accept is what a node uses.
func (x *X3DHEngine) X3DHReceive(init types.X3DHInit, peerBundle keymanager.PublicBundle) (*X3DHSession, error) {
	spk, _, err := x.accntManager.GetSignedPreKey()
	if err != nil {
		return nil, err
	}
	var opk *memguard.Enclave
	if init.OneTimeKeyID != nil {
		if opk, err = x.accntManager.OpenOPK(*init.OneTimeKeyID); err != nil {
			return nil, ErrOPKNotFound
		}
	}
	kem, err := x.responderKEM(init, &peerBundle)
	if err != nil {
		return nil, err
	}
	defer kem.wipe()
	return x.receive(init, peerBundle, spk, opk, kem)
}

// Accept answers an init packet from the owner of peerBundle (verified by the caller) and opens the
// message it carries. It tries the current signed prekey and the replaced ones still in use, and
// uses up the one-time prekey only when the message opened. A PQXDH init is opened with the KEM
// prekey it names; a classic one only from v0.2.0 (see responderKEM).
func (x *X3DHEngine) Accept(init types.X3DHInit, peerBundle keymanager.PublicBundle) (*X3DHSession, []byte, error) {
	if len(init.SecureMessage.Ciphertext) == 0 {
		return nil, nil, ErrInitWithoutMessage
	}
	var opk *memguard.Enclave
	if init.OneTimeKeyID != nil {
		var err error
		if opk, err = x.accntManager.OpenOPK(*init.OneTimeKeyID); err != nil {
			return nil, nil, ErrOPKNotFound
		}
	}
	kem, err := x.responderKEM(init, &peerBundle)
	if err != nil {
		return nil, nil, err
	}
	defer kem.wipe()
	spks, err := x.accntManager.SignedPreKeys()
	if err != nil {
		return nil, nil, err
	}
	for _, spk := range spks {
		session, err := x.receive(init, peerBundle, spk, opk, kem)
		if err != nil {
			return nil, nil, err
		}
		plain, err := session.RatchetDecrypt(init.SecureMessage)
		if err != nil {
			continue
		}
		if init.OneTimeKeyID != nil {
			_ = x.accntManager.DeleteOPK(*init.OneTimeKeyID)
		}
		return session, plain, nil
	}
	return nil, nil, ErrInitRejected
}

func (x *X3DHEngine) receive(init types.X3DHInit, peerBundle keymanager.PublicBundle, spk, opk *memguard.Enclave, kem *KEMInput) (*X3DHSession, error) {
	// the identity is the one the sender signed in its bundle, never the one the packet claims
	if !bytes.Equal(init.IdentityKey[:], peerBundle.IdentityKey[:]) {
		return nil, ErrIdentityMismatch
	}
	idPrivEnc, _, err := x.accntManager.GetEncryptionKey()
	if err != nil {
		return nil, err
	}

	rootKeyEnc, err := calculateReceiver(idPrivEnc, spk, opk, peerBundle.IdentityKey, init.EphemeralKey, kem)
	if err != nil {
		return nil, err
	}

	newPrivEnc, _, err := generateSecureEphemeral()
	if err != nil {
		return nil, err
	}

	session, err := NewX3DHSession(rootKeyEnc, newPrivEnc, peerBundle.PeerID, init.EphemeralKey, false)
	if err != nil {
		return nil, err
	}
	session.AD = associatedData(peerBundle.IdentityKey, x.bundle.IdentityKey)
	session.InitEK = init.EphemeralKey
	session.PQ = kem != nil
	if session.InitMsgKey, _, err = kdfChain(session.RecvChain.Key); err != nil {
		return nil, err
	}
	return session, nil
}

// CalculateX3DH is the initiator's side: DH1 = DH(IK_A, SPK_B), DH2 = DH(EK_A, IK_B),
// DH3 = DH(EK_A, SPK_B), DH4 = DH(EK_A, OPK_B).
func CalculateX3DH(params X3DHParameters) (*memguard.Enclave, error) {
	dh1, err := dhSecure(params.MyIdentityPriv, params.RemoteSignedPrePub)
	if err != nil {
		return nil, err
	}

	dh2, err := dhSecure(params.MyEphemeralPriv, params.RemoteIdentityPub)
	if err != nil {
		memguard.WipeBytes(dh1)
		return nil, err
	}

	dh3, err := dhSecure(params.MyEphemeralPriv, params.RemoteSignedPrePub)
	if err != nil {
		memguard.WipeBytes(dh1)
		memguard.WipeBytes(dh2)
		return nil, err
	}

	var dh4 []byte
	if params.RemoteOneTimePub != nil {
		dh4, err = dhSecure(params.MyEphemeralPriv, *params.RemoteOneTimePub)
		if err != nil {
			memguard.WipeBytes(dh1)
			memguard.WipeBytes(dh2)
			memguard.WipeBytes(dh3)
			return nil, err
		}
	}

	return dhHKDF(dh1, dh2, dh3, dh4, params.KEM)
}

// CalculateX3DH_Receiver is the responder's side of classic X3DH.
func CalculateX3DH_Receiver(
	myIdentityPrivEnc *memguard.Enclave,
	mySignedPrePrivEnc *memguard.Enclave,
	myOneTimePrivEnc *memguard.Enclave,
	remoteIdentityPub [32]byte,
	remoteEphemeralPub [32]byte,
) (*memguard.Enclave, error) {
	return calculateReceiver(myIdentityPrivEnc, mySignedPrePrivEnc, myOneTimePrivEnc, remoteIdentityPub, remoteEphemeralPub, nil)
}

// calculateReceiver is the responder's side; kem is the KEM part of a PQXDH init, nil for classic.
func calculateReceiver(
	myIdentityPrivEnc *memguard.Enclave,
	mySignedPrePrivEnc *memguard.Enclave,
	myOneTimePrivEnc *memguard.Enclave,
	remoteIdentityPub [32]byte,
	remoteEphemeralPub [32]byte,
	kem *KEMInput,
) (*memguard.Enclave, error) {

	dh1, err := dhSecure(mySignedPrePrivEnc, remoteIdentityPub)
	if err != nil {
		return nil, err
	}

	dh2, err := dhSecure(myIdentityPrivEnc, remoteEphemeralPub)
	if err != nil {
		memguard.WipeBytes(dh1)
		return nil, err
	}

	dh3, err := dhSecure(mySignedPrePrivEnc, remoteEphemeralPub)
	if err != nil {
		memguard.WipeBytes(dh1)
		memguard.WipeBytes(dh2)
		return nil, err
	}

	var dh4 []byte
	if myOneTimePrivEnc != nil {
		dh4, err = dhSecure(myOneTimePrivEnc, remoteEphemeralPub)
		if err != nil {
			memguard.WipeBytes(dh1)
			memguard.WipeBytes(dh2)
			memguard.WipeBytes(dh3)
			return nil, err
		}
	}

	return dhHKDF(dh1, dh2, dh3, dh4, kem)
}

func dhSecure(privEnc *memguard.Enclave, pub [32]byte) ([]byte, error) {
	lockedPriv, err := privEnc.Open()
	if err != nil {
		return nil, ErrEnclaveOpenFailed
	}
	defer lockedPriv.Destroy()

	res, err := curve25519.X25519(lockedPriv.Bytes(), pub[:])
	if err != nil {
		return nil, ErrDHComputation
	}
	return res, nil
}

// dhHKDF: SK = HKDF(F || DH1 || DH2 || DH3 [|| DH4]) with F = 32 bytes of 0xFF, a zero salt and
// the protocol name as info, as the X3DH specification gives for X25519. With kem (PQXDH, as in
// Signal's PQXDH) the KEM shared secret follows the DH outputs, and the info is infoPQXDH || KEM
// key id || SHA-256(KEM ciphertext). The classic form is unchanged from v0.2.0.
func dhHKDF(dh1, dh2, dh3, dh4 []byte, kem *KEMInput) (*memguard.Enclave, error) {
	defer memguard.WipeBytes(dh1)
	defer memguard.WipeBytes(dh2)
	defer memguard.WipeBytes(dh3)
	if dh4 != nil {
		defer memguard.WipeBytes(dh4)
	}

	secretInputBuf := memguard.NewBuffer(32 * 6)
	defer secretInputBuf.Destroy()

	buf := secretInputBuf.Bytes()
	for i := 0; i < 32; i++ {
		buf[i] = 0xFF
	}
	idx := 32
	for _, dh := range [][]byte{dh1, dh2, dh3, dh4} {
		if dh != nil {
			copy(buf[idx:], dh)
			idx += 32
		}
	}

	info := []byte(DOMAIN_SEPARATION)
	if kem != nil {
		if len(kem.SharedSecret) != mlkem.SharedKeySize {
			return nil, ErrBadKEMInit
		}
		idx += copy(buf[idx:], kem.SharedSecret)
		ctHash := sha256.Sum256(kem.Ciphertext)
		info = append(append([]byte(infoPQXDH), kem.KeyID[:]...), ctHash[:]...)
	}

	salt := make([]byte, 32)
	kdf := hkdf.New(sha256.New, buf[:idx], salt, info)

	rootKeyBuf := memguard.NewBuffer(32)
	defer rootKeyBuf.Destroy()

	if _, err := io.ReadFull(kdf, rootKeyBuf.Bytes()); err != nil {
		return nil, ErrHKDFFailed
	}

	return rootKeyBuf.Seal(), nil
}

func generateSecureEphemeral() (*memguard.Enclave, [32]byte, error) {
	privBuf := memguard.NewBuffer(32)
	defer privBuf.Destroy()

	if _, err := rand.Read(privBuf.Bytes()); err != nil {
		return nil, [32]byte{}, ErrEphemeralGenFailed
	}

	var xPriv [32]byte
	copy(xPriv[:], privBuf.Bytes())
	defer memguard.WipeBytes(xPriv[:])

	var pubKey [32]byte
	curve25519.ScalarBaseMult(&pubKey, &xPriv)

	return privBuf.Seal(), pubKey, nil
}
