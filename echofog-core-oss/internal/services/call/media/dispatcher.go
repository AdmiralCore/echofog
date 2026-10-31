package media

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/services/call/internal/calllog"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/pkg/api/types"
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
// to the local consumer. Relays never decrypt: they forward fragments as received.
type MediaDispatcher struct {
	selfID       types.PeerID
	outbox       *Outbox
	consumer     LocalMediaConsumer
	streamEngine *tree.StreamTreeEngine

	mu               sync.RWMutex
	roomKey          []byte
	voiceManifest    *tree.TreeManifest
	inCall           map[types.PeerID]bool // every peer the voice tree names, built with it
	upstreamOverride types.PeerID
	adopted          map[types.PeerID]bool
	watchingStreams  map[string]bool
	videoSeq         map[string]*atomic.Uint32
	streamAudioSeq   map[string]*atomic.Uint32
	lastDelivered    map[sourceKey]uint32
	missing          map[sourceKey]map[uint32]struct{} // seqs skipped over, waiting out the reorder window
	lastForward      map[string]time.Time              // stream -> last time we sent anything to its children
	lastFromFeeder   map[linkKey]time.Time             // (stream, feeder) -> last frame received from it
	watchSince       map[linkKey]time.Time             // (stream, feeder) -> first time we waited on it
	nackAsked        map[ringKey]nackAsk               // (stream, seq) -> when we asked upstream
	rtt              map[string]time.Duration          // stream -> smoothed time a NACK takes to be answered
	keyWanted        map[string]time.Time              // stream -> when we asked for a keyframe that has not come yet
	streamStats      map[string]*StreamCounters

	audioSeq atomic.Uint32
	dedup    *dedupTable
	reasm    *reassembler
	ring     *fragRing
	fecTx    *fecSender
	fecRx    *fecReceiver

	// OnVideoNack fires shortly after a watched stream has a gap; the call layer sends the NACK
	// to the stream's parent, which resends from its ring.
	OnVideoNack func(streamID string, lost []NackEntry)
	// OnVideoLoss fires when a gap outlives the reorder window despite NACKs; the call layer turns
	// it into a keyframe request.
	OnVideoLoss func(streamID string, broadcaster types.PeerID)

	audioDelivered, videoDelivered, relayed, duplicates, decryptFailures, malformed, incomplete atomic.Uint64
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
		missing:         make(map[sourceKey]map[uint32]struct{}),
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
	if !d.dedup.accept(p, now) {
		d.duplicates.Add(1)
		return nil
	}

	d.mu.RLock()
	neighbors := d.voiceNeighborsLocked()
	d.mu.RUnlock()
	for _, n := range neighbors {
		if n != fromPeer && n != p.Broadcaster {
			d.relayed.Add(1)
			d.outbox.Send(n, ClassAudio, net_types.TypeCallMediaAudio, rawBytes)
		}
	}

	d.deliver(p, now)
	return nil
}

// ProcessIncomingVideo handles a stream fragment: forward to downstream subscribers, deliver if watching.
func (d *MediaDispatcher) ProcessIncomingVideo(rawBytes []byte, fromPeer types.PeerID) error {
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
	if !d.dedup.accept(p, now) {
		d.duplicates.Add(1)
		return nil
	}

	if d.streamEngine != nil {
		forwarded := false
		class := ClassVideo
		if p.Kind == FrameStreamAudio {
			class = ClassAudio // a gap in sound is heard, a late video frame is not
		}
		for _, child := range d.streamEngine.GetDownstream(p.StreamID, d.selfID) {
			if child != fromPeer && child != p.Broadcaster {
				d.relayed.Add(1)
				d.outbox.Send(child, class, net_types.TypeCallMediaVideo, rawBytes)
				forwarded = true
			}
		}
		if forwarded {
			if p.Kind == FrameVideo {
				d.ring.put(p.StreamID, p.Seq, p.FragIdx, p.FragCount, rawBytes, now)
			}
			d.markForwarded(p.StreamID, now)
		}
	}

	if p.Kind == FrameVideoFEC {
		rebuilt, err := d.fecRx.parity(p, now)
		if err != nil {
			d.malformed.Add(1)
			d.logMalformed("fec", fromPeer, nil, err)
			return nil
		}
		if rebuilt != nil {
			d.recoverFragments(p.StreamID, [][]byte{rebuilt}, fromPeer)
		}
		return nil
	}
	if d.IsWatchingStream(p.StreamID) {
		d.deliver(p, now)
	}
	if p.Kind == FrameVideo {
		d.recoverFragments(p.StreamID, d.fecRx.fragment(p, now), fromPeer)
	}
	return nil
}

// recoverFragments takes in fragments rebuilt from FEC parity as if they had come from the peer
// that sent what completed them: they are forwarded to children and delivered like any other.
func (d *MediaDispatcher) recoverFragments(streamID string, rebuilt [][]byte, from types.PeerID) {
	if len(rebuilt) == 0 {
		return
	}
	d.recovered.Add(uint64(len(rebuilt)))
	d.mu.Lock()
	d.streamLocked(streamID).Recovered += uint64(len(rebuilt))
	d.mu.Unlock()
	for _, f := range rebuilt {
		_ = d.ProcessIncomingVideo(f, from)
	}
}

func (d *MediaDispatcher) deliver(p *MediaPacket, now time.Time) {
	ct, complete := d.reasm.add(p, now)
	if !complete || d.consumer == nil {
		return
	}
	d.mu.RLock()
	key := d.roomKey
	d.mu.RUnlock()

	plain, err := OpenUnit(p, ct, key)
	if err != nil {
		d.decryptFailures.Add(1) // tampered, wrong key, or forged header: never play it
		d.lim.Log(d.log, slog.LevelWarn, "decrypt:"+p.StreamID+":"+p.Broadcaster.String(),
			"Dropped media unit that failed to decrypt", "stream", p.StreamID, "kind", kindName(p.Kind),
			"broadcaster", calllog.Peer(p.Broadcaster), "failures_total", d.decryptFailures.Load(), "err", err)
		return
	}
	d.dedup.commit(p) // authentic: now it may move the replay window
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
	d.trackVideoGap(&unit)
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

func (d *MediaDispatcher) trackVideoGap(p *MediaPacket) {
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
	case diff > 0:
		d.lastDelivered[k] = p.Seq
		if diff > maxTrackedGap {
			lostNow = true // too much at once to be reordering
		} else if diff > 1 {
			m := d.missing[k]
			if m == nil {
				m = make(map[uint32]struct{})
				d.missing[k] = m
			}
			for s := last + 1; s != p.Seq; s++ {
				m[s] = struct{}{}
				gap = append(gap, s)
			}
		}
	default:
		// a late unit filling a gap
		if m := d.missing[k]; m != nil {
			delete(m, p.Seq)
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
	for _, n := range d.VoiceNeighbors() {
		for _, f := range frags {
			d.outbox.Send(n, ClassAudio, net_types.TypeCallMediaAudio, f)
		}
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
	frags, err := d.seal(FrameVideo, streamID, seq, videoBytes, fecReserve)
	if err != nil {
		return err
	}
	if d.streamEngine == nil {
		return nil
	}
	subs := d.streamEngine.GetDownstream(streamID, d.selfID)
	if len(subs) == 0 {
		return nil
	}
	now := time.Now()
	for i, f := range frags {
		d.ring.put(streamID, seq, uint8(i), uint8(len(frags)), f, now)
	}
	for _, sub := range subs {
		for _, f := range frags {
			d.outbox.Send(sub, ClassVideo, net_types.TypeCallMediaVideo, f)
		}
	}
	for _, par := range d.fecTx.add(streamID, seq, frags, videoBytes, now) {
		d.sendParity(subs, par, now)
	}
	d.markForwarded(streamID, now)
	return nil
}

// sendParity sends a parity of a local stream to the first hop of its tree.
func (d *MediaDispatcher) sendParity(subs []types.PeerID, par fecParityOut, now time.Time) {
	pkt := (&MediaPacket{
		Kind:        FrameVideoFEC,
		Seq:         par.first,
		Timestamp:   uint32(now.UnixMilli()),
		Broadcaster: d.selfID,
		StreamID:    par.streamID,
		FragCount:   1,
		Payload:     par.payload,
	}).Encode()
	for _, sub := range subs {
		d.outbox.Send(sub, ClassVideo, net_types.TypeCallMediaVideo, pkt)
	}
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
	for _, sub := range subs {
		for _, f := range frags {
			d.outbox.Send(sub, ClassAudio, net_types.TypeCallMediaVideo, f)
		}
	}
	if len(subs) > 0 {
		d.markForwarded(streamID, time.Now())
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
	key := d.roomKey
	d.mu.RUnlock()
	if key == nil {
		return nil, ErrBadKey // refuse to leak plaintext media
	}
	return sealUnit(&MediaPacket{
		Kind:        kind,
		Seq:         seq,
		Timestamp:   uint32(time.Now().UnixMilli()),
		Broadcaster: d.selfID,
		StreamID:    streamID,
		Payload:     payload,
	}, key, reserve)
}

// Stats returns a snapshot of media counters.
func (d *MediaDispatcher) Stats() DispatcherStats {
	s := DispatcherStats{
		AudioDelivered:   d.audioDelivered.Load(),
		VideoDelivered:   d.videoDelivered.Load(),
		FragmentsRelayed: d.relayed.Load(),
		Duplicates:       d.duplicates.Load(),
		DecryptFailures:  d.decryptFailures.Load(),
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
			d.ring.expire(now)
			d.fecRx.gc(now)
			d.mu.Lock()
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
