// Package senderkeys encrypts group messages once for the whole group, the way Signal's sender
// keys do, and makes every message carry its author's signature.
//
// Without it a group message is encrypted separately for every member over that member's own
// Double Ratchet session. That stays the transport (the envelopes below travel inside those
// sessions), but a message sealed here is the same bytes for everybody, so any member can hand it
// on to a member the author could not reach, and the signature inside lets the receiver check who
// wrote it whoever handed it over: neither a relaying member nor an admin passing history to a
// newcomer can put words in somebody's mouth.
//
// Keys: an epoch is a 32-byte secret made by an admin and handed to every member. Each sender
// derives its own chain from it (HKDF with the sender's ID) and turns the chain once per message:
// the message key is HMAC(chain, 1), the next chain HMAC(chain, 2), and used keys are forgotten. A
// new epoch is made when somebody leaves or is removed, so they cannot read what comes after.
// Holding an epoch secret lets one derive every chain of that epoch from its start, so forward
// secrecy inside an epoch is only as good as how soon the secret is replaced; epochs also run out
// with time (MaxEpochAge).
package senderkeys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// MaxSkip bounds the message keys kept for messages of a chain that have not arrived yet
	// (they come late or out of order when other members hand them on).
	MaxSkip = 2000
	// MaxEpochAge is how long an epoch is used for sending before an admin replaces it.
	MaxEpochAge = 7 * 24 * time.Hour
	// KeepReplacedFor is how long a member keeps an epoch after a newer one replaced it: a message
	// written in it can still arrive that late (an outbox keeps trying for a week).
	KeepReplacedFor = 7 * 24 * time.Hour
	// MaxEpochs bounds the epochs kept, whatever their age.
	MaxEpochs = 64
)

var (
	ErrUnknownEpoch = errors.New("senderkeys: epoch not known")
	ErrBadEnvelope  = errors.New("senderkeys: envelope cannot be opened")
	ErrOldMessage   = errors.New("senderkeys: message key already used or too old")
	ErrTooFarAhead  = errors.New("senderkeys: message too far ahead of its chain")
	ErrBadSignature = errors.New("senderkeys: signature does not verify")
	ErrBadEpoch     = errors.New("senderkeys: epoch is malformed")
)

// randReader is where secrets come from; tests swap it to make generation fail.
var randReader io.Reader = rand.Reader

// EpochID names an epoch: its number and a tag from its secret, so two admins who made an epoch
// with the same number at the same time are told apart.
type EpochID struct {
	N   uint64 `json:"n"`
	Tag string `json:"t"` // hex of 8 bytes
}

func (id EpochID) String() string { return fmt.Sprintf("%d.%s", id.N, id.Tag) }

// After tells whether id is the newer of the two: the higher number, or with the same number the
// smaller tag, so every member picks the same one without talking.
func (id EpochID) After(o EpochID) bool {
	if id.N != o.N {
		return id.N > o.N
	}
	return id.Tag < o.Tag
}

// Epoch is one group secret with who made it and for whom.
type Epoch struct {
	Group     string   `json:"g"`
	N         uint64   `json:"n"`
	Secret    []byte   `json:"k"`
	Creator   string   `json:"c"` // peer ID (hex) of the admin who made it
	CreatedAt int64    `json:"at"`
	Members   []string `json:"m"` // peer IDs it was made for, sorted
	Sig       []byte   `json:"s"` // the creator's identity signature over SignedBytes
}

func tagOf(group string, n uint64, secret []byte) string {
	h := sha256.New()
	h.Write([]byte("echofog-gsk-tag-v1"))
	writeStr(h, group)
	_ = binary.Write(h, binary.BigEndian, n)
	h.Write(secret)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ID is the epoch's identity.
func (e *Epoch) ID() EpochID { return EpochID{N: e.N, Tag: tagOf(e.Group, e.N, e.Secret)} }

// SignedBytes is what the creator signs: everything but the secret itself (the tag stands for it).
func (e *Epoch) SignedBytes() []byte {
	var b bytes.Buffer
	b.WriteString("echofog-gsk-epoch-v1")
	writeStr(&b, e.Group)
	_ = binary.Write(&b, binary.BigEndian, e.N)
	writeStr(&b, e.ID().Tag)
	writeStr(&b, e.Creator)
	_ = binary.Write(&b, binary.BigEndian, e.CreatedAt)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(e.Members)))
	for _, m := range e.Members {
		writeStr(&b, m)
	}
	return b.Bytes()
}

// NewEpoch makes epoch n of group for members, signed with sign (the creator's identity key).
func NewEpoch(group string, n uint64, creator string, members []string, now time.Time, sign func([]byte) ([]byte, error)) (*Epoch, error) {
	secret := make([]byte, 32)
	if _, err := io.ReadFull(randReader, secret); err != nil {
		return nil, err
	}
	ms := append([]string(nil), members...)
	sort.Strings(ms)
	e := &Epoch{Group: group, N: n, Secret: secret, Creator: creator, CreatedAt: now.UnixMilli(), Members: ms}
	sig, err := sign(e.SignedBytes())
	if err != nil {
		return nil, err
	}
	e.Sig = sig
	return e, nil
}

// Verify checks the epoch's shape and its creator's signature.
func (e *Epoch) Verify(creatorPub ed25519.PublicKey) error {
	if e == nil || e.Group == "" || e.N == 0 || len(e.Secret) != 32 || e.Creator == "" || len(creatorPub) != ed25519.PublicKeySize {
		return ErrBadEpoch
	}
	if !sort.StringsAreSorted(e.Members) {
		return ErrBadEpoch
	}
	if !ed25519.Verify(creatorPub, e.SignedBytes(), e.Sig) {
		return ErrBadSignature
	}
	return nil
}

// Has tells whether the epoch was made with peer among its members.
func (e *Epoch) Has(peer string) bool {
	i := sort.SearchStrings(e.Members, peer)
	return i < len(e.Members) && e.Members[i] == peer
}

// chainStart is the first chain key of sender in epoch e.
func chainStart(e *Epoch, sender string) []byte {
	info := append([]byte("echofog-gsk-chain-v1|"), sender...)
	r := hkdf.New(sha256.New, e.Secret, []byte(e.Group), info)
	out := make([]byte, 32)
	_, _ = io.ReadFull(r, out) // HKDF-SHA256 gives far more than 32 bytes
	return out
}

func hm(key []byte, b byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte{b})
	return m.Sum(nil)
}

// step turns a chain once: the key of this message and the chain for the next one.
func step(chain []byte) (msgKey, next []byte) { return hm(chain, 1), hm(chain, 2) }

func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Envelope is one sealed group message as it travels, the same bytes for every member.
type Envelope struct {
	Group  string `json:"g"`
	Epoch  uint64 `json:"e"`
	Tag    string `json:"t"`
	Sender string `json:"s"`
	N      uint32 `json:"n"`
	Nonce  []byte `json:"nc"`
	CT     []byte `json:"c"`
}

// EpochID of the epoch the envelope was sealed in.
func (v *Envelope) EpochID() EpochID { return EpochID{N: v.Epoch, Tag: v.Tag} }

// header is the additional data of the seal: everything in the envelope but the ciphertext.
func (v *Envelope) header() []byte {
	var b bytes.Buffer
	b.WriteString("echofog-gsk-msg-v1")
	writeStr(&b, v.Group)
	_ = binary.Write(&b, binary.BigEndian, v.Epoch)
	writeStr(&b, v.Tag)
	writeStr(&b, v.Sender)
	_ = binary.Write(&b, binary.BigEndian, v.N)
	return b.Bytes()
}

// Every message key should seal one message only, but the nonce is random all the same: should a
// crash lose the turned chain before it is saved, the same key would seal a second message, and
// a random 24-byte nonce keeps that from repeating a keystream.
func seal(key []byte, v *Envelope, plain []byte) error {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	v.Nonce = make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := io.ReadFull(randReader, v.Nonce); err != nil {
		return err
	}
	v.CT = aead.Seal(nil, v.Nonce, plain, v.header())
	return nil
}

func open(key []byte, v *Envelope) ([]byte, error) {
	if len(v.Nonce) != chacha20poly1305.NonceSizeX {
		return nil, ErrBadEnvelope
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, v.Nonce, v.CT, v.header())
	if err != nil {
		return nil, ErrBadEnvelope
	}
	return plain, nil
}

// Content is what a member writes, signed by them. The signature binds the group, the message ID
// and time, the author and the text, so the message can be checked wherever it is found: in an
// envelope, handed on by another member, or in history an admin passes to a newcomer.
type Content struct {
	Group  string `json:"g"`
	ID     string `json:"i"`
	TS     int64  `json:"ts"`
	Sender string `json:"s"`
	Text   string `json:"t"`
	// Extra is opaque data the app signs along (a file offer); FileDigest is its SHA-256, which
	// is what the signature covers, so history can carry the digest without the data.
	Extra      []byte `json:"x,omitempty"`
	FileDigest string `json:"fd,omitempty"`
	Sig        []byte `json:"sig"`
}

// SignedBytes is what the author signs.
func (c *Content) SignedBytes() []byte {
	var b bytes.Buffer
	b.WriteString("echofog-gsk-content-v1")
	writeStr(&b, c.Group)
	writeStr(&b, c.ID)
	_ = binary.Write(&b, binary.BigEndian, c.TS)
	writeStr(&b, c.Sender)
	writeStr(&b, c.Text)
	writeStr(&b, c.digest())
	return b.Bytes()
}

func (c *Content) digest() string {
	if len(c.Extra) > 0 {
		sum := sha256.Sum256(c.Extra)
		return hex.EncodeToString(sum[:])
	}
	return c.FileDigest
}

// Sign signs c with sign (the author's identity key) and fills FileDigest from Extra.
func (c *Content) Sign(sign func([]byte) ([]byte, error)) error {
	c.FileDigest = c.digest()
	sig, err := sign(c.SignedBytes())
	if err != nil {
		return err
	}
	c.Sig = sig
	return nil
}

// Verify checks the author's signature; when Extra is present it must match FileDigest.
func (c *Content) Verify(authorPub ed25519.PublicKey) error {
	if len(authorPub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	if len(c.Extra) > 0 && c.FileDigest != "" {
		sum := sha256.Sum256(c.Extra)
		if hex.EncodeToString(sum[:]) != c.FileDigest {
			return ErrBadSignature
		}
	}
	if !ed25519.Verify(authorPub, c.SignedBytes(), c.Sig) {
		return ErrBadSignature
	}
	return nil
}

func writeStr(w io.Writer, s string) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(s)))
	_, _ = w.Write(l[:])
	_, _ = io.WriteString(w, s)
}
