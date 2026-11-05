// Package profile measures the cover site of a reality2 listener and compares what a passive
// observer sees of it with what the node's own TLS server shows (wire.md §3.4).
//
// An observer sees the ServerHello in the clear and everything after it only as record or packet
// lengths. So the unit of comparison is a Vector — the open ServerHello fields plus the sequence
// of (kind, length) of the server's records or packets until the handshake is over and a short
// while after it — never the TLS content and never crypto/tls ConnectionState. Observe turns the
// exact bytes of one connection into a Vector; Aggregate folds several Vectors of one cover into
// a CoverProfile that keeps the range each length showed; Compare lists what a second profile
// does not reproduce. D13 and the lab (T15.0b) use the same Observe and Compare.
//
// Rules of a match (T2.6b-1): discrete items (version, cipher, group, HelloRetryRequest, ALPN,
// extension order, session-id echo length, server SCID length, packing and record kinds, number
// of post-handshake records) must be equal; lengths must lie inside the range the cover itself
// showed over its own samples. An item the cover varies from connection to connection cannot be
// mirrored and is reported as "unstable".
//
// Limits of what is measured, stated so nobody trusts it for more:
//   - Inner handshake messages are not decrypted. The certificate, CertificateVerify and Finished
//     appear only through the record or packet lengths that carry them, which is exactly what an
//     observer has.
//   - Post-handshake records are the server records between the client's Finished and the first
//     request; on TLS 1.3 they are the NewSessionTicket messages. On QUIC they are the server's
//     1-RTT packets in that window and also contain acknowledgements, so they are noisier.
//   - ALPN is read from the client's side of the handshake (it is encrypted on the wire).
package profile

import (
	"crypto/sha256"
	"errors"
)

// Transport selects the cover protocol.
type Transport string

const (
	TCP  Transport = "tcp"
	QUIC Transport = "quic"
)

// Compared items, the names Diff and Unstable use.
const (
	ItemVersion     = "version"
	ItemCipher      = "cipher"
	ItemGroup       = "group"
	ItemHRR         = "hrr"
	ItemALPN        = "alpn"
	ItemExtOrder    = "ext_order"
	ItemSessionID   = "session_id_len"
	ItemSCID        = "scid_len"
	ItemCCS         = "ccs"
	ItemPacking     = "packing"
	ItemFlightLen   = "flight_len"
	ItemMsgs        = "messages"
	ItemMsgLen      = "msg_len"
	ItemTicketCount = "ticket_count"
	ItemTicketLen   = "ticket_len"
)

// Event is one chunk the capture saw: a read from or a write to the connection (TCP) or one
// datagram (QUIC), in the order they happened.
type Event struct {
	FromServer bool
	Data       []byte
}

// Item is one server record (TCP) or packet (QUIC).
type Item struct {
	// Kind is "rec/<content type>" on TCP and initial, 0rtt, handshake, retry, 1rtt or vneg on
	// QUIC.
	Kind string
	// Unit is the index of the datagram that carried a QUIC packet (coalescing is visible through
	// equal Units); zero on TCP.
	Unit int
	Len  int
}

// Vector is what one connection showed to an observer.
type Vector struct {
	Transport Transport
	// Open ServerHello fields (of the final ServerHello when a HelloRetryRequest came first).
	Version      uint16
	Cipher       uint16
	Group        uint16
	HRR          bool
	ExtOrder     []uint16
	SessionIDLen int
	// SCIDLen is the length of the server's source connection ID (QUIC).
	SCIDLen int
	// CCS reports a ChangeCipherSpec record from the server (TCP).
	CCS bool
	// Flight is the server's handshake flight, Post the post-handshake records or 1-RTT packets.
	Flight []Item
	Post   []Item
	// Decrypted reports that Inner and Tickets were read with the key log (TCP only).
	Decrypted bool
	// Inner are the encrypted handshake messages up to Finished (EncryptedExtensions,
	// Certificate, CertificateVerify, Finished) with their exact lengths.
	Inner []Item
	// Tickets are the NewSessionTicket messages, wherever the server put them.
	Tickets []Item
}

var (
	// ErrNoServerHello reports a capture without a parsable ServerHello.
	ErrNoServerHello = errors.New("profile: no ServerHello in the capture")
	// ErrMalformed reports bytes that are not a TLS or QUIC stream.
	ErrMalformed = errors.New("profile: malformed capture")
	// ErrTooBig reports a capture beyond the bounds Observe accepts.
	ErrTooBig = errors.New("profile: capture too big")
)

const (
	maxCaptureBytes = 4 << 20
	maxItems        = 512
)

var helloRetryRequestRandom = sha256.Sum256([]byte("HelloRetryRequest"))
