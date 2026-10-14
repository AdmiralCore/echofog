package x3dh

import (
	"bytes"
	"encoding/gob"
	"errors"

	"github.com/awnumar/memguard"
	"github.com/echofog/echofog-core/pkg/api/types"
)

// sessionFormat changes when a stored session cannot be read by the code any more; such a session is
// dropped and the next message starts a new one.
const sessionFormat = 2

var ErrSessionFormat = errors.New("stored session has an old format")

type x3dhSessionDTO struct {
	Format           int
	RemotePeerID     types.PeerID
	RootKey          []byte
	SendChain        chainStateDTO
	RecvChain        chainStateDTO
	LocalRatchetPub  [32]byte
	LocalRatchetPriv []byte
	RemoteRatchetPub [32]byte
	SkippedKeys      map[[32]byte]map[uint32][]byte
	SkippedOrder     [][32]byte
	IsInitiator      bool
	PrevSendCount    uint32
	AD               []byte
	InitEK           [32]byte
	InitMsgKey       []byte
}

type chainStateDTO struct {
	Key   []byte
	Index uint32
}

func (x *X3DHSession) ToBytes() ([]byte, error) {
	dto := x3dhSessionDTO{
		Format:           sessionFormat,
		RemotePeerID:     x.RemotePeerID,
		SkippedOrder:     x.SkippedOrder,
		PrevSendCount:    x.PrevSendCount,
		AD:               x.AD,
		InitEK:           x.InitEK,
		LocalRatchetPub:  x.LocalRatchetPub,
		RemoteRatchetPub: x.RemoteRatchetPub,
		IsInitiator:      x.IsInitiator,
		SkippedKeys:      make(map[[32]byte]map[uint32][]byte),
	}

	getEnc := func(e *memguard.Enclave) ([]byte, error) {
		if e == nil {
			return nil, nil
		}
		l, err := e.Open()
		if err != nil {
			return nil, err
		}
		defer l.Destroy()
		return append([]byte{}, l.Bytes()...), nil
	}

	var err error
	dto.RootKey, err = getEnc(x.RootKey)
	if err != nil {
		return nil, err
	}
	dto.LocalRatchetPriv, err = getEnc(x.LocalRatchetPriv)
	if err != nil {
		return nil, err
	}
	dto.SendChain.Index = x.SendChain.Index
	dto.SendChain.Key, err = getEnc(x.SendChain.Key)
	if err != nil {
		return nil, err
	}
	dto.InitMsgKey, err = getEnc(x.InitMsgKey)
	if err != nil {
		return nil, err
	}
	dto.RecvChain.Index = x.RecvChain.Index
	dto.RecvChain.Key, err = getEnc(x.RecvChain.Key)
	if err != nil {
		return nil, err
	}

	for dh, msgs := range x.SkippedKeys {
		dto.SkippedKeys[dh] = make(map[uint32][]byte)
		for n, e := range msgs {
			b, err := getEnc(e)
			if err != nil {
				return nil, err
			}
			dto.SkippedKeys[dh][n] = b
		}
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(dto); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func FromBytes(data []byte) (*X3DHSession, error) {
	var dto x3dhSessionDTO
	dec := gob.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&dto); err != nil {
		return nil, err
	}
	if dto.Format != sessionFormat || len(dto.AD) != 64 {
		return nil, ErrSessionFormat
	}

	sealEnc := func(b []byte) *memguard.Enclave {
		if b == nil {
			return nil
		}
		e := memguard.NewEnclave(b)
		memguard.WipeBytes(b)
		return e
	}

	session := &X3DHSession{
		RemotePeerID:     dto.RemotePeerID,
		LocalRatchetPub:  dto.LocalRatchetPub,
		RemoteRatchetPub: dto.RemoteRatchetPub,
		IsInitiator:      dto.IsInitiator,
		SkippedKeys:      make(map[[32]byte]map[uint32]*memguard.Enclave),
		SkippedOrder:     dto.SkippedOrder,
		PrevSendCount:    dto.PrevSendCount,
		AD:               dto.AD,
		InitEK:           dto.InitEK,
	}

	session.RootKey = sealEnc(dto.RootKey)
	session.LocalRatchetPriv = sealEnc(dto.LocalRatchetPriv)
	session.SendChain.Index = dto.SendChain.Index
	session.SendChain.Key = sealEnc(dto.SendChain.Key)
	session.RecvChain.Index = dto.RecvChain.Index
	session.RecvChain.Key = sealEnc(dto.RecvChain.Key)
	session.InitMsgKey = sealEnc(dto.InitMsgKey)

	for dh, msgs := range dto.SkippedKeys {
		session.SkippedKeys[dh] = make(map[uint32]*memguard.Enclave)
		for n, b := range msgs {
			session.SkippedKeys[dh][n] = sealEnc(b)
		}
	}

	return session, nil
}
