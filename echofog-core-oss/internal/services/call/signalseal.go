package call

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"

	"github.com/echofog/echofog-core/pkg/api/types"
)

// Call signals end to end. A signal is signed JSON, readable by every relay circuit it crosses:
// who calls whom, the shape of the trees, who speaks when (vad_state), the streams and their rates.
// Between two members that both announced a key for it (PeerNodeProfile.SignalKX, the member's
// X25519 key for the call), a signal goes sealed instead: the signed envelope, byte for byte, under
// AES-GCM keyed from X25519 between the two members' keys. Only the two ends read it; a flooded
// signal is opened and sealed again on every hop, so its signature still covers the bytes its
// originator signed. A member that announced no key (0.2.0 and earlier) gets the plain envelope.
//
// Wire form: sealedMagic, the sender's key (32), a random nonce (12), the sealed envelope. JSON
// never starts with a zero byte, so a receiver tells the two apart by the first bytes.

var sealedMagic = []byte{0, 'E', 'F', 's', 1}

const sealedHeaderLen = 5 + 32 + 12

var errBadSealedSignal = errors.New("sealed call signal does not open")

// signalLink is the AEAD of one member's link, cached with the key it was made for.
type signalLink struct {
	peerKX string
	aead   cipher.AEAD
}

func isSealedSignal(b []byte) bool { return bytes.HasPrefix(b, sealedMagic) }

// peerSignalKXLocked is the key a peer takes sealed signals under, or nil if it announced none:
// the profile its join carried (coordinator) or the voice manifest gave it.
func (m *CallManager) peerSignalKXLocked(room *ActiveRoomState, peer types.PeerID) []byte {
	if p, ok := room.ConnectedPeers[peer]; ok && len(p.SignalKX) == 32 {
		return p.SignalKX
	}
	if mf := room.VoiceManifest; mf != nil {
		if p, ok := mf.Profiles[peer]; ok && len(p.SignalKX) == 32 {
			return p.SignalKX
		}
	}
	return nil
}

// linkAEADLocked is the AEAD between this node and peer, whose key is peerKX.
func (m *CallManager) linkAEADLocked(room *ActiveRoomState, peer types.PeerID, peerKX []byte) (cipher.AEAD, error) {
	if l, ok := room.signalLinks[peer]; ok && l.peerKX == string(peerKX) {
		return l.aead, nil
	}
	pub, err := ecdh.X25519().NewPublicKey(peerKX)
	if err != nil {
		return nil, err
	}
	shared, err := room.kxPriv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	lo, hi := m.selfID, peer
	if bytes.Compare(lo[:], hi[:]) > 0 {
		lo, hi = hi, lo
	}
	info := append(append(append([]byte("echofog-call-signal\x00"), room.CallID...), lo[:]...), hi[:]...)
	k, err := hkdf.Key(sha256.New, shared, nil, string(info), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if room.signalLinks == nil {
		room.signalLinks = make(map[types.PeerID]signalLink)
	}
	room.signalLinks[peer] = signalLink{peerKX: string(peerKX), aead: aead}
	return aead, nil
}

// sealSignalLocked returns a signal for to: sealed when both ends announced a key, as it is otherwise.
func (m *CallManager) sealSignalLocked(room *ActiveRoomState, to types.PeerID, b []byte) []byte {
	if m.plainSignals {
		return b
	}
	own := m.signalKXLocked(room)
	peerKX := m.peerSignalKXLocked(room, to)
	if own == nil || peerKX == nil {
		return b
	}
	aead, err := m.linkAEADLocked(room, to, peerKX)
	if err != nil {
		return b
	}
	out := make([]byte, sealedHeaderLen, sealedHeaderLen+len(b)+aead.Overhead())
	copy(out, sealedMagic)
	copy(out[5:37], own)
	if _, err := rand.Read(out[37:sealedHeaderLen]); err != nil {
		return b
	}
	return aead.Seal(out, out[37:sealedHeaderLen], b, linkAAD(m.selfID, to))
}

// openSignalLocked opens a sealed signal from a peer and returns the envelope inside.
func (m *CallManager) openSignalLocked(room *ActiveRoomState, from types.PeerID, b []byte) ([]byte, error) {
	if room == nil || m.signalKXLocked(room) == nil || len(b) < sealedHeaderLen {
		return nil, errBadSealedSignal
	}
	aead, err := m.linkAEADLocked(room, from, b[5:37])
	if err != nil {
		return nil, errBadSealedSignal
	}
	inner, err := aead.Open(nil, b[37:sealedHeaderLen], b[sealedHeaderLen:], linkAAD(from, m.selfID))
	if err != nil || isSealedSignal(inner) {
		return nil, errBadSealedSignal
	}
	return inner, nil
}

// signalKXLocked is this node's key for sealed signals, nil when it takes none.
func (m *CallManager) signalKXLocked(room *ActiveRoomState) []byte {
	if room == nil || room.kxPriv == nil || m.olderNode {
		return nil
	}
	return room.kxPriv.PublicKey().Bytes()
}

// linkAAD binds a sealed signal to its direction, so it cannot be reflected back to its sender.
func linkAAD(from, to types.PeerID) []byte {
	return append(append(make([]byte, 0, 64), from[:]...), to[:]...)
}
