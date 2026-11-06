// Package v1sig maps the call layer's v1 signals (a JSON envelope with a per-action JSON payload,
// internal/services/call/signaling) to the typed call/2 CallSignal and back (T9.3b, services.md
// 9.1). The envelope's v1 signature is dropped: on call/2 the signature is the originator's
// device-key signature over the exact CallSignal bytes (call.Encode), and the sender is the
// verified device ID, never a value read from the payload.
package v1sig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/v2/call"
	pb "github.com/echofog/echofog-core/internal/v2/call/pb"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// ErrMapping: a signal cannot be mapped between v1 and call/2.
var ErrMapping = errors.New("v1sig: cannot map signal")

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrMapping, fmt.Sprintf(format, a...))
}

// ---- ids and profiles

func idOpt(p types.PeerID) []byte {
	if p == (types.PeerID{}) {
		return nil
	}
	return append([]byte(nil), p[:]...)
}

func idFull(p types.PeerID) []byte { return append([]byte(nil), p[:]...) }

func toID(b []byte) (types.PeerID, error) {
	var p types.PeerID
	switch len(b) {
	case 0:
		return p, nil
	case 32:
		copy(p[:], b)
		return p, nil
	}
	return p, bad("peer id of %d bytes", len(b))
}

func toIDs(bs [][]byte) ([]types.PeerID, error) {
	if len(bs) == 0 {
		return nil, nil
	}
	out := make([]types.PeerID, len(bs))
	for i, b := range bs {
		if len(b) != 32 {
			return nil, bad("peer id of %d bytes", len(b))
		}
		copy(out[i][:], b)
	}
	return out, nil
}

func idsBytes(ps []types.PeerID) [][]byte {
	if len(ps) == 0 {
		return nil
	}
	out := make([][]byte, len(ps))
	for i, p := range ps {
		out[i] = idFull(p)
	}
	return out
}

func sortedKeys[V any](m map[types.PeerID]V) []types.PeerID {
	keys := make([]types.PeerID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	return keys
}

func profileToPB(p tree.PeerNodeProfile) *pb.PeerNodeProfile {
	out := &pb.PeerNodeProfile{
		PeerId: idOpt(p.PeerID), UploadKbps: int64(p.UploadKbps), MaxRelayChildren: int64(p.MaxRelayChildren),
		RttNanos: int64(p.RTT), PacketLossPercent: p.PacketLossPercent, IsMobile: p.IsMobile, RelayOnly: p.RelayOnly,
		Backdrop: p.Backdrop, DisplayHz: int64(p.DisplayHz), SignalKx: append([]byte(nil), p.SignalKX...),
	}
	if !p.LastHeartbeat.IsZero() {
		out.LastHeartbeatUnixNano = p.LastHeartbeat.UnixNano()
	}
	return out
}

func profileToV1(p *pb.PeerNodeProfile) (tree.PeerNodeProfile, error) {
	var out tree.PeerNodeProfile
	if p == nil {
		return out, nil
	}
	id, err := toID(p.PeerId)
	if err != nil {
		return out, err
	}
	out = tree.PeerNodeProfile{
		PeerID: id, UploadKbps: int(p.UploadKbps), MaxRelayChildren: int(p.MaxRelayChildren),
		RTT: time.Duration(p.RttNanos), PacketLossPercent: p.PacketLossPercent, IsMobile: p.IsMobile, RelayOnly: p.RelayOnly,
		Backdrop: p.Backdrop, DisplayHz: int(p.DisplayHz), SignalKX: append([]byte(nil), p.SignalKx...),
	}
	if p.LastHeartbeatUnixNano != 0 {
		out.LastHeartbeat = time.Unix(0, p.LastHeartbeatUnixNano)
	}
	return out, nil
}

func profilesToPB(m map[types.PeerID]tree.PeerNodeProfile) []*pb.ProfileEntry {
	var out []*pb.ProfileEntry
	for _, k := range sortedKeys(m) {
		out = append(out, &pb.ProfileEntry{Peer: idFull(k), Profile: profileToPB(m[k])})
	}
	return out
}

func profilesToV1(es []*pb.ProfileEntry) (map[types.PeerID]tree.PeerNodeProfile, error) {
	if len(es) == 0 {
		return nil, nil
	}
	out := make(map[types.PeerID]tree.PeerNodeProfile, len(es))
	for _, e := range es {
		k, err := toID(e.GetPeer())
		if err != nil {
			return nil, err
		}
		p, err := profileToV1(e.GetProfile())
		if err != nil {
			return nil, err
		}
		out[k] = p
	}
	return out, nil
}

func childrenToPB(m map[types.PeerID][]types.PeerID) []*pb.ChildrenEntry {
	var out []*pb.ChildrenEntry
	for _, k := range sortedKeys(m) {
		out = append(out, &pb.ChildrenEntry{Peer: idFull(k), Children: idsBytes(m[k])})
	}
	return out
}

func childrenToV1(es []*pb.ChildrenEntry) (map[types.PeerID][]types.PeerID, error) {
	if len(es) == 0 {
		return nil, nil
	}
	out := make(map[types.PeerID][]types.PeerID, len(es))
	for _, e := range es {
		k, err := toID(e.GetPeer())
		if err != nil {
			return nil, err
		}
		if out[k], err = toIDs(e.GetChildren()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- manifests

func voiceToPB(m *tree.TreeManifest) *pb.TreeManifest {
	out := &pb.TreeManifest{
		CallId: m.CallID, Epoch: m.Epoch, Coordinator: idOpt(m.Coordinator),
		StandbyCoordinators: idsBytes(m.StandbyCoordinators), Timestamp: m.Timestamp,
		ChildrenMap: childrenToPB(m.ChildrenMap), Profiles: profilesToPB(m.Profiles),
	}
	for _, k := range sortedKeys(m.ParentMap) {
		a := m.ParentMap[k]
		out.ParentMap = append(out.ParentMap, &pb.ParentAssignment{Peer: idFull(k), Primary: idOpt(a.PrimaryParent), Standby: idOpt(a.StandbyParent)})
	}
	return out
}

func voiceToV1(m *pb.TreeManifest) (*tree.TreeManifest, error) {
	if m == nil {
		return nil, bad("no tree manifest")
	}
	out := &tree.TreeManifest{CallID: m.CallId, Epoch: m.Epoch, Timestamp: m.Timestamp}
	var err error
	if out.Coordinator, err = toID(m.Coordinator); err != nil {
		return nil, err
	}
	if out.StandbyCoordinators, err = toIDs(m.StandbyCoordinators); err != nil {
		return nil, err
	}
	if len(m.ParentMap) > 0 {
		out.ParentMap = make(map[types.PeerID]tree.ParentAssignment, len(m.ParentMap))
		for _, e := range m.ParentMap {
			k, err := toID(e.GetPeer())
			if err != nil {
				return nil, err
			}
			var a tree.ParentAssignment
			if a.PrimaryParent, err = toID(e.GetPrimary()); err != nil {
				return nil, err
			}
			if a.StandbyParent, err = toID(e.GetStandby()); err != nil {
				return nil, err
			}
			out.ParentMap[k] = a
		}
	}
	if out.ChildrenMap, err = childrenToV1(m.ChildrenMap); err != nil {
		return nil, err
	}
	if out.Profiles, err = profilesToV1(m.Profiles); err != nil {
		return nil, err
	}
	return out, nil
}

func streamToPB(m *tree.StreamManifest) *pb.StreamManifest {
	out := &pb.StreamManifest{
		StreamId: m.StreamID, Broadcaster: idOpt(m.Broadcaster), MediaType: m.MediaType, BitrateKbps: int64(m.BitrateKbps),
		Width: int64(m.Width), Height: int64(m.Height), Fps: int64(m.FPS), Codec: m.Codec, QualityId: m.QualityID,
		Epoch: m.Epoch, Subscribers: idsBytes(m.Subscribers), DistributionTree: childrenToPB(m.DistributionTree),
		Profiles: profilesToPB(m.Profiles), Overflow: idsBytes(m.Overflow), Weak: idsBytes(m.Weak),
	}
	for _, k := range sortedKeys(m.ParentMap) {
		out.ParentMap = append(out.ParentMap, &pb.ParentEntry{Peer: idFull(k), Parent: idOpt(m.ParentMap[k])})
	}
	for _, k := range sortedKeys(m.Avoid) {
		out.Avoid = append(out.Avoid, &pb.AvoidEntry{Peer: idFull(k), Avoid: idsBytes(m.Avoid[k])})
	}
	return out
}

func streamToV1(m *pb.StreamManifest) (*tree.StreamManifest, error) {
	if m == nil {
		return nil, bad("no stream manifest")
	}
	out := &tree.StreamManifest{
		StreamID: m.StreamId, MediaType: m.MediaType, BitrateKbps: int(m.BitrateKbps), Width: int(m.Width), Height: int(m.Height),
		FPS: int(m.Fps), Codec: m.Codec, QualityID: m.QualityId, Epoch: m.Epoch,
	}
	var err error
	if out.Broadcaster, err = toID(m.Broadcaster); err != nil {
		return nil, err
	}
	if out.Subscribers, err = toIDs(m.Subscribers); err != nil {
		return nil, err
	}
	if out.DistributionTree, err = childrenToV1(m.DistributionTree); err != nil {
		return nil, err
	}
	if len(m.ParentMap) > 0 {
		out.ParentMap = make(map[types.PeerID]types.PeerID, len(m.ParentMap))
		for _, e := range m.ParentMap {
			k, err := toID(e.GetPeer())
			if err != nil {
				return nil, err
			}
			if out.ParentMap[k], err = toID(e.GetParent()); err != nil {
				return nil, err
			}
		}
	}
	if out.Profiles, err = profilesToV1(m.Profiles); err != nil {
		return nil, err
	}
	if out.Overflow, err = toIDs(m.Overflow); err != nil {
		return nil, err
	}
	if len(m.Avoid) > 0 {
		out.Avoid = make(map[types.PeerID][]types.PeerID, len(m.Avoid))
		for _, e := range m.Avoid {
			k, err := toID(e.GetPeer())
			if err != nil {
				return nil, err
			}
			if out.Avoid[k], err = toIDs(e.GetAvoid()); err != nil {
				return nil, err
			}
		}
	}
	if out.Weak, err = toIDs(m.Weak); err != nil {
		return nil, err
	}
	return out, nil
}

func announceToPB(a signaling.StreamAnnouncePayload) *pb.StreamAnnounce {
	return &pb.StreamAnnounce{
		StreamId: a.StreamID, Broadcaster: idOpt(a.Broadcaster), MediaType: a.MediaType, BitrateKbps: int64(a.BitrateKbps),
		Width: int64(a.Width), Height: int64(a.Height), Fps: int64(a.FPS), Codec: a.Codec, QualityId: a.QualityID,
		Profile: profileToPB(a.Profile),
	}
}

func announceToV1(a *pb.StreamAnnounce) (signaling.StreamAnnouncePayload, error) {
	var out signaling.StreamAnnouncePayload
	if a == nil {
		return out, bad("no stream announce")
	}
	out = signaling.StreamAnnouncePayload{
		StreamID: a.StreamId, MediaType: a.MediaType, BitrateKbps: int(a.BitrateKbps), Width: int(a.Width),
		Height: int(a.Height), FPS: int(a.Fps), Codec: a.Codec, QualityID: a.QualityId,
	}
	var err error
	if out.Broadcaster, err = toID(a.Broadcaster); err != nil {
		return out, err
	}
	out.Profile, err = profileToV1(a.Profile)
	return out, err
}

// ---- v1 -> call/2

// ToPB converts a v1 envelope (JSON bytes) to a CallSignal. The payload must match the action.
func ToPB(v1 []byte) (*pb.CallSignal, error) {
	env, err := signaling.DecodeSignal(v1)
	if err != nil {
		return nil, bad("envelope: %v", err)
	}
	action, ok := call.ActionByName(string(env.Action))
	if !ok {
		return nil, bad("unknown action %q", env.Action)
	}
	m := &pb.CallSignal{
		Action: action, CallId: env.CallID, Sender: idFull(env.Sender), MsgId: env.MsgID, Epoch: env.Epoch, Timestamp: env.Timestamp,
	}
	if env.Sender == (types.PeerID{}) {
		return nil, bad("no sender")
	}
	if err := fillPayload(m, env.Payload); err != nil {
		return nil, err
	}
	return m, nil
}

func decode[T any](raw []byte) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, bad("payload missing")
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, bad("payload: %v", err)
	}
	return v, nil
}

func fillPayload(m *pb.CallSignal, raw []byte) error {
	switch m.Action {
	case pb.Action_ACTION_TREE_MANIFEST:
		v, err := decode[*tree.TreeManifest](raw)
		if err != nil || v == nil {
			return orBad(err)
		}
		m.Payload = &pb.CallSignal_TreeManifest{TreeManifest: voiceToPB(v)}
	case pb.Action_ACTION_STREAM_TREE_UPDATE:
		v, err := decode[*tree.StreamManifest](raw)
		if err != nil || v == nil {
			return orBad(err)
		}
		m.Payload = &pb.CallSignal_StreamManifest{StreamManifest: streamToPB(v)}
	case pb.Action_ACTION_COORDINATOR_HEARTBEAT:
		v, err := decode[signaling.CoordinatorHeartbeatPayload](raw)
		if err != nil {
			return err
		}
		hb := &pb.CoordinatorHeartbeat{Coordinator: idOpt(v.Coordinator), Epoch: v.Epoch, Members: int64(v.Members)}
		ids := make([]string, 0, len(v.Streams))
		for id := range v.Streams {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			hb.Streams = append(hb.Streams, &pb.StreamEntry{StreamId: id, Epoch: v.Streams[id]})
		}
		m.Payload = &pb.CallSignal_CoordinatorHeartbeat{CoordinatorHeartbeat: hb}
	case pb.Action_ACTION_STREAM_ANNOUNCE, pb.Action_ACTION_STREAM_REGISTER:
		v, err := decode[signaling.StreamAnnouncePayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_StreamAnnounce{StreamAnnounce: announceToPB(v)}
	case pb.Action_ACTION_STREAM_CLOSE, pb.Action_ACTION_STREAM_UNREGISTER, pb.Action_ACTION_STREAM_UNSUBSCRIBE, pb.Action_ACTION_KEYFRAME_REQUEST:
		v, err := decode[string](raw)
		if err != nil {
			return err
		}
		if v == "" {
			return bad("empty stream id")
		}
		m.Payload = &pb.CallSignal_StreamId{StreamId: v}
	case pb.Action_ACTION_COORDINATOR_ELECTION, pb.Action_ACTION_PARENT_LOST:
		v, err := decode[types.PeerID](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_Peer{Peer: idFull(v)}
	case pb.Action_ACTION_VAD_STATE:
		v, err := decode[signaling.VADPayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_Vad{Vad: &pb.VADState{Speaker: idOpt(v.Speaker), Speaking: v.Speaking, AudioDb: v.AudioDb}}
	case pb.Action_ACTION_JOIN, pb.Action_ACTION_MEMBER_HEARTBEAT:
		v, err := decode[signaling.JoinPayload](raw)
		if err != nil {
			return err
		}
		j := &pb.Join{Profile: profileToPB(v.Profile), Proof: v.Proof, Kx: v.KX, MediaKey: v.SK, JoinSalt: v.JoinSalt}
		if err := call.ValidateJoin(j); err != nil {
			return bad("%v", err)
		}
		m.Payload = &pb.CallSignal_Join{Join: j}
	case pb.Action_ACTION_STREAM_SUBSCRIBE:
		v, err := decode[signaling.StreamSubscribePayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_StreamSubscribe{StreamSubscribe: &pb.StreamSubscribe{StreamId: v.StreamID, Subscriber: idOpt(v.Subscriber), Profile: profileToPB(v.Profile)}}
	case pb.Action_ACTION_STREAM_REPARENT:
		v, err := decode[signaling.ReparentPayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_StreamReparent{StreamReparent: &pb.Reparent{StreamId: v.StreamID, Avoid: idOpt(v.Avoid), Reason: v.Reason}}
	case pb.Action_ACTION_REPORT:
		v, err := decode[signaling.ReportPayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_Report{Report: &pb.Report{Target: idOpt(v.Target), Reason: v.Reason}}
	case pb.Action_ACTION_ROOM_SYNC:
		v, err := decode[signaling.RoomSyncPayload](raw)
		if err != nil {
			return err
		}
		rs := &pb.RoomSync{}
		if v.Voice != nil {
			rs.Voice = voiceToPB(v.Voice)
		}
		for _, s := range v.Streams {
			if s != nil {
				rs.Streams = append(rs.Streams, streamToPB(s))
			}
		}
		for _, a := range v.Announces {
			rs.Announces = append(rs.Announces, announceToPB(a))
		}
		m.Payload = &pb.CallSignal_RoomSync{RoomSync: rs}
	case pb.Action_ACTION_KICKED:
		// The reason is for the log and the UI; a missing one is fine.
		if v, err := decode[string](raw); err == nil && v != "" {
			m.Payload = &pb.CallSignal_Reason{Reason: v}
		}
	case pb.Action_ACTION_NACK:
		v, err := decode[signaling.NackPayload](raw)
		if err != nil {
			return err
		}
		n := &pb.Nack{StreamId: v.StreamID}
		for _, e := range v.Lost {
			n.Lost = append(n.Lost, &pb.NackEntry{Seq: e.Seq, Have: uint32(e.Have)})
		}
		m.Payload = &pb.CallSignal_Nack{Nack: n}
	case pb.Action_ACTION_STREAM_FEEDBACK:
		v, err := decode[signaling.StreamFeedbackPayload](raw)
		if err != nil {
			return err
		}
		f := &pb.StreamFeedback{StreamId: v.StreamID, Layered: v.Layered}
		for _, r := range v.Reports {
			f.Reports = append(f.Reports, &pb.ViewerFeedback{Viewer: idOpt(r.Viewer), Level: uint32(r.Level), Kbps: int64(r.Kbps), Loss: r.Loss, DelayMs: int64(r.DelayMs)})
		}
		m.Payload = &pb.CallSignal_StreamFeedback{StreamFeedback: f}
	case pb.Action_ACTION_STREAM_WEAK:
		v, err := decode[signaling.StreamWeakPayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_StreamWeak{StreamWeak: &pb.StreamWeak{StreamId: v.StreamID, Weak: idsBytes(v.Weak)}}
	case pb.Action_ACTION_MEDIA_KEY:
		v, err := decode[signaling.MediaKeyPayload](raw)
		if err != nil {
			return err
		}
		mk := &pb.MediaKey{KeyEpoch: v.KeyEpoch, Eph: v.Eph, Sealed: v.Sealed}
		for _, k := range sortedKeys(v.Signers) {
			mk.Signers = append(mk.Signers, &pb.SignerEntry{Peer: idFull(k), Key: v.Signers[k]})
		}
		m.Payload = &pb.CallSignal_MediaKey{MediaKey: mk}
	case pb.Action_ACTION_MEDIA_KEY_ACK:
		v, err := decode[signaling.KeyAckPayload](raw)
		if err != nil {
			return err
		}
		m.Payload = &pb.CallSignal_MediaKeyAck{MediaKeyAck: &pb.KeyAck{Epoch: v.Epoch}}
	case pb.Action_ACTION_LEAVE, pb.Action_ACTION_PING, pb.Action_ACTION_PONG, pb.Action_ACTION_SYNC_REQUEST, pb.Action_ACTION_ADOPT:
		// no payload
	default:
		return bad("action %v", m.Action)
	}
	return nil
}

func orBad(err error) error {
	if err != nil {
		return err
	}
	return bad("payload missing")
}

// ---- call/2 -> v1

// ToV1 converts a verified CallSignal to the v1 envelope JSON the call layer reads. The envelope
// is unsigned: the caller has verified the signal on call/2.
func ToV1(m *pb.CallSignal) ([]byte, error) {
	if m == nil {
		return nil, bad("nil signal")
	}
	name := call.V1Name(m.Action)
	if name == "" {
		return nil, bad("action %v", m.Action)
	}
	sender, err := toID(m.Sender)
	if err != nil || sender == (types.PeerID{}) {
		return nil, bad("sender of %d bytes", len(m.Sender))
	}
	payload, err := payloadV1(m)
	if err != nil {
		return nil, err
	}
	env := signaling.CallSignalEnvelope{
		Action: signaling.SignalAction(name), CallID: m.CallId, Sender: sender, MsgID: m.MsgId, Epoch: m.Epoch,
		Timestamp: m.Timestamp, Payload: payload,
	}
	return json.Marshal(env)
}

func payloadV1(m *pb.CallSignal) ([]byte, error) {
	var v any
	switch m.Action {
	case pb.Action_ACTION_TREE_MANIFEST:
		p, ok := m.Payload.(*pb.CallSignal_TreeManifest)
		if !ok || p.TreeManifest == nil {
			return nil, bad("%v without tree manifest", m.Action)
		}
		x, err := voiceToV1(p.TreeManifest)
		if err != nil {
			return nil, err
		}
		v = x
	case pb.Action_ACTION_STREAM_TREE_UPDATE:
		p, ok := m.Payload.(*pb.CallSignal_StreamManifest)
		if !ok || p.StreamManifest == nil {
			return nil, bad("%v without stream manifest", m.Action)
		}
		x, err := streamToV1(p.StreamManifest)
		if err != nil {
			return nil, err
		}
		v = x
	case pb.Action_ACTION_COORDINATOR_HEARTBEAT:
		p, ok := m.Payload.(*pb.CallSignal_CoordinatorHeartbeat)
		if !ok || p.CoordinatorHeartbeat == nil {
			return nil, bad("%v without heartbeat", m.Action)
		}
		h := p.CoordinatorHeartbeat
		out := signaling.CoordinatorHeartbeatPayload{Epoch: h.Epoch, Members: int(h.Members)}
		var err error
		if out.Coordinator, err = toID(h.Coordinator); err != nil {
			return nil, err
		}
		if len(h.Streams) > 0 {
			out.Streams = make(map[string]uint64, len(h.Streams))
			for _, e := range h.Streams {
				out.Streams[e.GetStreamId()] = e.GetEpoch()
			}
		}
		v = out
	case pb.Action_ACTION_STREAM_ANNOUNCE, pb.Action_ACTION_STREAM_REGISTER:
		p, ok := m.Payload.(*pb.CallSignal_StreamAnnounce)
		if !ok {
			return nil, bad("%v without announce", m.Action)
		}
		x, err := announceToV1(p.StreamAnnounce)
		if err != nil {
			return nil, err
		}
		v = x
	case pb.Action_ACTION_STREAM_CLOSE, pb.Action_ACTION_STREAM_UNREGISTER, pb.Action_ACTION_STREAM_UNSUBSCRIBE, pb.Action_ACTION_KEYFRAME_REQUEST:
		p, ok := m.Payload.(*pb.CallSignal_StreamId)
		if !ok || p.StreamId == "" {
			return nil, bad("%v without stream id", m.Action)
		}
		v = p.StreamId
	case pb.Action_ACTION_COORDINATOR_ELECTION, pb.Action_ACTION_PARENT_LOST:
		p, ok := m.Payload.(*pb.CallSignal_Peer)
		if !ok || len(p.Peer) != 32 {
			return nil, bad("%v without peer", m.Action)
		}
		id, _ := toID(p.Peer)
		v = id
	case pb.Action_ACTION_VAD_STATE:
		p, ok := m.Payload.(*pb.CallSignal_Vad)
		if !ok || p.Vad == nil {
			return nil, bad("%v without state", m.Action)
		}
		id, err := toID(p.Vad.Speaker)
		if err != nil {
			return nil, err
		}
		v = signaling.VADPayload{Speaker: id, Speaking: p.Vad.Speaking, AudioDb: p.Vad.AudioDb}
	case pb.Action_ACTION_JOIN, pb.Action_ACTION_MEMBER_HEARTBEAT:
		p, ok := m.Payload.(*pb.CallSignal_Join)
		if !ok {
			return nil, bad("%v without join", m.Action)
		}
		if err := call.ValidateJoin(p.Join); err != nil {
			return nil, bad("%v", err)
		}
		prof, err := profileToV1(p.Join.Profile)
		if err != nil {
			return nil, err
		}
		v = signaling.JoinPayload{Profile: prof, Proof: p.Join.Proof, KX: p.Join.Kx, SK: p.Join.MediaKey, JoinSalt: p.Join.JoinSalt}
	case pb.Action_ACTION_STREAM_SUBSCRIBE:
		p, ok := m.Payload.(*pb.CallSignal_StreamSubscribe)
		if !ok || p.StreamSubscribe == nil {
			return nil, bad("%v without subscribe", m.Action)
		}
		sub, err := toID(p.StreamSubscribe.Subscriber)
		if err != nil {
			return nil, err
		}
		prof, err := profileToV1(p.StreamSubscribe.Profile)
		if err != nil {
			return nil, err
		}
		v = signaling.StreamSubscribePayload{StreamID: p.StreamSubscribe.StreamId, Subscriber: sub, Profile: prof}
	case pb.Action_ACTION_STREAM_REPARENT:
		p, ok := m.Payload.(*pb.CallSignal_StreamReparent)
		if !ok || p.StreamReparent == nil {
			return nil, bad("%v without reparent", m.Action)
		}
		avoid, err := toID(p.StreamReparent.Avoid)
		if err != nil {
			return nil, err
		}
		v = signaling.ReparentPayload{StreamID: p.StreamReparent.StreamId, Avoid: avoid, Reason: p.StreamReparent.Reason}
	case pb.Action_ACTION_REPORT:
		p, ok := m.Payload.(*pb.CallSignal_Report)
		if !ok || p.Report == nil {
			return nil, bad("%v without report", m.Action)
		}
		t, err := toID(p.Report.Target)
		if err != nil {
			return nil, err
		}
		v = signaling.ReportPayload{Target: t, Reason: p.Report.Reason}
	case pb.Action_ACTION_ROOM_SYNC:
		p, ok := m.Payload.(*pb.CallSignal_RoomSync)
		if !ok || p.RoomSync == nil {
			return nil, bad("%v without room sync", m.Action)
		}
		var out signaling.RoomSyncPayload
		var err error
		if p.RoomSync.Voice != nil {
			if out.Voice, err = voiceToV1(p.RoomSync.Voice); err != nil {
				return nil, err
			}
		}
		for _, s := range p.RoomSync.Streams {
			x, err := streamToV1(s)
			if err != nil {
				return nil, err
			}
			out.Streams = append(out.Streams, x)
		}
		for _, a := range p.RoomSync.Announces {
			x, err := announceToV1(a)
			if err != nil {
				return nil, err
			}
			out.Announces = append(out.Announces, x)
		}
		v = out
	case pb.Action_ACTION_KICKED:
		if p, ok := m.Payload.(*pb.CallSignal_Reason); ok {
			v = p.Reason
		} else if m.Payload != nil {
			return nil, bad("%v with a payload of another kind", m.Action)
		}
	case pb.Action_ACTION_NACK:
		p, ok := m.Payload.(*pb.CallSignal_Nack)
		if !ok || p.Nack == nil {
			return nil, bad("%v without nack", m.Action)
		}
		out := signaling.NackPayload{StreamID: p.Nack.StreamId}
		for _, e := range p.Nack.Lost {
			if e.GetHave() > 0xffff {
				return nil, bad("nack bitmap %#x", e.GetHave())
			}
			out.Lost = append(out.Lost, media.NackEntry{Seq: e.GetSeq(), Have: uint16(e.GetHave())})
		}
		v = out
	case pb.Action_ACTION_STREAM_FEEDBACK:
		p, ok := m.Payload.(*pb.CallSignal_StreamFeedback)
		if !ok || p.StreamFeedback == nil {
			return nil, bad("%v without feedback", m.Action)
		}
		out := signaling.StreamFeedbackPayload{StreamID: p.StreamFeedback.StreamId, Layered: p.StreamFeedback.Layered}
		for _, r := range p.StreamFeedback.Reports {
			id, err := toID(r.GetViewer())
			if err != nil {
				return nil, err
			}
			if r.GetLevel() > 255 {
				return nil, bad("congestion level %d", r.GetLevel())
			}
			out.Reports = append(out.Reports, signaling.ViewerFeedback{Viewer: id, Level: uint8(r.GetLevel()), Kbps: int(r.GetKbps()), Loss: r.GetLoss(), DelayMs: int(r.GetDelayMs())})
		}
		v = out
	case pb.Action_ACTION_STREAM_WEAK:
		p, ok := m.Payload.(*pb.CallSignal_StreamWeak)
		if !ok || p.StreamWeak == nil {
			return nil, bad("%v without weak list", m.Action)
		}
		weak, err := toIDs(p.StreamWeak.Weak)
		if err != nil {
			return nil, err
		}
		v = signaling.StreamWeakPayload{StreamID: p.StreamWeak.StreamId, Weak: weak}
	case pb.Action_ACTION_MEDIA_KEY:
		p, ok := m.Payload.(*pb.CallSignal_MediaKey)
		if !ok || p.MediaKey == nil {
			return nil, bad("%v without key", m.Action)
		}
		out := signaling.MediaKeyPayload{KeyEpoch: p.MediaKey.KeyEpoch, Eph: p.MediaKey.Eph, Sealed: p.MediaKey.Sealed}
		if len(p.MediaKey.Signers) > 0 {
			out.Signers = make(map[types.PeerID][]byte, len(p.MediaKey.Signers))
			for _, e := range p.MediaKey.Signers {
				id, err := toID(e.GetPeer())
				if err != nil {
					return nil, err
				}
				out.Signers[id] = e.GetKey()
			}
		}
		v = out
	case pb.Action_ACTION_MEDIA_KEY_ACK:
		p, ok := m.Payload.(*pb.CallSignal_MediaKeyAck)
		if !ok || p.MediaKeyAck == nil {
			return nil, bad("%v without ack", m.Action)
		}
		v = signaling.KeyAckPayload{Epoch: p.MediaKeyAck.Epoch}
	case pb.Action_ACTION_LEAVE, pb.Action_ACTION_PING, pb.Action_ACTION_PONG, pb.Action_ACTION_SYNC_REQUEST, pb.Action_ACTION_ADOPT:
		if m.Payload != nil {
			return nil, bad("%v carries a payload", m.Action)
		}
		return nil, nil
	default:
		return nil, bad("action %v", m.Action)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, bad("payload: %v", err)
	}
	return b, nil
}
