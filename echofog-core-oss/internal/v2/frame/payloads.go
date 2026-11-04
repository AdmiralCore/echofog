package frame

import (
	"encoding/binary"
	"net/netip"
	"unicode/utf8"
)

// cursor reads the fields of a byte-layout payload; every error is ErrMalformed.
type cursor struct {
	b   []byte
	err error
}

func (c *cursor) varint(what string) uint64 {
	if c.err != nil {
		return 0
	}
	v, n, err := ParseVarint(c.b)
	if err != nil {
		c.err = malformed(what + ": " + err.Error())
		return 0
	}
	c.b = c.b[n:]
	return v
}

func (c *cursor) bytes(n int, what string) []byte {
	if c.err != nil {
		return nil
	}
	if len(c.b) < n {
		c.err = malformed(what + ": short")
		return nil
	}
	v := c.b[:n:n]
	c.b = c.b[n:]
	return v
}

func (c *cursor) u8(what string) byte {
	b := c.bytes(1, what)
	if b == nil {
		return 0
	}
	return b[0]
}

// end checks that the payload was consumed exactly.
func (c *cursor) end(what string) error {
	if c.err == nil && len(c.b) != 0 {
		c.err = malformed(what + ": trailing bytes")
	}
	return c.err
}

// OpenFlagEarly: the stream was opened in 0-RTT data (wire.md §6.3).
const OpenFlagEarly byte = 0x01

// Open is the payload of OPEN: service(varint) ‖ version(varint) ‖ flags(u8).
type Open struct {
	Service uint64
	Version uint64
	Flags   byte
}

// Early reports the EARLY flag.
func (o Open) Early() bool { return o.Flags&OpenFlagEarly != 0 }

func (o Open) Append(b []byte) []byte {
	b = AppendVarint(b, o.Service)
	b = AppendVarint(b, o.Version)
	return append(b, o.Flags)
}

// ParseOpen decodes an OPEN payload. Reserved flag bits are kept, not rejected: they are sent as
// 0 and ignored on receipt (wire.md §6.3).
func ParseOpen(p []byte) (Open, error) {
	c := cursor{b: p}
	o := Open{Service: c.varint("open service"), Version: c.varint("open version"), Flags: c.u8("open flags")}
	return o, c.end("open")
}

// Unsupported is the payload of UNSUPPORTED: the versions the receiver serves; 0,0 = none.
type Unsupported struct{ Service, Min, Max uint64 }

func (u Unsupported) Append(b []byte) []byte {
	return AppendVarint(AppendVarint(AppendVarint(b, u.Service), u.Min), u.Max)
}

func ParseUnsupported(p []byte) (Unsupported, error) {
	c := cursor{b: p}
	u := Unsupported{Service: c.varint("unsupported service"), Min: c.varint("unsupported min"), Max: c.varint("unsupported max")}
	return u, c.end("unsupported")
}

// MaxCloseReason is the longest CLOSE reason in bytes (wire.md §6.3).
const MaxCloseReason = 200

// Close is the payload of CLOSE: code(varint) ‖ reason (UTF-8, ≤ 200 bytes).
type Close struct {
	Code   uint64
	Reason string
}

// Append encodes c. A reason longer than 200 bytes is cut at a rune boundary.
func (c Close) Append(b []byte) []byte {
	r := c.Reason
	if len(r) > MaxCloseReason {
		r = r[:MaxCloseReason]
		for !utf8.ValidString(r) {
			r = r[:len(r)-1]
		}
	}
	return append(AppendVarint(b, c.Code), r...)
}

func ParseClose(p []byte) (Close, error) {
	c := cursor{b: p}
	code := c.varint("close code")
	if c.err != nil {
		return Close{}, c.err
	}
	if len(c.b) > MaxCloseReason {
		return Close{}, malformed("close reason too long")
	}
	if !utf8.Valid(c.b) {
		return Close{}, malformed("close reason not UTF-8")
	}
	return Close{Code: code, Reason: string(c.b)}, nil
}

// ParsePing decodes the 8 opaque bytes of PING, PONG and PUNCH_SYNC.
func ParsePing(p []byte) ([8]byte, error) {
	var v [8]byte
	if len(p) != 8 {
		return v, malformed("ping: want 8 bytes")
	}
	copy(v[:], p)
	return v, nil
}

// PathBind is the payload of PATH_BIND: sid(16) ‖ path_id(varint) ‖ mac(32) (wire.md §8.2).
type PathBind struct {
	SID    [16]byte
	PathID uint64
	MAC    [32]byte
}

func (p PathBind) Append(b []byte) []byte {
	b = append(b, p.SID[:]...)
	b = AppendVarint(b, p.PathID)
	return append(b, p.MAC[:]...)
}

func ParsePathBind(p []byte) (PathBind, error) {
	c := cursor{b: p}
	var v PathBind
	copy(v.SID[:], c.bytes(16, "path_bind sid"))
	v.PathID = c.varint("path_bind path_id")
	copy(v.MAC[:], c.bytes(32, "path_bind mac"))
	return v, c.end("path_bind")
}

// ParsePathBindOK decodes PATH_BIND_OK: path_id(varint).
func ParsePathBindOK(p []byte) (uint64, error) {
	c := cursor{b: p}
	v := c.varint("path_bind_ok")
	return v, c.end("path_bind_ok")
}

// PathAbandon is the payload of PATH_ABANDON: path_id(varint) ‖ code(varint).
type PathAbandon struct{ PathID, Code uint64 }

func (p PathAbandon) Append(b []byte) []byte { return AppendVarint(AppendVarint(b, p.PathID), p.Code) }

func ParsePathAbandon(p []byte) (PathAbandon, error) {
	c := cursor{b: p}
	v := PathAbandon{PathID: c.varint("path_abandon id"), Code: c.varint("path_abandon code")}
	return v, c.end("path_abandon")
}

// AppendObservedAddr encodes family(u8: 4 or 6) ‖ address(4 or 16) ‖ port(u16).
func AppendObservedAddr(b []byte, ap netip.AddrPort) []byte {
	a := ap.Addr() // callers Unmap v4-in-v6 addresses; the encoding keeps the family as given
	if a.Is4() {
		b = append(b, 4)
		v := a.As4()
		b = append(b, v[:]...)
	} else {
		b = append(b, 6)
		v := a.As16()
		b = append(b, v[:]...)
	}
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

// ParseObservedAddr decodes OBSERVED_ADDR (also the address form of HsResp.observed, PEER_ADDR
// and PUNCH_OFFER entries).
func ParseObservedAddr(p []byte) (netip.AddrPort, error) {
	c := cursor{b: p}
	var a netip.Addr
	switch c.u8("observed family") {
	case 4:
		if v := c.bytes(4, "observed v4"); v != nil {
			a = netip.AddrFrom4([4]byte(v))
		}
	case 6:
		if v := c.bytes(16, "observed v6"); v != nil {
			a = netip.AddrFrom16([16]byte(v))
		}
	default:
		if c.err == nil {
			c.err = malformed("observed family")
		}
	}
	port := c.bytes(2, "observed port")
	if err := c.end("observed"); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(a, binary.BigEndian.Uint16(port)), nil
}

// MaxMuxData is the largest data field of one MUX_STREAM frame (wire.md §5.2).
const MaxMuxData = 16384

// MuxFlagFin closes the sender's direction of a mux stream.
const MuxFlagFin byte = 0x01

// MuxStream is the payload of MUX_STREAM: stream_id(varint) ‖ flags(u8) ‖ data.
type MuxStream struct {
	StreamID uint64
	Flags    byte
	Data     []byte
}

func (m MuxStream) Fin() bool { return m.Flags&MuxFlagFin != 0 }

func (m MuxStream) Append(b []byte) []byte {
	b = AppendVarint(b, m.StreamID)
	b = append(b, m.Flags)
	return append(b, m.Data...)
}

// ParseMuxStream decodes MUX_STREAM. Data longer than 16384 bytes is malformed. Reserved flag
// bits are ignored on receipt (wire.md §5.2).
func ParseMuxStream(p []byte) (MuxStream, error) {
	c := cursor{b: p}
	m := MuxStream{StreamID: c.varint("mux_stream id"), Flags: c.u8("mux_stream flags")}
	if c.err != nil {
		return MuxStream{}, c.err
	}
	if len(c.b) > MaxMuxData {
		return MuxStream{}, malformed("mux_stream data > 16384")
	}
	m.Data = c.b
	return m, nil
}

// MuxReset is the payload of MUX_RESET: stream_id(varint) ‖ code(varint).
type MuxReset struct{ StreamID, Code uint64 }

func (m MuxReset) Append(b []byte) []byte { return AppendVarint(AppendVarint(b, m.StreamID), m.Code) }

func ParseMuxReset(p []byte) (MuxReset, error) {
	c := cursor{b: p}
	m := MuxReset{StreamID: c.varint("mux_reset id"), Code: c.varint("mux_reset code")}
	return m, c.end("mux_reset")
}

// MuxWindow is the payload of MUX_WINDOW: stream_id(varint) ‖ increment(varint).
type MuxWindow struct{ StreamID, Increment uint64 }

func (m MuxWindow) Append(b []byte) []byte {
	return AppendVarint(AppendVarint(b, m.StreamID), m.Increment)
}

func ParseMuxWindow(p []byte) (MuxWindow, error) {
	c := cursor{b: p}
	m := MuxWindow{StreamID: c.varint("mux_window id"), Increment: c.varint("mux_window increment")}
	return m, c.end("mux_window")
}

// ParseMuxMaxData decodes MUX_MAX_DATA: increment(varint).
func ParseMuxMaxData(p []byte) (uint64, error) {
	c := cursor{b: p}
	v := c.varint("mux_max_data")
	return v, c.end("mux_max_data")
}
