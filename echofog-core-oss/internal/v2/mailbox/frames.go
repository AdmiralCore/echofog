// Package mailbox implements the mailbox/2 service of protocol v2 (docs/protocol/v2/services.md
// §5): store-and-forward queues on a mailbox relay, proof of work for public queues, quotas and
// the receiving side of forwarded deposits.
//
// The package has no dependency on the node runtime: it accepts the authenticated peer and the
// stream the runtime hands to a service handler. node/modules_mailbox.go builds it from the host.
package mailbox

import (
	"errors"
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/echofog/echofog-core/internal/v2/service"
)

// Service-local frame types (services.md §5.1).
const (
	FrameQueueCreate uint64 = 0x40
	FrameQueueOK     uint64 = 0x41
	FrameDeposit     uint64 = 0x42
	FrameDeposited   uint64 = 0x43
	FrameFetch       uint64 = 0x44
	FrameItems       uint64 = 0x45
	FrameAck         uint64 = 0x46
	FrameForward     uint64 = 0x47
	FrameError       uint64 = 0x48
)

// DEPOSITED status values (services.md §5).
const (
	StatusStored           uint32 = 0
	StatusUnknownQueue     uint32 = 1
	StatusFull             uint32 = 2
	StatusTooLarge         uint32 = 3
	StatusRefused          uint32 = 4
	StatusRelayUnreachable uint32 = 5
	StatusPowRequired      uint32 = 6
)

// Auth operations (services.md §5.2).
const (
	OpFetch byte = 1
	OpAck   byte = 2
)

// CodePowInvalid is the mailbox/2 service code of a proof of work below the queue's threshold
// (services.md §5.4). It travels in ERROR.code.
const CodePowInvalid uint32 = 0x100

// KDF and signature input labels (registries.md §9).
const (
	AuthLabel    = "echofog/mailbox2/fetch-auth"
	ForwardLabel = "echofog/mailbox2/forward"
	PowLabel     = "echofog/mailbox2/pow"
)

// Sizes and protocol limits (services.md §5.1–§5.4).
const (
	QueueSize       = 16
	ItemSize        = 16
	MaxBlob         = 65536
	DefaultPowBits  = 20
	MaxPowBits      = 26
	MaxAckIDs       = 100
	MaxWait         = 60 * time.Second
	MaxQueueTTL     = 30 * 24 * time.Hour
	MaxForwardBytes = 73728
	MaxItemsBytes   = 1048576
)

// ErrBadFrame reports a service frame whose payload is not the canonical message of its type.
var ErrBadFrame = errors.New("mailbox: malformed frame")

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrBadFrame, what) }

// Payload limits below are the "Max" columns of services.md §5.1. The runtime's frame limiter
// already enforces them; the parser also refuses a field that alone exceeds the message.
func frameMax(typ uint64) int {
	spec, ok := service.Builtin(service.Mailbox)
	if !ok {
		return 0
	}
	for _, ft := range spec.Versions[2].Types {
		if ft.Type == typ {
			return ft.Max
		}
	}
	return 0
}

// FrameMax returns the normative maximum payload of a mailbox/2 frame type, or 0 for an unknown
// type. It is exported for the client of the next task.
func FrameMax(typ uint64) int { return frameMax(typ) }

// QueueCreate is QUEUE_CREATE (0x40).
type QueueCreate struct {
	Req     uint64
	Queue   []byte
	AuthKey []byte
	TTL     uint32
	Public  bool
	PowBits uint32
}

func (m QueueCreate) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbBytes(b, 2, m.Queue)
	b = pbBytes(b, 3, m.AuthKey)
	b = pbUint(b, 4, uint64(m.TTL))
	b = pbUint(b, 5, pbBool(m.Public))
	return pbUint(b, 6, uint64(m.PowBits))
}

func ParseQueueCreate(p []byte) (QueueCreate, error) {
	var m QueueCreate
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("queue-create.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType {
				return malformed("queue-create.queue")
			}
			m.Queue = s
		case 3:
			if typ != protowire.BytesType || len(s) > 64 {
				return malformed("queue-create.auth_key")
			}
			m.AuthKey = s
		case 4:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("queue-create.ttl")
			}
			m.TTL = uint32(v)
		case 5:
			b, ok := parseBool(typ, v)
			if !ok {
				return malformed("queue-create.public")
			}
			m.Public = b
		case 6:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("queue-create.pow_bits")
			}
			m.PowBits = uint32(v)
		}
		return nil
	})
	return m, err
}

// QueueOK is QUEUE_OK (0x41).
type QueueOK struct {
	Req     uint64
	Expires uint64
}

func (m QueueOK) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	return pbUint(b, 2, m.Expires)
}

func ParseQueueOK(p []byte) (QueueOK, error) {
	var m QueueOK
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		if typ != protowire.VarintType {
			return malformed("queue-ok")
		}
		switch num {
		case 1:
			m.Req = v
		case 2:
			m.Expires = v
		}
		return nil
	})
	return m, err
}

// Deposit is DEPOSIT (0x42).
type Deposit struct {
	Req   uint64
	Queue []byte
	Blob  []byte
	Pow   uint64
}

func (m Deposit) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbBytes(b, 2, m.Queue)
	b = pbBytes(b, 3, m.Blob)
	return pbUint(b, 4, m.Pow)
}

func ParseDeposit(p []byte) (Deposit, error) {
	var m Deposit
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("deposit.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType {
				return malformed("deposit.queue")
			}
			m.Queue = s
		case 3:
			// The frame limiter bounds the message; the parser allows the small overhead above
			// MaxBlob so a too-large blob reaches DEPOSIT and is answered status "too-large"
			// instead of closing the stream.
			if typ != protowire.BytesType || len(s) > MaxForwardBytes {
				return malformed("deposit.blob")
			}
			m.Blob = s
		case 4:
			if typ != protowire.VarintType {
				return malformed("deposit.pow")
			}
			m.Pow = v
		}
		return nil
	})
	return m, err
}

// Deposited is DEPOSITED (0x43).
type Deposited struct {
	Req    uint64
	Status uint32
	Item   []byte
}

func (m Deposited) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbUint(b, 2, uint64(m.Status))
	return pbBytes(b, 3, m.Item)
}

func ParseDeposited(p []byte) (Deposited, error) {
	var m Deposited
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("deposited.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("deposited.status")
			}
			m.Status = uint32(v)
		case 3:
			if typ != protowire.BytesType {
				return malformed("deposited.item")
			}
			m.Item = s
		}
		return nil
	})
	return m, err
}

// Fetch is FETCH (0x44).
type Fetch struct {
	Req   uint64
	Queue []byte
	After []byte
	Max   uint32
	Wait  uint32
	Auth  []byte
}

func (m Fetch) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbBytes(b, 2, m.Queue)
	b = pbBytes(b, 3, m.After)
	b = pbUint(b, 4, uint64(m.Max))
	b = pbUint(b, 5, uint64(m.Wait))
	return pbBytes(b, 6, m.Auth)
}

func ParseFetch(p []byte) (Fetch, error) {
	var m Fetch
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("fetch.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType {
				return malformed("fetch.queue")
			}
			m.Queue = s
		case 3:
			if typ != protowire.BytesType {
				return malformed("fetch.after")
			}
			m.After = s
		case 4, 5:
			if typ != protowire.VarintType || v > math.MaxUint32 {
				return malformed("fetch.max/wait")
			}
			if num == 4 {
				m.Max = uint32(v)
			} else {
				m.Wait = uint32(v)
			}
		case 6:
			if typ != protowire.BytesType || len(s) > 128 {
				return malformed("fetch.auth")
			}
			m.Auth = s
		}
		return nil
	})
	return m, err
}

// Item is one queued deposit as returned by FETCH.
type Item struct {
	ID     []byte
	Blob   []byte
	Stored uint64
}

// Items is ITEMS (0x45).
type Items struct {
	Req   uint64
	Items []Item
	More  bool
}

func (m Items) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	for _, it := range m.Items {
		body := pbBytes(nil, 1, it.ID)
		body = pbBytes(body, 2, it.Blob)
		body = pbUint(body, 3, it.Stored)
		b = pbBytes(b, 2, body)
	}
	return pbUint(b, 3, pbBool(m.More))
}

func ParseItems(p []byte) (Items, error) {
	var m Items
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("items.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType || len(m.Items) >= 1000 {
				return malformed("items.items")
			}
			it, err := parseItem(s)
			if err != nil {
				return err
			}
			m.Items = append(m.Items, it)
		case 3:
			b, ok := parseBool(typ, v)
			if !ok {
				return malformed("items.more")
			}
			m.More = b
		}
		return nil
	})
	return m, err
}

func parseItem(p []byte) (Item, error) {
	var it Item
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.BytesType || len(s) > ItemSize {
				return malformed("item.id")
			}
			it.ID = s
		case 2:
			if typ != protowire.BytesType || len(s) > MaxBlob {
				return malformed("item.blob")
			}
			it.Blob = s
		case 3:
			if typ != protowire.VarintType {
				return malformed("item.stored")
			}
			it.Stored = v
		}
		return nil
	})
	return it, err
}

// Ack is ACK (0x46).
type Ack struct {
	Req   uint64
	Queue []byte
	IDs   [][]byte
	Auth  []byte
}

func (m Ack) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbBytes(b, 2, m.Queue)
	for _, id := range m.IDs {
		b = pbBytes(b, 3, id)
	}
	return pbBytes(b, 4, m.Auth)
}

func ParseAck(p []byte) (Ack, error) {
	var m Ack
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("ack.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType {
				return malformed("ack.queue")
			}
			m.Queue = s
		case 3:
			if typ != protowire.BytesType || len(s) > ItemSize || len(m.IDs) >= MaxAckIDs {
				return malformed("ack.ids")
			}
			m.IDs = append(m.IDs, s)
		case 4:
			if typ != protowire.BytesType || len(s) > 128 {
				return malformed("ack.auth")
			}
			m.Auth = s
		}
		return nil
	})
	return m, err
}

// Forward is FORWARD (0x47).
type Forward struct {
	Req       uint64
	Relay     []byte
	RelayCard []byte
	Sealed    []byte
}

func (m Forward) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	b = pbBytes(b, 2, m.Relay)
	b = pbBytes(b, 3, m.RelayCard)
	return pbBytes(b, 4, m.Sealed)
}

func ParseForward(p []byte) (Forward, error) {
	var m Forward
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error {
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return malformed("forward.req")
			}
			m.Req = v
		case 2:
			if typ != protowire.BytesType || len(s) > 64 {
				return malformed("forward.relay")
			}
			m.Relay = s
		case 3:
			if typ != protowire.BytesType || len(s) > 6400 {
				return malformed("forward.relay_card")
			}
			m.RelayCard = s
		case 4:
			if typ != protowire.BytesType || len(s) > MaxForwardBytes {
				return malformed("forward.sealed")
			}
			m.Sealed = s
		}
		return nil
	})
	return m, err
}

// Error is ERROR (0x48).
type Error struct {
	Req  uint64
	Code uint32
}

func (m Error) Append(b []byte) []byte {
	b = pbUint(b, 1, m.Req)
	return pbUint(b, 2, uint64(m.Code))
}

func ParseError(p []byte) (Error, error) {
	var m Error
	err := pbWalk(p, func(num protowire.Number, typ protowire.Type, v uint64, _ []byte) error {
		if typ != protowire.VarintType {
			return malformed("error")
		}
		switch num {
		case 1:
			m.Req = v
		case 2:
			if v > math.MaxUint32 {
				return malformed("error.code")
			}
			m.Code = uint32(v)
		}
		return nil
	})
	return m, err
}

// ---- protobuf helpers (canonical encoding, unknown fields skipped) --------------------------

func pbUint(b []byte, num protowire.Number, v uint64) []byte {
	if v == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func pbBytes(b []byte, num protowire.Number, s []byte) []byte {
	if len(s) == 0 {
		return b
	}
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, s)
}

func pbBool(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func parseBool(typ protowire.Type, v uint64) (bool, bool) {
	if typ != protowire.VarintType {
		return false, false
	}
	switch v {
	case 0:
		return false, true
	case 1:
		return true, true
	}
	return false, false
}

func pbWalk(p []byte, fn func(num protowire.Number, typ protowire.Type, v uint64, s []byte) error) error {
	for len(p) > 0 {
		num, typ, n := protowire.ConsumeTag(p)
		if n < 0 {
			return malformed("protobuf tag")
		}
		p = p[n:]
		var v uint64
		var s []byte
		switch typ {
		case protowire.VarintType:
			v, n = protowire.ConsumeVarint(p)
		case protowire.BytesType:
			s, n = protowire.ConsumeBytes(p)
		case protowire.Fixed32Type:
			var x uint32
			x, n = protowire.ConsumeFixed32(p)
			v = uint64(x)
		case protowire.Fixed64Type:
			v, n = protowire.ConsumeFixed64(p)
		default:
			return malformed("protobuf wire type")
		}
		if n < 0 {
			return malformed("protobuf value")
		}
		p = p[n:]
		if err := fn(num, typ, v, s); err != nil {
			return err
		}
	}
	return nil
}
