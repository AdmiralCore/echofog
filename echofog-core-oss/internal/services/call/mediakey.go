package call

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// Media key rotation. Everybody starts with the room key, derived from the invite. When the members
// change, the coordinator makes a new random media key and hands it to each member still in the
// call, sealed to the X25519 key that member's join carried; whoever left (or was voted out) never
// gets it, so what is said from then on is closed to them although they still hold the invite.
// Units carry no key ID: a receiver tries the newest key, the one before and the room key.
//
// A node of an older version sends no key exchange key and cannot take a new key. While one is in
// the call the coordinator hands out the room key itself as the new key, so everybody is back on
// what the older node speaks: the call works, without the protection, as before rotation.
//
// Each member also sends a fresh Ed25519 key for the call with its join. When every member the new
// key goes to has one, the coordinator names them all with the key, and every unit sealed under it
// ends with its sender's signature: a member can seal under the shared key as well as anybody, but
// cannot pass a unit off as another's. Units under the room key, or under a key some member could
// not sign for, carry no signature, as before.

const (
	// rekeyDelay gathers the joins and leaves that come together into one new key.
	rekeyDelay = time.Second
	// kxWait is how long a coordinator that took over waits for the members' key exchange keys
	// (their heartbeats carry them) before treating a member without one as an older node.
	kxWait = 10 * time.Second
)

var errBadMediaKey = errors.New("call media key does not open")

// mediaKeyAEAD keys the seal of one media key: X25519 between the coordinator's ephemeral key and
// the member's key exchange key, through HKDF bound to the call, the member and the key epoch.
func mediaKeyAEAD(shared []byte, callID string, member types.PeerID, epoch uint64) (cipher.AEAD, error) {
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], epoch)
	info := append(append(append([]byte("echofog-call-media-key\x00"), callID...), member[:]...), e[:]...)
	k, err := hkdf.Key(sha256.New, shared, nil, string(info), 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealMediaKey seals key for member, whose key exchange key is kx.
func sealMediaKey(key, kx []byte, callID string, member types.PeerID, epoch uint64) (signaling.MediaKeyPayload, error) {
	pub, err := ecdh.X25519().NewPublicKey(kx)
	if err != nil {
		return signaling.MediaKeyPayload{}, err
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return signaling.MediaKeyPayload{}, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return signaling.MediaKeyPayload{}, err
	}
	aead, err := mediaKeyAEAD(shared, callID, member, epoch)
	if err != nil {
		return signaling.MediaKeyPayload{}, err
	}
	nonce := make([]byte, aead.NonceSize()) // the AEAD key is used once: the ephemeral key is fresh
	return signaling.MediaKeyPayload{KeyEpoch: epoch, Eph: eph.PublicKey().Bytes(), Sealed: aead.Seal(nil, nonce, key, nil)}, nil
}

// openMediaKey opens a media key sealed to priv for self.
func openMediaKey(p signaling.MediaKeyPayload, priv *ecdh.PrivateKey, callID string, self types.PeerID) ([]byte, error) {
	if priv == nil {
		return nil, errBadMediaKey
	}
	eph, err := ecdh.X25519().NewPublicKey(p.Eph)
	if err != nil {
		return nil, errBadMediaKey
	}
	shared, err := priv.ECDH(eph)
	if err != nil {
		return nil, errBadMediaKey
	}
	aead, err := mediaKeyAEAD(shared, callID, self, p.KeyEpoch)
	if err != nil {
		return nil, err
	}
	key, err := aead.Open(nil, make([]byte, aead.NonceSize()), p.Sealed, nil)
	if err != nil || len(key) != 32 {
		return nil, errBadMediaKey
	}
	return key, nil
}

// kxPublicLocked is this node's key exchange key for the call, for its joins and heartbeats.
func (m *CallManager) kxPublicLocked(room *ActiveRoomState) []byte {
	if room.kxPriv == nil || m.olderNode {
		return nil
	}
	return room.kxPriv.PublicKey().Bytes()
}

// signPublicLocked is this node's unit signing key for the call, for its joins and heartbeats.
func (m *CallManager) signPublicLocked(room *ActiveRoomState) []byte {
	if room.signPriv == nil || m.olderNode || m.unsignedNode {
		return nil
	}
	return room.signPriv.Public().(ed25519.PublicKey)
}

// noteKXLocked keeps a member's key exchange and signing keys from its join or heartbeat; a member
// that changes one gets a new media key, as it may have restarted and lost the ones it had.
func (m *CallManager) noteKXLocked(room *ActiveRoomState, peer types.PeerID, kx, sk []byte, now time.Time) {
	if len(kx) != 32 || peer == m.selfID {
		return
	}
	changed := false
	if old, ok := room.memberKX[peer]; !ok || string(old) != string(kx) {
		room.memberKX[peer] = append([]byte(nil), kx...)
		changed = true
	}
	if len(sk) == ed25519.PublicKeySize {
		if old, ok := room.memberSK[peer]; !ok || string(old) != string(sk) {
			room.memberSK[peer] = append([]byte(nil), sk...)
			changed = true
		}
	}
	if changed {
		m.scheduleRekeyLocked(room, now)
	}
}

// scheduleRekeyLocked asks for a new media key soon (coordinator only).
func (m *CallManager) scheduleRekeyLocked(room *ActiveRoomState, now time.Time) {
	if !room.IsCoordinator {
		return
	}
	if room.rekeyAt.IsZero() || room.rekeyAt.After(now.Add(rekeyDelay)) {
		room.rekeyAt = now.Add(rekeyDelay)
	}
}

// keyResendEvery is how often the coordinator repeats a key a member has not confirmed yet: the
// path that broke while the key was in flight is often back a moment later.
const keyResendEvery = 500 * time.Millisecond

// rekeyTickLocked hands out a new media key once one is due (coordinator only).
func (m *CallManager) rekeyTickLocked(room *ActiveRoomState, now time.Time) {
	if !room.IsCoordinator || room.rekeyAt.IsZero() || now.Before(room.rekeyAt) {
		return
	}
	others, missing := 0, 0
	for p := range room.ConnectedPeers {
		if p == m.selfID {
			continue
		}
		others++
		if _, ok := room.memberKX[p]; !ok {
			missing++
		}
	}
	if missing > 0 && now.Sub(room.coordSince) < kxWait {
		room.rekeyAt = now.Add(rekeyDelay) // a coordinator that took over hears the keys with the heartbeats
		return
	}
	room.rekeyAt = time.Time{}
	if others == 0 {
		return // alone: nobody to keep anything from yet; the next member brings a new key
	}
	key := room.RoomKey // an older node is in the call: everybody back to what it speaks
	var signers map[types.PeerID][]byte
	if missing == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return
		}
		signers = m.signersLocked(room)
	}
	room.keyEpoch++
	room.prevMediaKey, room.prevMediaKeyEpoch = room.mediaKey, room.mediaKeyEpoch
	room.mediaKey, room.mediaKeyEpoch = append([]byte(nil), key...), room.keyEpoch
	room.mediaKeySigners = signers
	room.keyWait = make(map[types.PeerID]struct{})
	room.keyWaitEpoch, room.keyWaitSince = room.keyEpoch, now
	for p := range room.ConnectedPeers {
		kx, ok := room.memberKX[p]
		if p == m.selfID || !ok {
			continue
		}
		mk, err := sealMediaKey(key, kx, room.CallID, p, room.keyEpoch)
		if err != nil {
			continue
		}
		mk.Signers = signers
		m.sendLocked(room, p, signaling.ActionMediaKey, mk)
		room.keyWait[p] = struct{}{} // until the member confirms it (see keyWaitTickLocked)
	}
	_ = room.Dispatcher.SetMediaKey(key, signerKeys(signers), now)
	m.logger.Info("New call media key", "call_id", calllog.Call(room.CallID), "key_epoch", room.keyEpoch,
		"members", others+1, "older_nodes", missing, "signed", signers != nil)
}

// keyWaitTickLocked repeats the newest key to the members that have not confirmed it and keeps
// units on the key they do have meanwhile. A key handed out once can be lost on the link that had
// just come back; without this the member went without it to the end of the call.
func (m *CallManager) keyWaitTickLocked(room *ActiveRoomState, now time.Time) {
	if len(room.keyWait) == 0 {
		return
	}
	if now.Sub(room.keyWaitSince) >= kxWait {
		room.keyWait = nil
		m.releaseKeySwitchLocked(room, now)
		m.logger.Warn("Call media key not confirmed by every member, sending with it anyway",
			"call_id", calllog.Call(room.CallID), "key_epoch", room.keyWaitEpoch)
		return
	}
	if now.Sub(room.lastKeyResend) >= keyResendEvery {
		room.lastKeyResend = now
		m.resendMediaKeyLocked(room)
	}
	if room.Dispatcher != nil {
		room.Dispatcher.PostponeMediaKeySwitch(now)
	}
}

// resendMediaKeyLocked repeats the newest key to the members that have not confirmed it.
func (m *CallManager) resendMediaKeyLocked(room *ActiveRoomState) {
	if len(room.mediaKey) != 32 {
		return
	}
	for p := range room.keyWait {
		kx, ok := room.memberKX[p]
		if !ok {
			continue
		}
		mk, err := sealMediaKey(room.mediaKey, kx, room.CallID, p, room.mediaKeyEpoch)
		if err != nil {
			continue
		}
		mk.Signers = room.mediaKeySigners
		m.sendLocked(room, p, signaling.ActionMediaKey, mk)
	}
}

// handleMediaKeyAckLocked takes a member's confirmation of the newest key, and lets units go out
// with it once every member has it.
func (m *CallManager) handleMediaKeyAckLocked(room *ActiveRoomState, from types.PeerID, epoch uint64) {
	if !room.IsCoordinator || len(room.keyWait) == 0 || epoch != room.keyWaitEpoch {
		return
	}
	delete(room.keyWait, from)
	if len(room.keyWait) == 0 {
		m.releaseKeySwitchLocked(room, m.now())
		m.logger.Info("New call media key confirmed by every member", "call_id", calllog.Call(room.CallID),
			"key_epoch", room.keyWaitEpoch)
	}
}

// releaseKeySwitchLocked lets units go out with the newest key now.
func (m *CallManager) releaseKeySwitchLocked(room *ActiveRoomState, now time.Time) {
	if room.Dispatcher != nil {
		room.Dispatcher.ReleaseMediaKeySwitch(now)
	}
}

// signersLocked names the signing key of every member, this node included, or nil if one of them
// sent none (a build from before unit signatures): then units under the key go unsigned.
func (m *CallManager) signersLocked(room *ActiveRoomState) map[types.PeerID][]byte {
	own := m.signPublicLocked(room)
	if own == nil {
		return nil
	}
	out := map[types.PeerID][]byte{m.selfID: own}
	for p := range room.ConnectedPeers {
		if p == m.selfID {
			continue
		}
		sk, ok := room.memberSK[p]
		if !ok {
			return nil
		}
		out[p] = sk
	}
	return out
}

// signerKeys turns the signers of a media key payload into the dispatcher's form; nil stays nil.
func signerKeys(in map[types.PeerID][]byte) map[types.PeerID]ed25519.PublicKey {
	if in == nil {
		return nil
	}
	out := make(map[types.PeerID]ed25519.PublicKey, len(in))
	for id, k := range in {
		out[id] = k
	}
	return out
}

// takeMediaKeyLocked applies a media key the coordinator sent this node.
func (m *CallManager) takeMediaKeyLocked(room *ActiveRoomState, from types.PeerID, p signaling.MediaKeyPayload, now time.Time) {
	if from == m.selfID || m.olderNode {
		return
	}
	if room.IsCoordinator {
		// We lead the call, or think we do: a key from another claimant is not used while we
		// lead. Keep the newest one anyway: the claimant may be the coordinator the others kept,
		// and when we yield to it the room's key must not be lost with our mistaken takeover.
		if len(p.Eph) == 0 || len(p.Sealed) == 0 {
			return
		}
		if room.pendingKeys == nil {
			room.pendingKeys = make(map[types.PeerID]pendingMediaKey)
		}
		for id, e := range room.pendingKeys {
			if now.Sub(e.at) > pendingKeyTTL {
				delete(room.pendingKeys, id) // a claimant we never followed: its key goes with it
			}
		}
		if _, known := room.pendingKeys[from]; !known && len(room.pendingKeys) >= maxPendingMediaKeys {
			return
		}
		if old, ok := room.pendingKeys[from]; !ok || p.KeyEpoch >= old.payload.KeyEpoch {
			room.pendingKeys[from] = pendingMediaKey{payload: p, at: now}
		}
		return
	}
	if from != room.Coordinator {
		return
	}
	if p.KeyEpoch <= room.keyEpoch && from == room.keyFrom {
		m.ackMediaKeyLocked(room, from, p.KeyEpoch) // a repeat: the first confirmation may be lost
		return
	}
	if m.installMediaKeyLocked(room, from, p, now) {
		m.ackMediaKeyLocked(room, from, p.KeyEpoch)
	}
}

// maxPendingMediaKeys bounds the keys kept from coordinators we do not follow yet.
const maxPendingMediaKeys = 16

// pendingKeyTTL bounds how long a key from a coordinator we do not follow yet is kept: one that
// never becomes our coordinator is forgotten with its key.
const pendingKeyTTL = time.Minute

// pendingMediaKey is a key handed out by a claimant we do not follow (yet) and when it came.
type pendingMediaKey struct {
	payload signaling.MediaKeyPayload
	at      time.Time
}

// ackMediaKeyLocked tells the coordinator we took the key of epoch.
func (m *CallManager) ackMediaKeyLocked(room *ActiveRoomState, to types.PeerID, epoch uint64) {
	if room.IsCoordinator || to != room.Coordinator || epoch == 0 {
		return
	}
	m.sendLocked(room, to, signaling.ActionMediaKeyAck, signaling.KeyAckPayload{Epoch: epoch})
}

// installMediaKeyLocked opens a media key coordinator from handed out and starts the room on it.
func (m *CallManager) installMediaKeyLocked(room *ActiveRoomState, from types.PeerID, p signaling.MediaKeyPayload, now time.Time) bool {
	key, err := openMediaKey(p, room.kxPriv, room.CallID, m.selfID)
	if err != nil {
		m.warnOnce("mediakey:"+from.String(), "Call media key did not open", "coordinator", calllog.Peer(from))
		return false
	}
	room.keyEpoch, room.keyFrom = p.KeyEpoch, from
	_ = room.Dispatcher.SetMediaKey(key, signerKeys(p.Signers), now)
	return true
}

// adoptPendingMediaKeyLocked takes the key leader sent while we still thought we led the call,
// now that we follow it: the others speak it already, and our own would leave us unheard.
func (m *CallManager) adoptPendingMediaKeyLocked(room *ActiveRoomState, leader types.PeerID, now time.Time) {
	p, ok := room.pendingKeys[leader]
	if !ok {
		return
	}
	delete(room.pendingKeys, leader)
	if now.Sub(p.at) > pendingKeyTTL {
		return
	}
	if m.installMediaKeyLocked(room, leader, p.payload, now) {
		m.ackMediaKeyLocked(room, leader, p.payload.KeyEpoch)
	}
}

// sendRoomKeysLocked hands a member taken in the keys it needs to open what reaches it at once:
// the newest key, and while other members confirm it, the one media still goes out with.
func (m *CallManager) sendRoomKeysLocked(room *ActiveRoomState, peer types.PeerID, now time.Time) {
	if !room.IsCoordinator {
		return
	}
	kx, ok := room.memberKX[peer]
	if !ok || len(kx) != 32 {
		return
	}
	if len(room.keyWait) > 0 && len(room.prevMediaKey) == 32 &&
		room.prevMediaKeyEpoch != 0 && room.prevMediaKeyEpoch != room.mediaKeyEpoch {
		if mk, err := sealMediaKey(room.prevMediaKey, kx, room.CallID, peer, room.prevMediaKeyEpoch); err == nil {
			m.sendLocked(room, peer, signaling.ActionMediaKey, mk)
		}
	}
	if len(room.mediaKey) == 32 {
		if mk, err := sealMediaKey(room.mediaKey, kx, room.CallID, peer, room.mediaKeyEpoch); err == nil {
			mk.Signers = room.mediaKeySigners
			m.sendLocked(room, peer, signaling.ActionMediaKey, mk)
		}
	}
}
