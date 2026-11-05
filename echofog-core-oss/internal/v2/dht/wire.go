package dht

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	pb "github.com/echofog/echofog-core/internal/v2/dht/pb"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Service identity on the wire (services.md §2).
const (
	// Version is the dht/2 service version.
	Version uint64 = 2

	TypeFindNode  uint64 = 0x40
	TypeNodes     uint64 = 0x41
	TypeFindValue uint64 = 0x42
	TypeValue     uint64 = 0x43
	TypeStore     uint64 = 0x44
	TypeStored    uint64 = 0x45
	TypeError     uint64 = 0x46
)

// STORED status values (services.md §2).
const (
	StatusStored  uint32 = 0
	StatusStale   uint32 = 1
	StatusRefused uint32 = 2
	StatusQuota   uint32 = 3
	StatusInvalid uint32 = 4
)

// ERROR codes (services.md §2).
const (
	CodeBadRecord uint32 = 0x100
	CodeWrongKey  uint32 = 0x101
)

// Frame maxima that the server needs itself; the rest live in service.builtin.
const (
	// MaxNodesPayload is the NODES and VALUE frame maximum.
	MaxNodesPayload = 65536
	// MaxCardsPerAnswer is k: at most 20 cards in NODES or VALUE.
	MaxCardsPerAnswer = K
)

func unmarshalInto(b []byte, m proto.Message, what string) error {
	if err := objects.CheckCanonical(b, m.ProtoReflect().Descriptor()); err != nil {
		return err
	}
	if err := proto.Unmarshal(b, m); err != nil {
		return fmt.Errorf("dht: %s: %w", what, err)
	}
	return nil
}

func unmarshalFindNode(b []byte) (*pb.FindNode, error) {
	m := &pb.FindNode{}
	if err := unmarshalInto(b, m, "FIND_NODE"); err != nil {
		return nil, err
	}
	if len(m.Target) != 32 {
		return nil, fmt.Errorf("dht: FIND_NODE: target is %d bytes", len(m.Target))
	}
	return m, nil
}

func unmarshalFindValue(b []byte) (*pb.FindValue, error) {
	m := &pb.FindValue{}
	if err := unmarshalInto(b, m, "FIND_VALUE"); err != nil {
		return nil, err
	}
	if len(m.Key) != 32 {
		return nil, fmt.Errorf("dht: FIND_VALUE: key is %d bytes", len(m.Key))
	}
	return m, nil
}

func unmarshalStore(b []byte) (*pb.Store, error) {
	m := &pb.Store{}
	if err := unmarshalInto(b, m, "STORE"); err != nil {
		return nil, err
	}
	return m, nil
}

// Client-side decoders (used by the lookup of T5.2 and by tests).

// UnmarshalNodes decodes a NODES frame body.
func UnmarshalNodes(b []byte) (*pb.Nodes, error) {
	m := &pb.Nodes{}
	if err := unmarshalInto(b, m, "NODES"); err != nil {
		return nil, err
	}
	if len(m.Cards) > MaxCardsPerAnswer {
		return nil, fmt.Errorf("dht: NODES: %d cards", len(m.Cards))
	}
	return m, nil
}

// UnmarshalValue decodes a VALUE frame body.
func UnmarshalValue(b []byte) (*pb.Value, error) {
	m := &pb.Value{}
	if err := unmarshalInto(b, m, "VALUE"); err != nil {
		return nil, err
	}
	if len(m.Cards) > MaxCardsPerAnswer {
		return nil, fmt.Errorf("dht: VALUE: %d cards", len(m.Cards))
	}
	return m, nil
}

// UnmarshalStored decodes a STORED frame body.
func UnmarshalStored(b []byte) (*pb.Stored, error) {
	m := &pb.Stored{}
	return m, unmarshalInto(b, m, "STORED")
}

// UnmarshalError decodes an ERROR frame body.
func UnmarshalError(b []byte) (*pb.Error, error) {
	m := &pb.Error{}
	return m, unmarshalInto(b, m, "ERROR")
}

// MarshalFindNode encodes FIND_NODE{req, target}.
func MarshalFindNode(req uint64, target [32]byte) ([]byte, error) {
	return objects.Marshal(&pb.FindNode{Req: req, Target: target[:]})
}

// MarshalFindValue encodes FIND_VALUE{req, key}.
func MarshalFindValue(req uint64, key [32]byte) ([]byte, error) {
	return objects.Marshal(&pb.FindValue{Req: req, Key: key[:]})
}

// MarshalStore encodes STORE{req, record}.
func MarshalStore(req uint64, record []byte) ([]byte, error) {
	return objects.Marshal(&pb.Store{Req: req, Record: record})
}
