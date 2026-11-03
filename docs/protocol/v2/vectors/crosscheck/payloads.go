package main

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ---- minimal protobuf writer (proto3, fields appended in ascending order by the caller) -------

type pbw []byte

func (p pbw) varintField(field int, v uint64) pbw {
	if v == 0 {
		return p // proto3: default values are not sent
	}
	p = binary.AppendUvarint(p, uint64(field)<<3)
	return binary.AppendUvarint(p, v)
}

func (p pbw) bytesField(field int, b []byte) pbw {
	if len(b) == 0 {
		return p
	}
	p = binary.AppendUvarint(p, uint64(field)<<3|2)
	p = binary.AppendUvarint(p, uint64(len(b)))
	return append(p, b...)
}

// msgField writes an embedded message even when empty would be skipped by bytesField; our
// messages are never empty, so it is the same.
func (p pbw) msgField(field int, b []byte) pbw { return p.bytesField(field, b) }

func (p pbw) packedField(field int, vs ...uint64) pbw {
	var inner []byte
	for _, v := range vs {
		inner = binary.AppendUvarint(inner, v)
	}
	return p.bytesField(field, inner)
}

// pbFields scans a protobuf message and returns the length-delimited and varint fields by number
// (last value wins; enough for the checks this cross-check makes).
func pbFields(b []byte) (map[int][]byte, map[int]uint64, error) {
	ld := map[int][]byte{}
	vi := map[int]uint64{}
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, nil, errors.New("bad tag")
		}
		b = b[n:]
		field, wt := int(tag>>3), tag&7
		switch wt {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, nil, errors.New("bad varint")
			}
			vi[field] = v
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, nil, errors.New("bad length")
			}
			ld[field] = b[n : n+int(l)]
			b = b[n+int(l):]
		default:
			return nil, nil, errors.New("unexpected wire type")
		}
	}
	return ld, vi, nil
}

// ---- Payloads of handshake.md §7, as the vector file describes them ---------------------------

// hsInit builds HsInit. withAccess / withKnownSeq select the XK-only fields.
func hsInit(access []byte, knownSeq uint64) []byte {
	p := pbw(nil).varintField(1, 200).packedField(2, 1, 3, 4, 5, 0x2a2a)
	for id := uint64(1); id <= 8; id++ {
		p = p.msgField(3, pbw(nil).varintField(1, id).packedField(2, 2))
	}
	p = p.msgField(3, pbw(nil).varintField(1, 0x3a3a).packedField(2, 1)) // GREASE service, sorted last
	p = p.msgField(4, access)
	p = p.varintField(5, knownSeq)
	p = p.varintField(6, fixedTime)
	return p
}

func accessMsg(passID, proof []byte) []byte {
	return pbw(nil).bytesField(1, passID).bytesField(2, proof)
}

// hsResp builds HsResp; card may be nil.
func hsResp(card []byte) []byte {
	p := pbw(nil).varintField(1, 200).packedField(2, 1, 3, 4, 5)
	for id := uint64(1); id <= 3; id++ {
		p = p.msgField(3, pbw(nil).varintField(1, id).varintField(2, 2))
	}
	p = p.bytesField(4, card)
	p = p.bytesField(8, join([]byte{4}, []byte{192, 0, 2, 33}, u16be(51234)))
	p = p.varintField(9, fixedTime)
	return p
}

func hsFinish(card []byte) []byte { return pbw(nil).bytesField(1, card) }

// ---- access proof (§5) ------------------------------------------------------------------------

func passID(S []byte) []byte { return hmacSHA(S, []byte("echofog/hs2/pass-id"))[:8] }

func accessProof(S, h1 []byte) []byte {
	return hmacSHA(S, []byte("echofog/hs2/access-proof"), h1)
}

// ---- identities from seeds --------------------------------------------------------------------

// deviceID is SHA-256 of the algorithm-tagged Ed25519 key (README.md §5.2: tag 0x01).
func deviceID(edSeed []byte) []byte {
	pub := ed25519.NewKeyFromSeed(edSeed).Public().(ed25519.PublicKey)
	return sha([]byte{0x01}, pub)
}

// accessSecret is the 16-byte S of the invitation (objects.md §10): the vectors take the first 16
// bytes of seed "link/access_secret".
func accessSecret() []byte { return seed("link/access_secret")[:16] }

// ---- signed card envelope, just enough to read hs_key (objects.md §1) --------------------------

// cardHSKey returns NodeCard field 2 (hs_key, algorithm-tagged) from an envelope:
// version(1) ‖ label_len(1) ‖ label ‖ signer alg(1) ‖ signer(32) ‖ seq(8) ‖ not_after(8) ‖
// body_len(varint) ‖ body ‖ sig(64).
func cardHSKey(env []byte) ([]byte, error) {
	if len(env) < 2 {
		return nil, errors.New("short envelope")
	}
	off := 2 + int(env[1]) + 1 + 32 + 8 + 8
	if len(env) < off+1 {
		return nil, errors.New("short envelope")
	}
	b := env[off:]
	l := uint64(b[0] & 0x3f)
	nl := 1 << (b[0] >> 6)
	for i := 1; i < nl; i++ {
		l = l<<8 | uint64(b[i])
	}
	if uint64(len(b)-nl) < l {
		return nil, errors.New("short body")
	}
	ld, _, err := pbFields(b[nl : nl+int(l)])
	if err != nil {
		return nil, err
	}
	return ld[2], nil
}

// ---- reading the existing vector files --------------------------------------------------------

// vectorDir is the directory holding handshake.json etc. (parent of this module).
var vectorDir = ".."

func loadJSON(name string) map[string]any {
	b, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	return m
}

// jget walks a decoded JSON object by keys.
func jget(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		cur = cur.(map[string]any)[k]
	}
	return cur.(string)
}

func peerCardEnvelope() []byte { return fromHex(jget(loadJSON("node-card.json"), "peer", "envelope")) }
func publicCardEnvelope() []byte {
	return fromHex(jget(loadJSON("node-card.json"), "public", "envelope"))
}
