package call

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
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

// noteKXLocked keeps a member's key exchange key from its join or heartbeat; a member that changes
// it gets a new media key, as it may have restarted and lost the one it had.
func (m *CallManager) noteKXLocked(room *ActiveRoomState, peer types.PeerID, kx []byte, now time.Time) {
	if len(kx) != 32 || peer == m.selfID {
		return
	}
	if old, ok := room.memberKX[peer]; ok && string(old) == string(kx) {
		return
	}
	room.memberKX[peer] = append([]byte(nil), kx...)
	m.scheduleRekeyLocked(room, now)
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
	if missing == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return
		}
	}
	room.keyEpoch++
	for p := range room.ConnectedPeers {
		kx, ok := room.memberKX[p]
		if p == m.selfID || !ok {
			continue
		}
		mk, err := sealMediaKey(key, kx, room.CallID, p, room.keyEpoch)
		if err != nil {
			continue
		}
		m.sendLocked(room, p, signaling.ActionMediaKey, mk)
	}
	_ = room.Dispatcher.SetMediaKey(key, now)
	m.logger.Info("New call media key", "call_id", calllog.Call(room.CallID), "key_epoch", room.keyEpoch,
		"members", others+1, "older_nodes", missing)
}

// takeMediaKeyLocked applies a media key the coordinator sent this node.
func (m *CallManager) takeMediaKeyLocked(room *ActiveRoomState, from types.PeerID, p signaling.MediaKeyPayload, now time.Time) {
	if from != room.Coordinator || room.IsCoordinator || p.KeyEpoch <= room.keyEpoch && from == room.keyFrom {
		return
	}
	if m.olderNode {
		return
	}
	key, err := openMediaKey(p, room.kxPriv, room.CallID, m.selfID)
	if err != nil {
		m.warnOnce("mediakey:"+from.String(), "Call media key did not open", "coordinator", calllog.Peer(from))
		return
	}
	room.keyEpoch, room.keyFrom = p.KeyEpoch, from
	_ = room.Dispatcher.SetMediaKey(key, now)
}
