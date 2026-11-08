package main

// Vectors of the conversation log (convlog.json): signed events of kind MSG with an attachment
// (MsgBody field 6), kind group.close (10), and a negative event whose attachment was swapped under
// the original signature. Written from conversation-log.md §2.2 and §2.5, not from the Go code: the
// implementation's test (internal/services/convlog, convlog_vectors_test.go) checks these bytes.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// convSeed derives a named seed as every vector file does.
func convSeed(name string) []byte {
	h := sha256.Sum256([]byte("echofog v2 test vector seed: " + name))
	return h[:]
}

// lp is the length-prefixed form used by the signed bytes: u32 big-endian length, then the bytes.
func lp(b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

// convEvent holds the fields of one event as the protocol names them.
type convEvent struct {
	conv, author  []byte
	seq, lamport  uint64
	tsMs          uint64
	kind          uint32
	target, reply []byte
	thread        []byte
	body, ext     []byte
	sig           []byte
}

// signedBytes is conversation-log.md §2.2: the domain, then the fields in this order, with
// bytes fields length-prefixed and the body replaced by SHA-256(body).
func (e convEvent) signedBytes() []byte {
	bh := sha256.Sum256(e.body)
	out := []byte("echofog-ev-sig-v1")
	out = append(out, lp(e.conv)...)
	out = append(out, lp(e.author)...)
	out = binary.BigEndian.AppendUint64(out, e.seq)
	out = binary.BigEndian.AppendUint64(out, e.lamport)
	out = binary.BigEndian.AppendUint64(out, e.tsMs)
	out = binary.BigEndian.AppendUint32(out, e.kind)
	out = append(out, lp(e.target)...)
	out = append(out, lp(e.reply)...)
	out = append(out, lp(e.thread)...)
	out = append(out, lp(bh[:])...)
	out = append(out, lp(e.ext)...)
	return out
}

// encode is the protobuf Event (fields in ascending order, proto3 defaults omitted).
func (e convEvent) encode() []byte {
	p := &pb{}
	p.by(1, e.conv).by(2, e.author).u(3, e.seq).u(4, e.lamport).u(5, e.tsMs).u(6, uint64(e.kind))
	p.by(7, e.target).by(8, e.reply).by(9, e.thread).by(10, e.body).by(11, e.ext).by(15, e.sig)
	return p.b
}

// id is SHA-256("echofog-ev-v1" ‖ signed bytes ‖ sig)[:16].
func (e convEvent) id() []byte {
	h := sha256.New()
	h.Write([]byte("echofog-ev-v1"))
	h.Write(e.signedBytes())
	h.Write(e.sig)
	return h.Sum(nil)[:16]
}

// msgBody is MsgBody: text (1) and an optional attachment (6).
func msgBody(text string, fid, root []byte, size uint64, name, mime string) []byte {
	att := &pb{}
	att.by(1, fid).u(2, size).by(3, root).s(4, name).s(5, mime)
	body := &pb{}
	body.s(1, text).by(6, att.b)
	return body.b
}

func genConvlog() any {
	author := newSigner(convSeed("convlog author"))
	conv := convSeed("convlog conversation")[:16]
	fid := convSeed("convlog file fid")[:16]
	root := convSeed("convlog file merkle root")
	attackerFid := convSeed("convlog attacker file fid")[:16]

	msg := convEvent{
		conv: conv, author: author.pub, seq: 7, lamport: 12, tsMs: 1793664000000, kind: 1,
		body: msgBody("вот схема", fid, root, 4096, "схема.pdf", "application/pdf"),
		ext:  (&pb{}).s(1, "files").b, // EventExt.channel
	}
	msg.sig = ed25519.Sign(author.priv, msg.signedBytes())

	closing := convEvent{conv: conv, author: author.pub, seq: 8, lamport: 13, tsMs: 1793664060000, kind: 10}
	closing.sig = ed25519.Sign(author.priv, closing.signedBytes())

	// the same event with another file in its attachment, still carrying the original signature
	swapped := msg
	swapped.body = msgBody("вот схема", attackerFid, root, 4096, "схема.pdf", "application/pdf")

	ev := func(name, desc string, e convEvent) map[string]any {
		return map[string]any{
			"name": name, "description": desc,
			"event": hx(e.encode()), "signed_bytes": hx(e.signedBytes()),
			"body": hx(e.body), "body_sha256": hx(sha256Of(e.body)), "ext": hx(e.ext),
			"sig": hx(e.sig), "id": hx(e.id()),
		}
	}
	// group.close with a vector of last seq per author (conversation-log.md §2.5): the owner's
	// seq 7 (the msg-att event) and a member's seq 3, sorted by author public key.
	member := newSigner(convSeed("convlog member"))
	entries := []seqEntry{{author.pub, 7}, {member.pub, 3}}
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].author, entries[j].author) < 0 })
	vec := convEvent{conv: conv, author: author.pub, seq: 9, lamport: 14, tsMs: 1793664120000, kind: 10,
		body: closeBody(entries)}
	vec.sig = ed25519.Sign(author.priv, vec.signedBytes())
	closeVec := ev("group-close-vector", "group.close with CloseBody: owner seq 7, member seq 3 (sorted by author)", vec)
	last := []any{}
	for _, en := range entries {
		last = append(last, map[string]any{"author": hx(en.author), "seq": en.seq})
	}
	closeVec["last_seq"] = last

	return map[string]any{
		"description": "Conversation-log events (conversation-log.md §2.2, §2.5, versioning §3.9). " +
			"Positive: an MSG with an attachment (body field 6), a group.close (kind 10). " +
			"Negative: the MSG with another file in its attachment under the original signature, " +
			"which the signature does not cover and which MUST be rejected. Clock: 1793664000 s.",
		"seeds": map[string]string{
			"convlog author":            hx(convSeed("convlog author")),
			"convlog conversation":      hx(convSeed("convlog conversation")),
			"convlog file fid":          hx(convSeed("convlog file fid")),
			"convlog file merkle root":  hx(convSeed("convlog file merkle root")),
			"convlog attacker file fid": hx(convSeed("convlog attacker file fid")),
		},
		"author_public_key": hx(author.pub),
		"conv":              hx(conv),
		"events": []any{
			ev("msg-att", "MSG with att: fid, size 4096, Merkle root, name, mime; ext channel \"files\"", msg),
			ev("group-close", "group.close (kind 10), empty body, lamport 13, owner-signed", closing),
			closeVec,
		},
		"negative": []any{
			map[string]any{
				"name":         "att-not-covered",
				"description":  "msg-att with its attachment fid replaced by another file's; the original signature is kept. MUST be rejected: bad signature.",
				"event":        hx(swapped.encode()),
				"signed_bytes": hx(swapped.signedBytes()),
				"body":         hx(swapped.body),
				"sig":          hx(swapped.sig),
				"reason":       "bad signature",
			},
		},
	}
}

func sha256Of(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

type seqEntry struct {
	author []byte
	seq    uint64
}

// closeBody is CloseBody: one SeqEntry per author (field 1, repeated), each {author 1, seq 2}.
func closeBody(entries []seqEntry) []byte {
	p := &pb{}
	for _, en := range entries {
		e := &pb{}
		e.by(1, en.author).u(2, en.seq)
		p.by(1, e.b)
	}
	return p.b
}
