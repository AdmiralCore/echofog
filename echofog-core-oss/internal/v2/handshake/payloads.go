package handshake

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Payloads of handshake.md §7, encoded as canonical protobuf.

// ServiceOffer is one service with the versions the initiator can use, ascending.
type ServiceOffer struct {
	ID       uint32
	Versions []uint32
}

// Access is the invitation access proof (handshake.md §5).
type Access struct {
	PassID []byte // 8 bytes
	Proof  []byte // 32 bytes
}

// HsInit is P1.
type HsInit struct {
	ProtoVer     uint32
	Features     []uint32
	Services     []ServiceOffer
	Access       *Access
	KnownCardSeq uint64
	Time         uint64
	Early        []byte
	Pad          []byte
}

// ServiceChoice is the version the responder selects for one service.
type ServiceChoice struct {
	ID      uint32
	Version uint32
}

// HsResp is P2.
type HsResp struct {
	ProtoVer    uint32
	Features    []uint32
	Services    []ServiceChoice
	Card        []byte
	Reject      *Reject
	EarlyStatus uint32
	Early       []byte
	Observed    []byte
	Time        uint64
	Pad         []byte
}

// Early-data status values of HsResp.early_status.
const (
	EarlyNone      = 0
	EarlyProcessed = 1
	EarlyIgnored   = 2
)

// HsFinish is P3.
type HsFinish struct {
	Card []byte
	Pad  []byte
}

// Reject is the Reject message (P2.reject or a REJECT frame payload).
type Reject struct {
	Reason     uint32
	Min        uint32
	Max        uint32
	Detail     string
	RetryAfter uint32
}

// ErrPayload is returned for a payload that parses but breaks a rule of handshake.md §7.
var ErrPayload = errors.New("handshake: invalid payload")

func (s *ServiceOffer) marshal() []byte {
	return new(pbWriter).uint(1, uint64(s.ID)).packed(2, packed32(s.Versions)...).b
}

func (a *Access) marshal() []byte {
	return new(pbWriter).bytes(1, a.PassID).bytes(2, a.Proof).b
}

// Marshal encodes P1.
func (m *HsInit) Marshal() []byte {
	w := new(pbWriter).uint(1, uint64(m.ProtoVer)).packed(2, packed32(m.Features)...)
	for i := range m.Services {
		w.lenField(3, m.Services[i].marshal())
	}
	if m.Access != nil {
		w.lenField(4, m.Access.marshal())
	}
	return w.uint(5, m.KnownCardSeq).uint(6, m.Time).bytes(7, m.Early).bytes(15, m.Pad).b
}

var (
	schemaHsInit       = map[int]fieldKind{1: kScalar, 2: kPacked, 3: kRepeated, 4: kMsg, 5: kScalar, 6: kScalar, 7: kBytes, 15: kBytes}
	schemaServiceOffer = map[int]fieldKind{1: kScalar, 2: kPacked}
	schemaAccess       = map[int]fieldKind{1: kBytes, 2: kBytes}
	schemaHsResp       = map[int]fieldKind{1: kScalar, 2: kPacked, 3: kRepeated, 4: kBytes, 5: kMsg, 6: kScalar, 7: kBytes, 8: kBytes, 9: kScalar, 15: kBytes}
	schemaChoice       = map[int]fieldKind{1: kScalar, 2: kScalar}
	schemaHsFinish     = map[int]fieldKind{1: kBytes, 15: kBytes}
	schemaReject       = map[int]fieldKind{1: kScalar, 2: kScalar, 3: kScalar, 4: kBytes, 5: kScalar}
)

func strictlyAscending(vs []uint32) bool {
	for i := 1; i < len(vs); i++ {
		if vs[i] <= vs[i-1] {
			return false
		}
	}
	return true
}

// ParseHsInit decodes P1 and checks its list rules (features and services ascending, unique).
func ParseHsInit(b []byte) (*HsInit, error) {
	fs, err := parseCanonical(b, schemaHsInit)
	if err != nil {
		return nil, err
	}
	m := &HsInit{}
	for _, f := range fs {
		switch f.num {
		case 1:
			if m.ProtoVer, err = u32(f.v); err != nil {
				return nil, err
			}
		case 2:
			if m.Features, err = packedVals32(f.data); err != nil {
				return nil, err
			}
		case 3:
			so, err := parseServiceOffer(f.data)
			if err != nil {
				return nil, err
			}
			m.Services = append(m.Services, *so)
		case 4:
			if m.Access, err = parseAccess(f.data); err != nil {
				return nil, err
			}
		case 5:
			m.KnownCardSeq = f.v
		case 6:
			m.Time = f.v
		case 7:
			m.Early = clone(f.data)
		case 15:
			m.Pad = clone(f.data)
		}
	}
	if !strictlyAscending(m.Features) {
		return nil, fmt.Errorf("%w: features not ascending", ErrPayload)
	}
	for i := 1; i < len(m.Services); i++ {
		if m.Services[i].ID <= m.Services[i-1].ID {
			return nil, fmt.Errorf("%w: services not ascending", ErrPayload)
		}
	}
	return m, nil
}

func parseServiceOffer(b []byte) (*ServiceOffer, error) {
	fs, err := parseCanonical(b, schemaServiceOffer)
	if err != nil {
		return nil, err
	}
	s := &ServiceOffer{}
	for _, f := range fs {
		switch f.num {
		case 1:
			if s.ID, err = u32(f.v); err != nil {
				return nil, err
			}
		case 2:
			if s.Versions, err = packedVals32(f.data); err != nil {
				return nil, err
			}
		}
	}
	if !strictlyAscending(s.Versions) {
		return nil, fmt.Errorf("%w: versions not ascending", ErrPayload)
	}
	return s, nil
}

func parseAccess(b []byte) (*Access, error) {
	fs, err := parseCanonical(b, schemaAccess)
	if err != nil {
		return nil, err
	}
	a := &Access{}
	for _, f := range fs {
		switch f.num {
		case 1:
			a.PassID = clone(f.data)
		case 2:
			a.Proof = clone(f.data)
		}
	}
	if len(a.PassID) != 8 || len(a.Proof) != 32 {
		return nil, fmt.Errorf("%w: access sizes", ErrPayload)
	}
	return a, nil
}

// Marshal encodes P2.
func (m *HsResp) Marshal() []byte {
	w := new(pbWriter).uint(1, uint64(m.ProtoVer)).packed(2, packed32(m.Features)...)
	for _, s := range m.Services {
		w.lenField(3, new(pbWriter).uint(1, uint64(s.ID)).uint(2, uint64(s.Version)).b)
	}
	w.bytes(4, m.Card)
	if m.Reject != nil {
		w.lenField(5, m.Reject.Marshal())
	}
	return w.uint(6, uint64(m.EarlyStatus)).bytes(7, m.Early).bytes(8, m.Observed).uint(9, m.Time).bytes(15, m.Pad).b
}

// ParseHsResp decodes P2.
func ParseHsResp(b []byte) (*HsResp, error) {
	fs, err := parseCanonical(b, schemaHsResp)
	if err != nil {
		return nil, err
	}
	m := &HsResp{}
	for _, f := range fs {
		switch f.num {
		case 1:
			if m.ProtoVer, err = u32(f.v); err != nil {
				return nil, err
			}
		case 2:
			if m.Features, err = packedVals32(f.data); err != nil {
				return nil, err
			}
		case 3:
			cf, err := parseCanonical(f.data, schemaChoice)
			if err != nil {
				return nil, err
			}
			var c ServiceChoice
			for _, x := range cf {
				switch x.num {
				case 1:
					c.ID, err = u32(x.v)
				case 2:
					c.Version, err = u32(x.v)
				}
				if err != nil {
					return nil, err
				}
			}
			m.Services = append(m.Services, c)
		case 4:
			if len(f.data) > MaxCardLen {
				return nil, fmt.Errorf("%w: card too large", ErrPayload)
			}
			m.Card = clone(f.data)
		case 5:
			if m.Reject, err = ParseReject(f.data); err != nil {
				return nil, err
			}
		case 6:
			if m.EarlyStatus, err = u32(f.v); err != nil {
				return nil, err
			}
		case 7:
			m.Early = clone(f.data)
		case 8:
			m.Observed = clone(f.data)
		case 9:
			m.Time = f.v
		case 15:
			m.Pad = clone(f.data)
		}
	}
	return m, nil
}

// Marshal encodes P3.
func (m *HsFinish) Marshal() []byte {
	return new(pbWriter).bytes(1, m.Card).bytes(15, m.Pad).b
}

// ParseHsFinish decodes P3.
func ParseHsFinish(b []byte) (*HsFinish, error) {
	fs, err := parseCanonical(b, schemaHsFinish)
	if err != nil {
		return nil, err
	}
	m := &HsFinish{}
	for _, f := range fs {
		switch f.num {
		case 1:
			if len(f.data) > MaxCardLen {
				return nil, fmt.Errorf("%w: card too large", ErrPayload)
			}
			m.Card = clone(f.data)
		case 15:
			m.Pad = clone(f.data)
		}
	}
	return m, nil
}

// Marshal encodes a Reject.
func (r *Reject) Marshal() []byte {
	return new(pbWriter).uint(1, uint64(r.Reason)).uint(2, uint64(r.Min)).uint(3, uint64(r.Max)).
		bytes(4, []byte(r.Detail)).uint(5, uint64(r.RetryAfter)).b
}

// ParseReject decodes a Reject (P2.reject or a REJECT frame payload, ≤ 512 bytes).
func ParseReject(b []byte) (*Reject, error) {
	if len(b) > MaxRejectLen {
		return nil, fmt.Errorf("%w: reject too large", ErrPayload)
	}
	fs, err := parseCanonical(b, schemaReject)
	if err != nil {
		return nil, err
	}
	r := &Reject{}
	for _, f := range fs {
		switch f.num {
		case 1:
			r.Reason, err = u32(f.v)
		case 2:
			r.Min, err = u32(f.v)
		case 3:
			r.Max, err = u32(f.v)
		case 4:
			if len(f.data) > 200 || !utf8.Valid(f.data) {
				return nil, fmt.Errorf("%w: reject detail", ErrPayload)
			}
			r.Detail = string(f.data)
		case 5:
			r.RetryAfter, err = u32(f.v)
		}
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Reject) Error() string {
	return fmt.Sprintf("handshake: rejected (reason %d, min %d, max %d, %q, retry after %d s)", r.Reason, r.Min, r.Max, r.Detail, r.RetryAfter)
}
