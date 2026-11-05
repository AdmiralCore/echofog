package dht

import (
	"context"
	"net/netip"

	pb "github.com/echofog/echofog-core/internal/v2/dht/pb"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/service"
)

const protocolViolation = frame.CodeProtocolViolation

// UnitOf returns the diversity unit of an address: an IPv4 /24 or an IPv6 /32 (dht.md §1).
func UnitOf(a netip.Addr) netip.Prefix { return diversityUnit(a.Unmap()) }

// Handle serves one dht/2 stream. Requests may share the stream; every answer echoes req and
// travels on the stream of the authenticated session, so a reply is bound to the asked node (K4).
// A request over the session rate limit is dropped: services.md §2 has no error frame for it, and
// the client's 2 s timeout ends its attempt.
func (s *Server) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	if spec, ok := service.Builtin(service.DHT); ok {
		st.SetLimiter(spec.Versions[Version].Limiter())
	}
	for {
		if ctx.Err() != nil {
			return
		}
		f, err := st.ReadFrame()
		if err != nil {
			return
		}
		now := s.cfg.Now()
		var typ uint64
		var payload []byte
		switch f.Type {
		case TypeFindNode:
			m, err := unmarshalFindNode(f.Payload)
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
			if !s.Allow(p.SID, now) {
				continue
			}
			typ = TypeNodes
			payload, err = objects.Marshal(&pb.Nodes{Req: m.Req, Cards: s.FindNodeAt([32]byte(m.Target), now)})
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
		case TypeFindValue:
			m, err := unmarshalFindValue(f.Payload)
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
			if !s.Allow(p.SID, now) {
				continue
			}
			rec, cards := s.FindValueAt([32]byte(m.Key), now)
			typ = TypeValue
			payload, err = objects.Marshal(&pb.Value{Req: m.Req, Record: rec, Cards: cards})
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
		case TypeStore:
			m, err := unmarshalStore(f.Payload)
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
			if !s.Allow(p.SID, now) {
				continue
			}
			res := s.StoreAt(p, m.Record, now)
			if res.Err != 0 {
				typ = TypeError
				payload, err = objects.Marshal(&pb.Error{Req: m.Req, Code: res.Err})
			} else {
				typ = TypeStored
				payload, err = objects.Marshal(&pb.Stored{Req: m.Req, Status: res.Status})
			}
			if err != nil {
				_ = st.CloseWithCode(protocolViolation, "")
				return
			}
		default:
			// A response type or an ERROR from the peer: this stream only takes requests.
			_ = st.CloseWithCode(protocolViolation, "")
			return
		}
		if err := st.WriteFrame(typ, 0, payload); err != nil {
			return
		}
	}
}
