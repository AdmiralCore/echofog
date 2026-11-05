package pex

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/pex/pb"
	"github.com/echofog/echofog-core/internal/v2/service"
)

const protocolViolation = frame.CodeProtocolViolation

func marshalGet(req Request) ([]byte, error) {
	return objects.Marshal(&pb.Get{
		Req:       1,
		Max:       req.Max,
		WantFlags: req.WantFlags,
		WantRole:  req.WantRole,
		Region:    req.Region,
		Release:   req.Release,
	})
}

func unmarshalGet(b []byte) (*pb.Get, error) {
	m := &pb.Get{}
	if err := objects.CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("pex: GET: %w", err)
	}
	return m, nil
}

func unmarshalCards(b []byte) (*pb.Cards, error) {
	m := &pb.Cards{}
	if err := objects.CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("pex: CARDS: %w", err)
	}
	return m, nil
}

// Handle serves one pex/2 stream. Requests may share the stream; answers echo req. A GET over the
// session limit is dropped (there is no error frame in services.md §3): the client's 4 s timeout
// ends its attempt.
func (s *Service) Handle(ctx context.Context, p Peer, st *mux.Stream) {
	if spec, ok := service.Builtin(service.PEX); ok {
		st.SetLimiter(spec.Versions[Version].Limiter())
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		f, err := st.ReadFrame()
		if err != nil {
			return
		}
		if f.Type != TypeGet {
			_ = st.CloseWithCode(protocolViolation, "")
			return
		}
		msg, err := unmarshalGet(f.Payload)
		if err != nil {
			_ = st.CloseWithCode(protocolViolation, "")
			return
		}
		now := s.cfg.Now()
		if !s.Allow(p.SID, now) {
			continue
		}
		req := Request{
			Max:       msg.GetMax(),
			WantFlags: msg.GetWantFlags(),
			WantRole:  msg.GetWantRole(),
			Region:    msg.GetRegion(),
			Release:   msg.GetRelease(),
		}
		cards, release := s.Response(p, req, now)
		answer := &pb.Cards{Req: msg.GetReq(), Release: release}
		for _, c := range cards {
			answer.Cards = append(answer.Cards, c.Env.Raw)
		}
		payload, err := objects.Marshal(answer)
		if err != nil {
			_ = st.CloseWithCode(protocolViolation, "")
			return
		}
		if len(payload) > MaxCardsPayload {
			// Never exceed the normative frame maximum: drop the optional release manifest first.
			answer.Release = nil
			payload, err = objects.Marshal(answer)
			if err != nil || len(payload) > MaxCardsPayload {
				continue
			}
		}
		if err := st.WriteFrame(TypeCards, 0, payload); err != nil {
			return
		}
	}
}
