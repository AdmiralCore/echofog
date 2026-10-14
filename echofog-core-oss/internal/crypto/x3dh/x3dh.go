package x3dh

import (
	"bytes"
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
}

// associatedData binds a session to both identities, initiator first (X3DH: AD = IK_A || IK_B).
func associatedData(initiatorIK, responderIK [32]byte) []byte {
	return append(append([]byte(nil), initiatorIK[:]...), responderIK[:]...)
}

// X3DHInit starts a session with the owner of peerBundle, which the caller has verified. When the
// bundle offers one-time prekeys, one of them is picked at random.
func (x *X3DHEngine) X3DHInit(peerBundle keymanager.PublicBundle) (*X3DHSession, types.X3DHInit, error) {
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
	}

	rootKeyEnc, err := CalculateX3DH(params)
	if err != nil {
		return nil, types.X3DHInit{}, err
	}

	initPacket := types.X3DHInit{
		EphemeralKey: empPub,
		IdentityKey:  x.bundle.IdentityKey,
		OneTimeKeyID: remoteOPK,
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
	return x3dhSession, initPacket, nil
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
	return x.receive(init, peerBundle, spk, opk)
}

// Accept answers an init packet from the owner of peerBundle (verified by the caller) and opens the
// message it carries. It tries the current signed prekey and the replaced ones still in use, and
// uses up the one-time prekey only when the message opened.
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
	spks, err := x.accntManager.SignedPreKeys()
	if err != nil {
		return nil, nil, err
	}
	for _, spk := range spks {
		session, err := x.receive(init, peerBundle, spk, opk)
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

func (x *X3DHEngine) receive(init types.X3DHInit, peerBundle keymanager.PublicBundle, spk, opk *memguard.Enclave) (*X3DHSession, error) {
	// the identity is the one the sender signed in its bundle, never the one the packet claims
	if !bytes.Equal(init.IdentityKey[:], peerBundle.IdentityKey[:]) {
		return nil, ErrIdentityMismatch
	}
	idPrivEnc, _, err := x.accntManager.GetEncryptionKey()
	if err != nil {
		return nil, err
	}

	rootKeyEnc, err := CalculateX3DH_Receiver(idPrivEnc, spk, opk, peerBundle.IdentityKey, init.EphemeralKey)
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

	return dhHKDF(dh1, dh2, dh3, dh4)
}

func CalculateX3DH_Receiver(
	myIdentityPrivEnc *memguard.Enclave,
	mySignedPrePrivEnc *memguard.Enclave,
	myOneTimePrivEnc *memguard.Enclave,
	remoteIdentityPub [32]byte,
	remoteEphemeralPub [32]byte,
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

	return dhHKDF(dh1, dh2, dh3, dh4)
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
// the protocol name as info, as the X3DH specification gives for X25519.
func dhHKDF(dh1, dh2, dh3, dh4 []byte) (*memguard.Enclave, error) {
	defer memguard.WipeBytes(dh1)
	defer memguard.WipeBytes(dh2)
	defer memguard.WipeBytes(dh3)
	if dh4 != nil {
		defer memguard.WipeBytes(dh4)
	}

	secretInputBuf := memguard.NewBuffer(32 * 5)
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

	salt := make([]byte, 32)
	kdf := hkdf.New(sha256.New, buf[:idx], salt, []byte(DOMAIN_SEPARATION))

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
