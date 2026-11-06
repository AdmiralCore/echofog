package chatcrypto

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/crypto/x3dh"
	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	objpb "github.com/echofog/echofog-core/internal/v2/objects/pb"
	"golang.org/x/crypto/curve25519"
)

// rawSender is a sender built straight on the v1 engine, independent of Crypto, so that a test can
// choose the labels and build inits the real Start never makes (a classic one).
type rawSender struct {
	tw     *identity.Throwaway
	dl     *objects.DeviceList
	engine *x3dh.X3DHEngine
	ikPub  [32]byte
}

type fixedIdentity struct {
	enc *memguard.Enclave
	pub []byte
}

func (f fixedIdentity) GetEncryptionKey() (*memguard.Enclave, []byte, error) {
	return f.enc, f.pub, nil
}

func newRawSender(t *testing.T, clock *testClock, chat2 bool) *rawSender {
	t.Helper()
	tw, err := identity.NewThrowaway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Destroy)
	rawDL, err := tw.DeviceList(1, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	dl, err := objects.VerifyDeviceList(rawDL, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	var priv, pub [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		t.Fatal(err)
	}
	curve25519.ScalarBaseMult(&pub, &priv)
	labels := x3dh.V1Labels
	if chat2 {
		labels = Labels
	}
	own := &keymanager.PublicBundle{PeerID: tw.Device.ID(), IdentityKey: pub}
	eng := x3dh.NewX3DHEngineWithConfig(own, newMem(), nil, x3dh.Config{
		Labels:   labels,
		Identity: fixedIdentity{enc: memguard.NewEnclave(append([]byte(nil), priv[:]...)), pub: pub[:]},
	})
	return &rawSender{tw: tw, dl: dl, engine: eng, ikPub: pub}
}

func (s *rawSender) init(t *testing.T, bundle keymanager.PublicBundle, plain []byte) []byte {
	t.Helper()
	sess, pkt, err := s.engine.X3DHInit(bundle)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := sess.RatchetEncrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	m := &chatpb.X3DHInit{
		EphemeralKey: pkt.EphemeralKey[:],
		IdentityKey:  pkt.IdentityKey[:],
		InitialCiphertext: &chatpb.SecureMessage{
			Header:     &chatpb.RatchetHeader{RatchetKey: msg.Header.DH[:], Pn: msg.Header.PN, N: msg.Header.N},
			Ciphertext: msg.Ciphertext,
		},
		KemCiphertext: pkt.KEMCiphertext,
	}
	if pkt.OneTimeKeyID != nil {
		m.OneTimeKeyId = pkt.OneTimeKeyID[:]
	}
	if pkt.KEMKeyID != nil {
		m.KemKeyId = pkt.KEMKeyID[:]
	}
	out, err := objects.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *rawSender) start(t *testing.T, peer *objects.Prekeys, plain []byte) []byte {
	t.Helper()
	return s.init(t, publicBundle(peer), plain)
}

func (s *rawSender) startClassic(t *testing.T, peer *objects.Prekeys, plain []byte) []byte {
	t.Helper()
	b := publicBundle(peer)
	b.KEMPreKey = nil
	return s.init(t, b, plain)
}

// prekeys is the sender's own prekeys/2, with the identity key of the engine.
func (s *rawSender) prekeys(t *testing.T, clock *testClock) *objects.Prekeys {
	t.Helper()
	spk, kem := make([]byte, 32), make([]byte, 1184)
	_, _ = rand.Read(spk)
	_, _ = rand.Read(kem)
	now := uint64(clock.Now().Unix())
	body, err := objects.Marshal(&objpb.Prekeys{
		Ik: keys.New(keys.X25519, s.ikPub[:]), Spk: spk, SpkCreated: now, Kem: kem, KemCreated: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	err = s.tw.Device.WithSigner(func(sg *keys.Signer) error {
		var err error
		raw, err = objects.Seal(objects.LabelPrekeys, sg, now, now+uint64((30*24*time.Hour).Seconds()), body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := objects.VerifyPrekeys(raw, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
