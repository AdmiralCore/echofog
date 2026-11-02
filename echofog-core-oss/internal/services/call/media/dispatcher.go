package media

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/videolayer"
)

// LocalMediaConsumer receives decrypted media for playback in UI / WebRTC.
type LocalMediaConsumer interface {
	OnLocalAudio(packet *MediaPacket)
	OnLocalVideo(packet *MediaPacket)
}

const (
	voiceStreamID = "voice"
	// PulseInterval is how long a stream link may stay idle before the feeder sends a pulse.
	PulseInterval    = 250 * time.Millisecond
	housekeepingTick = 100 * time.Millisecond
)

type linkKey struct {
	streamID string
	feeder   types.PeerID
}

// DispatcherStats is a snapshot of the media counters, used by telemetry and tests.
type DispatcherStats struct {
	AudioDelivered   uint64
	VideoDelivered   uint64
	FragmentsRelayed uint64
	Duplicates       uint64
	DecryptFailures  uint64
	Forged           uint64 // units that opened but were not signed by the member they name
	Malformed        uint64
	IncompleteUnits  uint64
	Retransmitted    uint64 // fragments resent to children from the retransmit ring
	NackedUnits      uint64 // lost units this node asked its parent to resend
	Recovered        uint64 // fragments rebuilt from FEC parity
	Outbox           OutboxStats
}

// StreamCounters is what one stream did on this node, for the summary logged when a call ends.
type StreamCounters struct {
	UnitsSent      uint64 // units this node broadcast on its own stream
	Delivered      uint64 // video units decrypted and played here
	Lost           uint64 // gaps that outlived the reorder window (each costs a keyframe request)
	Nacked         uint64 // lost units this node asked its parent to resend
	Retransmitted  uint64 // fragments resent to children from the ring
	Recovered      uint64 // lost fragments rebuilt here from FEC parity, without asking anybody
	SoundSent      uint64 // units of the stream's sound this node broadcast (a shared screen's audio)
	SoundDelivered uint64 // units of the stream's sound decrypted and played here
}

// MediaDispatcher forwards media fragments along the call trees and delivers decrypted units
// to the local consumer. Every hop opens a unit before it relays it (see authenticate), so
// nothing forged travels further than the first member it reaches.
type MediaDispatcher struct {
	selfID       types.PeerID
	outbox       *Outbox
	consumer     LocalMediaConsumer
	streamEngine *tree.StreamTreeEngine

	mu      sync.RWMutex
	roomKey []byte // the invite's key: what everybody starts with, always accepted
	// The key the coordinator handed out last (cur), sent with from curFrom on, and the one before
	// (prev), still sent until then and accepted while late units arrive. See SetMediaKey.
	cur, prev        mediaKey
	curFrom          time.Time
	signKey          ed25519.PrivateKey // this node's signing key for the call, see SetSigningKey
	voiceManifest    *tree.TreeManifest
	inCall           map[types.PeerID]bool // every peer the voice tree names, built with it
	upstreamOverride types.PeerID
	adopted          map[types.PeerID]bool
	watchingStreams  map[string]bool
	videoSeq         map[string]*atomic.Uint32
	streamAudioSeq   map[string]*atomic.Uint32
	lastDelivered    map[sourceKey]uint32
	missing          map[sourceKey]map[uint32]*gapGroup // seqs skipped over, waiting out the reorder window
	lastForward      map[string]time.Time               // stream -> last time we sent anything to its children
	lastFromFeeder   map[linkKey]time.Time              // (stream, feeder) -> last frame received from it
	watchSince       map[linkKey]time.Time              // (stream, feeder) -> first time we waited on it
	nackAsked        map[ringKey]nackAsk                // (stream, seq) -> when we asked upstream
	rtt              map[string]time.Duration           // stream -> smoothed time a NACK takes to be answered
	keyWanted        map[string]time.Time               // stream -> when we asked for a keyframe that has not come yet
	streamStats      map[string]*StreamCounters
	feeders          map[string]feederState         // stream -> the peer the tree feeds us from, see fromFeeder
	resend           map[types.PeerID]*resendBudget // child -> what it may still pull from the ring

	// temporal layers, see layers.go
	filters    map[linkKey]*childFilter // (stream, child) -> the layers it gets
	layerTop   map[string]layerTop      // stream -> the highest layer step seen lately
	caps       map[sourceKey]*capTrack  // the caps a stream came here with
	renum      map[sourceKey]*capTrack  // ... and how its delivered units are renumbered
	lastLayer  map[sourceKey]unitLayer  // the layer of the newest unit delivered
	layerCount map[string]*[2]uint16    // local stream -> packets of layer 0, and of layers 0-1, sent

	audioSeq atomic.Uint32
	dedup    *dedupTable
	reasm    *reassembler
	ring     *fragRing
	fecTx    *fecSender
	fecRx    *fecReceiver
	rx       rxMeters // what the path of each watched stream does, for the broadcaster's rate
	repeat   audioRepeat

	// OnVideoNack fires shortly after a watched stream has a gap; the call layer sends the NACK
	// to the stream's parent, which resends from its ring.
	OnVideoNack func(streamID string, lost []NackEntry)
	// OnVideoLoss fires when a gap outlives the reorder window despite NACKs; the call layer turns
	// it into a keyframe request.
	OnVideoLoss func(streamID string, broadcaster types.PeerID)

	audioDelivered, videoDelivered, relayed, duplicates, decryptFailures, malformed, incomplete atomic.Uint64
	forged                                                                                      atomic.Uint64 // opened, but not signed by the member it names
	outsiders                                                                                   atomic.Uint64 // media from a peer outside the call, dropped
	retransmitted, nacked, recovered                                                            atomic.Uint64

	// Per-packet problems (garbage from a peer, failed decryption, lost units) are logged through
	// lim, one line per peer or stream and reason every few seconds.
	log *slog.Logger
	lim *calllog.Limiter

	stop chan struct{}
	once sync.Once
}

// NewMediaDispatcher initializes a dispatcher; sender may be nil for offline tests.
func NewMediaDispatcher(selfID types.PeerID, sender FrameSender, consumer LocalMediaConsumer, engine *tree.StreamTreeEngine) *MediaDispatcher {
	d := &MediaDispatcher{
		selfID:          selfID,
		consumer:        consumer,
		streamEngine:    engine,
		adopted:         make(map[types.PeerID]bool),
		watchingStreams: make(map[string]bool),
		videoSeq:        make(map[string]*atomic.Uint32),
		streamAudioSeq:  make(map[string]*atomic.Uint32),
		lastDelivered:   make(map[sourceKey]uint32),
		missing:         make(map[sourceKey]map[uint32]*gapGroup),
		lastForward:     make(map[string]time.Time),
		lastFromFeeder:  make(map[linkKey]time.Time),
		watchSince:      make(map[linkKey]time.Time),
		nackAsked:       make(map[ringKey]nackAsk),
		rtt:             make(map[string]time.Duration),
		keyWanted:       make(map[string]time.Time),
		streamStats:     make(map[string]*StreamCounters),
		log:             slog.Default().With("component", "call-media"),
		lim:             calllog.New(calllog.Every),
		dedup:           newDedupTable(),
		reasm:           newReassembler(),
		ring:            newFragRing(),
		fecTx:           newFECSender(),
		fecRx:           newFECReceiver(),
		stop:            make(chan struct{}),
	}
	if sender != nil {
		d.outbox = NewOutbox(sender)
	}
	go d.housekeeping()
	return d
}

// Outbox exposes the per-peer sender so signaling shares the same non-blocking queues.
func (d *MediaDispatcher) Outbox() *Outbox { return d.outbox }

// Close stops background work and peer workers.
func (d *MediaDispatcher) Close() {
	d.once.Do(func() {
		close(d.stop)
		if d.outbox != nil {
			d.outbox.Close()
		}
	})
}

// SetRoomKey sets the E2EE key for room media. Media is never sent or accepted without it.
func (d *MediaDispatcher) SetRoomKey(key []byte) error {
	if len(key) != keyLen {
		return ErrBadKey
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roomKey = append([]byte(nil), key...)
	return nil
}

// MediaKeySwitch is how long after a new media key arrives this node starts sending with it: the
// others get theirs at about the same time, and until then they could not open it.
const MediaKeySwitch = 2 * time.Second

// mediaKey is a key units may be sealed with. A key handed out to members that all sign names
// each one's signing key for the call: every unit under it ends with its sender's signature, so a
// member, who can seal under the key as well as anybody, still cannot pass a unit off as another's.
type mediaKey struct {
	key     []byte
	signers map[types.PeerID]ed25519.PublicKey // nil: units under this key are not signed
}

// SetSigningKey sets this node's Ed25519 key for the call; it signs every unit sent under a key
// whose members all sign.
func (d *MediaDispatcher) SetSigningKey(priv ed25519.PrivateKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signKey = priv
}

// SetMediaKey takes the call's new media key, handed out by the coordinator when the members
// changed: it is accepted at once and sent with after MediaKeySwitch. Whoever left never gets it,
// so what is said from then on is closed to them although they hold the invite. signers, when not
// nil, are the signing keys of every member the key went to: units under it must carry their
// sender's signature.
func (d *MediaDispatcher) SetMediaKey(key []byte, signers map[types.PeerID]ed25519.PublicKey, now time.Time) error {
	if len(key) != keyLen {
		return ErrBadKey
	}
	var sk map[types.PeerID]ed25519.PublicKey
	if signers != nil {
		sk = make(map[types.PeerID]ed25519.PublicKey, len(signers))
		for id, pub := range signers {
			if len(pub) == ed25519.PublicKeySize {
				sk[id] = append(ed25519.PublicKey(nil), pub...)
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prev = d.sendKeyLocked(now)
	d.cur = mediaKey{key: append([]byte(nil), key...), signers: sk}
	d.curFrom = now.Add(MediaKeySwitch)
	return nil
}

// SendKeyID names the key units go out with now by the first bytes of its hash, for the logs and
// tests (it says nothing of the key).
func (d *MediaDispatcher) SendKeyID() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	k := d.sendKeyLocked(time.Now())
	if k.key == nil {
		return ""
	}
	h := sha256.Sum256(k.key)
	return hex.EncodeToString(h[:4])
}

// SignsUnits reports whether units go out signed now, for the logs and tests.
func (d *MediaDispatcher) SignsUnits() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.sendKeyLocked(time.Now()).signers != nil && d.signKey != nil
}

// sendKeyLocked is the key units go out with now.
func (d *MediaDispatcher) sendKeyLocked(now time.Time) mediaKey {
	if d.cur.key != nil && !now.Before(d.curFrom) {
		return d.cur
	}
	if d.prev.key != nil {
		return d.prev
	}
	return mediaKey{key: d.roomKey}
}

// openKeysLocked are the keys a unit may be sealed with, the likeliest first.
func (d *MediaDispatcher) openKeysLocked() []mediaKey {
	out := make([]mediaKey, 0, 3)
	for _, k := range []mediaKey{d.cur, d.prev, {key: d.roomKey}} {
		if k.key == nil {
			continue
		}
		dup := false
		for _, o := range out {
			if string(o.key) == string(k.key) {
				dup = true
			}
		}
		if !dup {
			out = append(out, k)
		}
	}
	return out
}

// SetVoiceManifest updates the Voice Backbone Tree topology and resets temporary failover links.
func (d *MediaDispatcher) SetVoiceManifest(manifest *tree.TreeManifest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.voiceManifest = manifest
	d.upstreamOverride = types.PeerID{}
	d.adopted = make(map[types.PeerID]bool)
	d.inCall = callPeers(manifest)
}

// callPeers is every peer a voice tree names: its coordinator and standbys, the members it places
// and their primary and standby parents, and every link of its children map.
func callPeers(m *tree.TreeManifest) map[types.PeerID]bool {
	if m == nil {
		return nil
	}
	out := map[types.PeerID]bool{m.Coordinator: true}
	for _, s := range m.StandbyCoordinators {
		out[s] = true
	}
	for id, a := range m.ParentMap {
		out[id], out[a.PrimaryParent], out[a.StandbyParent] = true, true, true
	}
	for id, kids := range m.ChildrenMap {
		out[id] = true
		for _, k := range kids {
			out[k] = true
		}
	}
	delete(out, types.PeerID{})
	return out
}

// UseStandbyParent routes voice through the standby parent until the next manifest arrives.
func (d *MediaDispatcher) UseStandbyParent(parent types.PeerID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.upstreamOverride = parent
}

// Adopt temporarily feeds a child whose primary parent died.
func (d *MediaDispatcher) Adopt(child types.PeerID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.adopted[child] = true
}

// SetStreamWatching toggles whether this node decodes video for a stream.
func (d *MediaDispatcher) SetStreamWatching(streamID string, watching bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if watching {
		d.watchingStreams[streamID] = true
	} else {
		delete(d.watchingStreams, streamID)
		d.rx.drop(streamID)
	}
}

// IsWatchingStream checks if this node is watching a stream.
func (d *MediaDispatcher) IsWatchingStream(streamID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.watchingStreams[streamID]
}

// voiceNeighborsLocked returns every tree link of this node: children, adopted children and upstream.
func (d *MediaDispatcher) voiceNeighborsLocked() []types.PeerID {
	m := d.voiceManifest
	if m == nil {
		return nil
	}
	out := make([]types.PeerID, 0, len(m.ChildrenMap[d.selfID])+len(d.adopted)+1)
	out = append(out, m.ChildrenMap[d.selfID]...)
	for c := range d.adopted {
		out = append(out, c)
	}
	up := d.upstreamOverride
	if up == (types.PeerID{}) {
		if ass, ok := m.ParentMap[d.selfID]; ok {
			up = ass.PrimaryParent
		}
	}
	if up != (types.PeerID{}) && up != d.selfID {
		out = append(out, up)
	}
	return out
}

// VoiceNeighbors is the flooding set for room-wide signaling.
func (d *MediaDispatcher) VoiceNeighbors() []types.PeerID {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.voiceNeighborsLocked()
}

// ProcessIncomingAudio handles a voice fragment: deliver locally once, flood to every other tree link.
func (d *MediaDispatcher) ProcessIncomingAudio(rawBytes []byte, fromPeer types.PeerID) error {
	p, err := Decode(rawBytes)
	if err != nil || p.Kind != FrameAudio || p.StreamID != voiceStreamID {
		d.malformed.Add(1)
		d.logMalformed("audio", fromPeer, p, err)
		return err
	}
	if p.Broadcaster == d.selfID {
		return nil // our own audio echoed back through a failover link
	}
	if !d.fromCall(fromPeer, p.Broadcaster) {
		d.outsiders.Add(1)
		return nil
	}
	now := time.Now()
	if d.dedup.seen(p) {
		d.duplicates.Add(1)
		return nil
	}
	plain, raws, ok := d.authenticate(p, rawBytes, true, now)
	if !ok {
		return nil
	}

	d.mu.RLock()
	neighbors := d.voiceNeighborsLocked()
	d.mu.RUnlock()
	src := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	for _, n := range neighbors {
		if n != fromPeer && n != p.Broadcaster {
			d.relayed.Add(1)
			d.sendAudio(n, src, net_types.TypeCallMediaAudio, raws, now)
		}
	}

	d.consume(p, plain, unitLayer{cap: capAll})
	return nil
}

// ProcessIncomingVideo handles a stream fragment: forward to downstream subscribers, deliver if watching.
func (d *MediaDispatcher) ProcessIncomingVideo(rawBytes []byte, fromPeer types.PeerID) error {
	return d.processVideo(rawBytes, fromPeer, false)
}

// processVideo is ProcessIncomingVideo for a fragment that came from fromPeer, or that was rebuilt
// here from FEC parity and fragments that came from it.
func (d *MediaDispatcher) processVideo(rawBytes []byte, fromPeer types.PeerID, rebuilt bool) error {
	p, err := Decode(rawBytes)
	if err != nil || (p.Kind != FrameVideo && p.Kind != FramePulse && p.Kind != FrameStreamAudio && p.Kind != FrameVideoFEC) {
		d.malformed.Add(1)
		d.logMalformed("video", fromPeer, p, err)
		return err
	}
	now := time.Now()
	d.mu.Lock()
	d.lastFromFeeder[linkKey{p.StreamID, fromPeer}] = now
	d.mu.Unlock()
	if p.Kind == FramePulse || p.Broadcaster == d.selfID {
		return nil
	}
	if !d.fromCall(fromPeer, p.Broadcaster) {
		d.outsiders.Add(1)
		return nil
	}
	if p.Kind == FrameVideoFEC {
		// a parity cannot be authenticated by itself: it is taken only from the stream's feeder,
		// as no other member's must shadow the real one (the first copy of a parity is the one
		// kept) or rebuild a fragment in its name
		if !d.fromFeeder(p.StreamID, fromPeer, now) || !d.currentFeeder(p.StreamID, fromPeer) {
			d.outsiders.Add(1)
			return nil
		}
		return d.processParity(p, rawBytes, fromPeer, now)
	}
	if d.dedup.seen(p) {
		d.duplicates.Add(1)
		return nil
	}
	rawBytes = withoutHopCap(rawBytes) // the cap is the hop's word to us: we relay what was sealed
	if !d.fromFeeder(p.StreamID, fromPeer, now) {
		d.outsiders.Add(1)
		return nil
	}
	watching := d.IsWatchingStream(p.StreamID)
	plain, raws, ok := d.authenticateUnit(p, rawBytes, watching || p.Kind == FrameStreamAudio, rebuilt, now)
	if !ok {
		return nil
	}
	lay := unitLayer{cap: capAll}
	if p.Kind == FrameVideo {
		lay = layerOf(p.Seq, plain)
		d.mu.Lock()
		lay.cap = d.arrivedCapLocked(p, lay, rebuilt)
		d.noteLayerLocked(p.StreamID, p.Broadcaster, lay, now)
		d.mu.Unlock()
	}

	if d.streamEngine != nil {
		forwarded := false
		for _, child := range d.streamEngine.GetDownstream(p.StreamID, d.selfID) {
			if child != fromPeer && child != p.Broadcaster {
				if p.Kind == FrameStreamAudio {
					// sound rides the audio lane: a gap in it is heard, a late video frame is not
					d.relayed.Add(uint64(len(raws)))
					d.sendAudio(child, sourceKey{p.Kind, p.Broadcaster, p.StreamID}, net_types.TypeCallMediaVideo, raws, now)
				} else {
					d.sendVideo(child, p.StreamID, lay, raws)
				}
				forwarded = true
			}
		}
		if forwarded {
			if p.Kind == FrameVideo {
				for i, raw := range raws {
					d.ring.put(p.StreamID, p.Seq, uint8(i), p.FragCount, raw, lay.ring(), now)
				}
			}
			d.markForwarded(p.StreamID, now)
		}
	}

	if watching {
		if p.Kind == FrameVideo {
			space, pos := lay.space(p.Seq, plain)
			for _, raw := range raws {
				d.rx.add(p, len(raw), now, space, pos)
			}
		}
		// a unit of a layer this node does not get (rebuilt from a parity that covered it as
		// the cap changed) would have no place in the numbering its webview sees
		if lay.class() <= lay.cap {
			d.consume(p, plain, lay)
		}
	}
	if p.Kind == FrameVideo {
		for _, raw := range raws {
			if f, err := Decode(raw); err == nil {
				d.recoverFragments(p.StreamID, d.fecRx.fragment(f, now))
			}
		}
	}
	return nil
}

// sendVideo sends a video unit's fragments to a child: those of a frame the child's cap leaves
// out not at all, and to a child that reads it, with the cap they go with.
func (d *MediaDispatcher) sendVideo(child types.PeerID, streamID string, lay unitLayer, raws [][]byte) {
	hop := uint8(0)
	if lay.marked {
		d.mu.Lock()
		f := d.filters[linkKey{streamID, child}]
		if f.active() {
			send, c := f.decide(lay)
			if !send {
				d.mu.Unlock()
				return
			}
			hop = c + 1
		}
		d.mu.Unlock()
	}
	d.relayed.Add(uint64(len(raws)))
	for _, raw := range raws {
		if hop != 0 {
			raw = withHopCap(raw, hop-1)
		}
		d.outbox.Send(child, ClassVideo, net_types.TypeCallMediaVideo, raw)
	}
}

// processParity takes a FEC parity: it is relayed as it came, since it cannot be authenticated by
// itself; what it rebuilds is checked like any other fragment.
func (d *MediaDispatcher) processParity(p *MediaPacket, rawBytes []byte, fromPeer types.PeerID, now time.Time) error {
	if !d.dedup.acceptFragment(p, now) {
		d.duplicates.Add(1)
		return nil
	}
	if d.streamEngine != nil {
		forwarded := false
		class := parityClass(p)
		for _, child := range d.streamEngine.GetDownstream(p.StreamID, d.selfID) {
			if child != fromPeer && child != p.Broadcaster && d.parityFor(child, p.StreamID, class) {
				d.relayed.Add(1)
				d.outbox.Send(child, ClassVideo, net_types.TypeCallMediaVideo, rawBytes)
				forwarded = true
			}
		}
		if forwarded {
			d.markForwarded(p.StreamID, now)
		}
	}
	rebuilt, err := d.fecRx.parity(p, fromPeer, now)
	if err != nil {
		d.malformed.Add(1)
		d.logMalformed("fec", fromPeer, nil, err)
		return nil
	}
	if rebuilt != nil {
		d.recoverFragments(p.StreamID, []fecRebuilt{{rebuilt, fromPeer}})
	}
	return nil
}

// recoverFragments takes in fragments rebuilt from FEC parity as if they had come from the peer
// that sent the parity: they are checked against the feeder again, and forwarded to children and
// delivered like any other once their unit authenticated. A rebuilt fragment never takes the
// place of one that came, and a unit it does not authenticate with waits for the real one.
func (d *MediaDispatcher) recoverFragments(streamID string, rebuilt []fecRebuilt) {
	if len(rebuilt) == 0 {
		return
	}
	d.recovered.Add(uint64(len(rebuilt)))
	d.mu.Lock()
	d.streamLocked(streamID).Recovered += uint64(len(rebuilt))
	d.mu.Unlock()
	for _, f := range rebuilt {
		_ = d.processVideo(f.raw, f.from, true)
	}
}

// currentFeeder reports whether from is the peer the stream's tree feeds this node from now (not
// the one before it, which fromFeeder still takes units from for a moment). With no tree for the
// stream known here, any member of the call is.
func (d *MediaDispatcher) currentFeeder(streamID string, from types.PeerID) bool {
	if d.streamEngine == nil {
		return true
	}
	up, ok := d.streamEngine.GetUpstream(streamID, d.selfID)
	return !ok || up == from
}

// authenticate gathers the fragments of a unit and opens it with the keys it may be sealed with;
// under a key whose members sign, with verify it also checks the sender's signature. Only a unit
// that passes is taken: it is marked in the replay window, relayed and played; anything else
// (forged, tampered, under a key we do not hold) is dropped right here, without a mark that would
// shadow the real unit. It returns the plaintext and the unit's wire fragments.
//
// Sound is verified on every hop (50 units a second per speaker cost nothing); video only where it
// is watched, as a signature check costs ~60 us and a 4K stream carries thousands of units a second.
// A relay that only passes video on takes it from its feeder on the stream alone (fromFeeder), so
// no other member can slip a unit in ahead of the real one there.
func (d *MediaDispatcher) authenticate(p *MediaPacket, raw []byte, verify bool, now time.Time) ([]byte, [][]byte, bool) {
	return d.authenticateUnit(p, raw, verify, false, now)
}

// authenticateUnit is authenticate for a fragment that came (rebuilt false) or was rebuilt here
// from FEC parity. A unit completed with a rebuilt fragment that does not open is kept pending
// without it (reassembler.settle).
func (d *MediaDispatcher) authenticateUnit(p *MediaPacket, raw []byte, verify, rebuilt bool, now time.Time) (plain []byte, raws [][]byte, ok bool) {
	ct, raws, complete := d.reasm.add(p, raw, now, rebuilt)
	if !complete {
		return nil, nil, false
	}
	defer func() { d.reasm.settle(p, ok) }()
	d.mu.RLock()
	keys := d.openKeysLocked()
	d.mu.RUnlock()

	var used mediaKey
	err := ErrBadKey
	for _, key := range keys {
		if plain, err = OpenUnit(p, ct, key.key); err == nil {
			used = key
			break
		}
	}
	if err == nil && used.signers != nil {
		if verify {
			plain, err = VerifyUnitPayload(p, plain, used.signers[p.Broadcaster])
		} else if len(plain) >= UnitSigLen {
			plain = plain[:len(plain)-UnitSigLen]
		} else {
			err = ErrBadUnitSignature
		}
		if err != nil {
			d.forged.Add(1) // a member sealed it, but not the one it names
			d.lim.Log(d.log, slog.LevelWarn, "forged:"+p.StreamID+":"+p.Broadcaster.String(),
				"Dropped media unit not signed by the member it names", "stream", p.StreamID, "kind", kindName(p.Kind),
				"broadcaster", calllog.Peer(p.Broadcaster), "forged_total", d.forged.Load())
			return nil, nil, false
		}
	}
	if err != nil {
		d.decryptFailures.Add(1) // tampered, wrong key, or forged header: never relay or play it
		d.lim.Log(d.log, slog.LevelWarn, "decrypt:"+p.StreamID+":"+p.Broadcaster.String(),
			"Dropped media unit that failed to decrypt", "stream", p.StreamID, "kind", kindName(p.Kind),
			"broadcaster", calllog.Peer(p.Broadcaster), "failures_total", d.decryptFailures.Load(), "err", err)
		return nil, nil, false
	}
	if !d.dedup.mark(p, now) {
		d.duplicates.Add(1) // a copy that went the other way was faster
		d.reasm.settle(p, true)
		return nil, nil, false
	}
	return plain, raws, true
}

// consume hands an authenticated unit to the local consumer.
func (d *MediaDispatcher) consume(p *MediaPacket, plain []byte, lay unitLayer) {
	if d.consumer == nil {
		return
	}
	unit := *p
	unit.FragIdx, unit.FragCount, unit.Payload = 0, 1, plain

	if p.Kind == FrameAudio {
		d.audioDelivered.Add(1)
		d.consumer.OnLocalAudio(&unit)
		return
	}
	if p.Kind == FrameStreamAudio {
		// it rides the stream tree, so it reaches the consumer the way video does; the consumer
		// tells the two apart by Kind. No loss tracking: lost sound is not worth a keyframe.
		d.audioDelivered.Add(1)
		d.mu.Lock()
		d.streamLocked(p.StreamID).SoundDelivered++
		d.mu.Unlock()
		d.consumer.OnLocalVideo(&unit)
		return
	}
	d.videoDelivered.Add(1)
	if rtpKeyframeEnd(plain) {
		d.mu.Lock()
		delete(d.keyWanted, p.StreamID) // the keyframe we waited for, or one asked for by someone else
		d.mu.Unlock()
	}
	d.trackVideoGap(&unit, lay)
	if lay.marked {
		d.mu.Lock()
		ok := d.renumberLocked(sourceKey{p.Kind, p.Broadcaster, p.StreamID}, p.Seq, lay, plain)
		d.mu.Unlock()
		if !ok {
			return
		}
	}
	d.consumer.OnLocalVideo(&unit)
}

// KeyframePending is how long a keyframe this node asked for may take before it asks again. The
// answer is a frame many times the usual size, and the browser behind a frozen picture asks for
// one with every frame it cannot decode; asking again while one is on its way only made the
// broadcaster pour more of them into a link that was already losing packets.
const KeyframePending = 1500 * time.Millisecond

// WantKeyframe reports whether a keyframe request for a stream should go out now, and if so
// records it: not while the last one has neither been answered nor timed out.
func (d *MediaDispatcher) WantKeyframe(streamID string) bool {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.keyWanted[streamID]; ok && now.Sub(at) < KeyframePending {
		return false
	}
	d.keyWanted[streamID] = now
	return true
}

// A sequence gap only counts as loss if the skipped units are still missing a while later.
// Datagrams arrive out of order all the time, and every false alarm used to cost the broadcaster
// a full IDR frame, whose size then starved the frames after it into mush. The while is
// reorderWindow until a NACK has been answered, then grows with the time NACKs take: on a path
// with a long round trip the resent units used to come just after the wait had given up on them.
const (
	reorderWindow = 200 * time.Millisecond
	maxLossWait   = 900 * time.Millisecond // within retransmitWindow and reassemblyTimeout
	maxNackRetry  = 400 * time.Millisecond
	maxTrackedGap = 64
)

// lossTimingLocked is how long a stream's gaps wait before they count as lost, and how long
// before a NACK is repeated; d.mu must be held.
func (d *MediaDispatcher) lossTimingLocked(streamID string) (wait, retry time.Duration) {
	rtt, ok := d.rtt[streamID]
	if !ok {
		return reorderWindow, nackRetry
	}
	wait = min(max(2*rtt+80*time.Millisecond, reorderWindow), maxLossWait)
	retry = min(max(rtt*6/5+30*time.Millisecond, nackRetry), maxNackRetry)
	return wait, retry
}

// sampleRTTLocked folds in the time one NACK took to be answered; d.mu must be held.
func (d *MediaDispatcher) sampleRTTLocked(streamID string, sample time.Duration) {
	sample = min(sample, 2*time.Second)
	if old, ok := d.rtt[streamID]; ok {
		sample = (7*old + sample) / 8
	}
	d.rtt[streamID] = sample
}

func (d *MediaDispatcher) trackVideoGap(p *MediaPacket, lay unitLayer) {
	k := sourceKey{p.Kind, p.Broadcaster, p.StreamID}
	var gap []uint32
	lostNow := false
	d.mu.Lock()
	d.streamLocked(p.StreamID).Delivered++
	last, seen := d.lastDelivered[k]
	diff := int32(p.Seq - last)
	switch {
	case !seen:
		d.lastDelivered[k] = p.Seq
		d.noteDeliveredLocked(k, lay)
	case diff > 0:
		d.lastDelivered[k] = p.Seq
		// in a stream whose layers are cut, only some of the units skipped over were sent to us
		cands, expect := layerGap(last, d.lastLayer[k], p.Seq, lay)
		d.noteDeliveredLocked(k, lay)
		if len(cands) > maxTrackedGap {
			lostNow = true // too much at once to be reordering
		} else if expect > 0 {
			m := d.missing[k]
			if m == nil {
				m = make(map[uint32]*gapGroup)
				d.missing[k] = m
			}
			g := &gapGroup{seqs: cands, expect: expect}
			for _, s := range cands {
				m[s] = g
				gap = append(gap, s)
			}
		}
	default:
		// a late unit filling a gap; a gap whose candidates came as many as it waited for is whole
		if m := d.missing[k]; m != nil {
			if g := m[p.Seq]; g != nil {
				delete(m, p.Seq)
				if g.got++; g.got >= g.expect {
					for _, s := range g.seqs {
						delete(m, s)
					}
				}
			}
		}
		rk := ringKey{p.StreamID, p.Seq}
		if a, ok := d.nackAsked[rk]; ok {
			d.sampleRTTLocked(p.StreamID, time.Since(a.first))
			delete(d.nackAsked, rk)
		}
	}
	wait, retry := d.lossTimingLocked(p.StreamID)
	d.mu.Unlock()

	if lostNow {
		d.lim.Log(d.log, slog.LevelWarn, "gap:"+p.StreamID, "Video jumped too far ahead to recover, asking for a keyframe",
			"stream", p.StreamID, "broadcaster", calllog.Peer(p.Broadcaster), "missing_units", diff-1)
		d.reportVideoLoss(p.StreamID, p.Broadcaster)
	}
	if len(gap) == 0 {
		return
	}
	time.AfterFunc(nackDelay, func() { d.nackGap(k, gap) })
	for at := retry; at < wait; at += retry {
		time.AfterFunc(at, func() { d.nackGap(k, gap) })
	}
	time.AfterFunc(wait, func() {
		still := false
		d.mu.Lock()
		if m := d.missing[k]; m != nil {
			for _, s := range gap {
				if _, ok := m[s]; ok {
					still = true
					delete(m, s)
				}
			}
			if len(m) == 0 {
				delete(d.missing, k)
			}
		}
		d.mu.Unlock()
		if still {
			d.lim.Log(d.log, slog.LevelWarn, "lost:"+p.StreamID, "Video units lost despite NACKs, asking for a keyframe",
				"stream", p.StreamID, "broadcaster", calllog.Peer(p.Broadcaster))
			d.reportVideoLoss(p.StreamID, p.Broadcaster)
		}
	})
}

func (d *MediaDispatcher) reportVideoLoss(streamID string, broadcaster types.PeerID) {
	select {
	case <-d.stop:
		return
	default:
	}
	d.mu.Lock()
	d.streamLocked(streamID).Lost++
	d.mu.Unlock()
	if d.OnVideoLoss != nil {
		d.OnVideoLoss(streamID, broadcaster)
	}
}

// noteDeliveredLocked keeps the layer of the newest unit delivered of a source; d.mu must be held.
func (d *MediaDispatcher) noteDeliveredLocked(k sourceKey, lay unitLayer) {
	if !lay.marked {
		delete(d.lastLayer, k)
		return
	}
	if d.lastLayer == nil {
		d.lastLayer = make(map[sourceKey]unitLayer)
	}
	d.lastLayer[k] = lay
}

// streamLocked returns the counters of a stream, creating them; d.mu must be held.
func (d *MediaDispatcher) streamLocked(streamID string) *StreamCounters {
	c := d.streamStats[streamID]
	if c == nil {
		c = &StreamCounters{}
		d.streamStats[streamID] = c
	}
	return c
}

// StreamStats returns the per-stream counters of this node, including its own broadcasts.
func (d *MediaDispatcher) StreamStats() map[string]StreamCounters {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]StreamCounters, len(d.streamStats)+len(d.videoSeq))
	for id, c := range d.streamStats {
		out[id] = *c
	}
	for id, ctr := range d.videoSeq {
		c := out[id]
		c.UnitsSent = uint64(ctr.Load())
		out[id] = c
	}
	for id, ctr := range d.streamAudioSeq {
		c := out[id]
		c.SoundSent = uint64(ctr.Load())
		out[id] = c
	}
	return out
}

// logMalformed reports a fragment that could not be parsed or does not belong on this path; it
// usually means a peer runs an incompatible version or sends garbage.
func (d *MediaDispatcher) logMalformed(path string, from types.PeerID, p *MediaPacket, err error) {
	args := []any{"path", path, "from", calllog.Peer(from), "malformed_total", d.malformed.Load()}
	if err != nil {
		args = append(args, "err", err)
	} else if p != nil {
		args = append(args, "kind", kindName(p.Kind), "stream", p.StreamID)
	}
	d.lim.Log(d.log, slog.LevelWarn, "malformed:"+path+":"+from.String(), "Dropped malformed media fragment", args...)
}

// kindName names a frame kind in logs.
func kindName(k FrameKind) string {
	switch k {
	case FrameAudio:
		return "audio"
	case FrameVideo:
		return "video"
	case FramePulse:
		return "pulse"
	case FrameStreamAudio:
		return "streamaudio"
	case FrameVideoFEC:
		return "fec"
	}
	return "unknown"
}

// VideoPressure reports how much video the outbox has dropped so far and its deepest queue now;
// the call layer turns it into a send bitrate for the local encoder.
func (d *MediaDispatcher) VideoPressure() (dropped uint64, backlog int) {
	if d.outbox == nil {
		return 0, 0
	}
	return d.outbox.Stats().Dropped[ClassVideo], d.outbox.VideoBacklog()
}

// BroadcastLocalAudio sends local microphone audio to every voice tree link.
func (d *MediaDispatcher) BroadcastLocalAudio(opusBytes []byte) error {
	frags, err := d.seal(FrameAudio, voiceStreamID, d.audioSeq.Add(1), opusBytes, 0)
	if err != nil {
		return err
	}
	src, now := sourceKey{FrameAudio, d.selfID, voiceStreamID}, time.Now()
	for _, n := range d.VoiceNeighbors() {
		d.sendAudio(n, src, net_types.TypeCallMediaAudio, frags, now)
	}
	return nil
}

// BroadcastLocalVideo sends a screen/webcam unit to the first hop of the stream tree.
func (d *MediaDispatcher) BroadcastLocalVideo(streamID string, videoBytes []byte) error {
	d.mu.Lock()
	ctr := d.videoSeq[streamID]
	if ctr == nil {
		ctr = &atomic.Uint32{}
		d.videoSeq[streamID] = ctr
	}
	d.mu.Unlock()

	seq := ctr.Add(1)
	videoBytes, lay := d.countLayers(streamID, seq, videoBytes)
	frags, err := d.seal(FrameVideo, streamID, seq, videoBytes, fecReserve)
	if err != nil {
		return err
	}
	if d.streamEngine == nil {
		return nil
	}
	now := time.Now()
	d.mu.Lock()
	d.noteLayerLocked(streamID, d.selfID, lay, now)
	d.mu.Unlock()
	subs := d.streamEngine.GetDownstream(streamID, d.selfID)
	if len(subs) == 0 {
		return nil
	}
	for i, f := range frags {
		d.ring.put(streamID, seq, uint8(i), uint8(len(frags)), f, lay.ring(), now)
	}
	for _, sub := range subs {
		d.sendVideo(sub, streamID, lay, frags)
	}
	for _, par := range d.fecTx.add(streamID, seq, frags, videoBytes, now) {
		d.sendParity(subs, par, now)
	}
	d.markForwarded(streamID, now)
	return nil
}

// countLayers stamps a marked packet of a local stream with the counts of its layers (videolayer
// Mark.L0, L1), as this node numbers the units: the packetizer cannot know which of its packets
// become units. It returns the packet to seal, a copy when stamped, and its layer. The counts go
// up with the unit seq only while one goroutine sends a stream's units (the capture's pacer):
// BroadcastLocalVideo must not be called for one stream from two at once.
func (d *MediaDispatcher) countLayers(streamID string, seq uint32, pkt []byte) ([]byte, unitLayer) {
	m, off, ok := videolayer.Find(pkt)
	if !ok {
		return pkt, unitLayer{cap: capAll}
	}
	d.mu.Lock()
	if d.layerCount == nil {
		d.layerCount = make(map[string]*[2]uint16)
	}
	c := d.layerCount[streamID]
	if c == nil {
		c = &[2]uint16{}
		d.layerCount[streamID] = c
	}
	class := m.Class()
	if m.Key {
		class = capBase
	}
	if class == capBase {
		c[0]++
	}
	if class <= capMid {
		c[1]++
	}
	m.L0, m.L1 = c[0], c[1]
	d.mu.Unlock()
	out := append([]byte(nil), pkt...)
	videolayer.SetCounts(out, off, m.L0, m.L1)
	return out, unitLayer{marked: true, mark: m, frame: seq - uint32(m.Index), cap: capAll}
}

// sendParity sends a parity of a local stream to the first hop of its tree.
func (d *MediaDispatcher) sendParity(subs []types.PeerID, par fecParityOut, now time.Time) {
	pkt := &MediaPacket{
		Kind:        FrameVideoFEC,
		Seq:         par.first,
		Timestamp:   uint32(now.UnixMilli()),
		Broadcaster: d.selfID,
		StreamID:    par.streamID,
		FragIdx:     par.lane, // lanes may start at the same unit: this keeps their parities apart
		FragCount:   fecLanes,
		Payload:     par.payload,
	}
	if par.layered {
		// a layered stream's parity says its layer class too: class*fecLanes + lane, out of
		// fecLanes*fecClasses (an older node takes the index as it is)
		pkt.FragIdx, pkt.FragCount = par.class*fecLanes+par.lane, fecLanes*fecClasses
	}
	raw := pkt.Encode()
	class := parityClass(pkt)
	for _, sub := range subs {
		if d.parityFor(sub, par.streamID, class) {
			d.outbox.Send(sub, ClassVideo, net_types.TypeCallMediaVideo, raw)
		}
	}
}

// parityClass is the layer step a parity's group is left out at.
func parityClass(p *MediaPacket) uint8 {
	if p.FragCount != fecLanes*fecClasses {
		return capBase
	}
	switch p.FragIdx / fecLanes {
	case 0:
		return capBase
	case 1:
		return capMid
	}
	return capAll
}

// parityFor reports whether a parity of this class goes to a child: not when its cap leaves the
// class out, as the group would miss what it covers there.
func (d *MediaDispatcher) parityFor(child types.PeerID, streamID string, class uint8) bool {
	if class == capBase {
		return true
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	f := d.filters[linkKey{streamID, child}]
	return !f.active() || class <= f.cap
}

// flushParities sends the parities of groups a quiet stream left open.
func (d *MediaDispatcher) flushParities(now time.Time) {
	if d.streamEngine == nil || d.outbox == nil {
		return
	}
	for _, par := range d.fecTx.flush(now) {
		if subs := d.streamEngine.GetDownstream(par.streamID, d.selfID); len(subs) > 0 {
			d.sendParity(subs, par, now)
		}
	}
}

// BroadcastLocalStreamAudio sends a unit of a local stream's sound (a screen's audio) to the
// first hop of that stream's tree. It is not kept for retransmission: by the time a NACK comes
// back, the sound would be too late to play.
func (d *MediaDispatcher) BroadcastLocalStreamAudio(streamID string, opusBytes []byte) error {
	d.mu.Lock()
	ctr := d.streamAudioSeq[streamID]
	if ctr == nil {
		ctr = &atomic.Uint32{}
		d.streamAudioSeq[streamID] = ctr
	}
	d.mu.Unlock()

	frags, err := d.seal(FrameStreamAudio, streamID, ctr.Add(1), opusBytes, 0)
	if err != nil {
		return err
	}
	if d.streamEngine == nil {
		return nil
	}
	subs := d.streamEngine.GetDownstream(streamID, d.selfID)
	src, now := sourceKey{FrameStreamAudio, d.selfID, streamID}, time.Now()
	for _, sub := range subs {
		d.sendAudio(sub, src, net_types.TypeCallMediaVideo, frags, now)
	}
	if len(subs) > 0 {
		d.markForwarded(streamID, now)
	}
	return nil
}

func (d *MediaDispatcher) markForwarded(streamID string, now time.Time) {
	d.mu.Lock()
	d.lastForward[streamID] = now
	d.mu.Unlock()
}

// FeederSilence reports how long the current feeder of a stream has been silent. A feeder that
// has not sent anything yet is measured from the first time we asked, which gives a new link
// the same grace period as an old one.
func (d *MediaDispatcher) FeederSilence(streamID string, feeder types.PeerID, now time.Time) time.Duration {
	k := linkKey{streamID, feeder}
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.lastFromFeeder[k]; ok {
		return now.Sub(last)
	}
	first, ok := d.watchSince[k]
	if !ok {
		d.watchSince[k] = now
		return 0
	}
	return now.Sub(first)
}

// sendPulses keeps every idle stream link alive so children can tell an idle stream from a
// dead feeder within a few hundred milliseconds.
func (d *MediaDispatcher) sendPulses(now time.Time) {
	if d.streamEngine == nil || d.outbox == nil {
		return
	}
	for streamID, children := range d.streamEngine.RelayDuties(d.selfID) {
		d.mu.Lock()
		idle := now.Sub(d.lastForward[streamID]) >= PulseInterval
		if idle {
			d.lastForward[streamID] = now
		}
		d.mu.Unlock()
		if !idle {
			continue
		}
		pulse := (&MediaPacket{Kind: FramePulse, Broadcaster: d.selfID, StreamID: streamID, FragCount: 1}).Encode()
		for _, c := range children {
			d.outbox.Send(c, ClassAudio, net_types.TypeCallMediaVideo, pulse) // tiny: rides the audio lane
		}
	}
}

// seal encrypts a unit into wire fragments that leave reserve bytes of the datagram free.
func (d *MediaDispatcher) seal(kind FrameKind, streamID string, seq uint32, payload []byte, reserve int) ([][]byte, error) {
	d.mu.RLock()
	key := d.sendKeyLocked(time.Now())
	signKey := d.signKey
	d.mu.RUnlock()
	if key.key == nil {
		return nil, ErrBadKey // refuse to leak plaintext media
	}
	unit := &MediaPacket{
		Kind:        kind,
		Seq:         seq,
		Timestamp:   uint32(time.Now().UnixMilli()),
		Broadcaster: d.selfID,
		StreamID:    streamID,
		Payload:     payload,
	}
	if key.signers != nil && signKey != nil {
		unit.Payload = SignUnitPayload(unit, payload, signKey)
	}
	return sealUnit(unit, key.key, reserve)
}

// Stats returns a snapshot of media counters.
func (d *MediaDispatcher) Stats() DispatcherStats {
	s := DispatcherStats{
		AudioDelivered:   d.audioDelivered.Load(),
		VideoDelivered:   d.videoDelivered.Load(),
		FragmentsRelayed: d.relayed.Load(),
		Duplicates:       d.duplicates.Load(),
		DecryptFailures:  d.decryptFailures.Load(),
		Forged:           d.forged.Load(),
		Malformed:        d.malformed.Load(),
		IncompleteUnits:  d.incomplete.Load(),
		Retransmitted:    d.retransmitted.Load(),
		NackedUnits:      d.nacked.Load(),
		Recovered:        d.recovered.Load(),
	}
	if d.outbox != nil {
		s.Outbox = d.outbox.Stats()
	}
	return s
}

func (d *MediaDispatcher) housekeeping() {
	t := time.NewTicker(housekeepingTick)
	defer t.Stop()
	var lastGC time.Time
	for {
		select {
		case <-d.stop:
			return
		case now := <-t.C:
			d.sendPulses(now)
			d.flushParities(now)
			if now.Sub(lastGC) < time.Second {
				continue
			}
			lastGC = now
			if n := d.reasm.expire(now); n > 0 {
				d.incomplete.Add(uint64(n))
				// some fragments of these units never came, not even after NACKs
				d.lim.Log(d.log, slog.LevelWarn, "incomplete", "Dropped media units that never fully arrived",
					"units", n, "incomplete_total", d.incomplete.Load())
			}
			d.dedup.gc(now, 30*time.Second)
			d.repeat.gc(now)
			d.ring.expire(now)
			d.fecRx.gc(now)
			d.mu.Lock()
			d.gcLayersLocked(now)
			for k, a := range d.nackAsked {
				if now.Sub(a.last) > time.Second {
					delete(d.nackAsked, k)
				}
			}
			for k, ts := range d.lastFromFeeder {
				if now.Sub(ts) > 30*time.Second {
					delete(d.lastFromFeeder, k)
				}
			}
			for k, ts := range d.watchSince {
				if now.Sub(ts) > 30*time.Second {
					delete(d.watchSince, k)
				}
			}
			for k, b := range d.resend {
				if now.Sub(b.at) > 10*time.Second {
					delete(d.resend, k) // refilled to the brim by now anyway
				}
			}
			d.mu.Unlock()
		}
	}
}

// fromCall reports whether media relayed by from, from broadcaster, may be taken: both are in the
// call's voice tree (the coordinator, a member placed in it, a child we adopted, the parent we
// were moved to). A peer outside the call that knows its ID gets nothing in, and cannot fill the
// replay windows with forged sequence numbers. With no tree known yet, everything is taken.
func (d *MediaDispatcher) fromCall(from, broadcaster types.PeerID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.voiceManifest == nil {
		return true
	}
	in := func(id types.PeerID) bool { return d.inCall[id] || id == d.upstreamOverride || d.adopted[id] }
	return in(from) && in(broadcaster)
}

// feederGrace is how long units of a stream are still taken from the feeder before the last tree
// change: the coordinator's update reaches the feeders and their children at slightly different
// times.
const feederGrace = 3 * time.Second

type feederState struct {
	cur, prev types.PeerID
	prevUntil time.Time
}

// fromFeeder reports whether a stream's units may be taken from this peer: the one the stream's
// tree feeds this node from (or fed it from a moment ago). Any other member could otherwise send a
// unit, sealed under the call's key, ahead of the real one, and on a relay that does not watch the
// stream, and so does not check signatures, it would take the real unit's place for the subtree.
// With no tree for the stream known here, everything from the call is taken.
func (d *MediaDispatcher) fromFeeder(streamID string, from types.PeerID, now time.Time) bool {
	if d.streamEngine == nil {
		return true
	}
	up, ok := d.streamEngine.GetUpstream(streamID, d.selfID)
	if !ok {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.feeders == nil {
		d.feeders = make(map[string]feederState)
	}
	f := d.feeders[streamID]
	if f.cur != up {
		if f.cur != (types.PeerID{}) {
			f.prev, f.prevUntil = f.cur, now.Add(feederGrace)
			d.restartFiltersLocked(streamID)
		}
		f.cur = up
		d.feeders[streamID] = f
	}
	return from == up || (from == f.prev && now.Before(f.prevUntil))
}
