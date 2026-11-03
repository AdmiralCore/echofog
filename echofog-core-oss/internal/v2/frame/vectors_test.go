package frame

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The vectors are copied from docs/protocol/v2/vectors into internal/v2/testdata so the module
// tests stand alone (the OSS copy ships without docs). TestVectorCopiesMatchSpec keeps them in sync.
const testdata = "../testdata"

func loadJSON(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testdata, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestVectorCopiesMatchSpec(t *testing.T) {
	spec := filepath.Join("..", "..", "..", "..", "docs", "protocol", "v2", "vectors")
	if _, err := os.Stat(spec); err != nil {
		t.Skip("spec vectors not present (OSS copy)")
	}
	for _, name := range []string{"varint.json", "frames.json", "kdf.json", "negative.json"} {
		a, err := os.ReadFile(filepath.Join(spec, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(testdata, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(a, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("%s: testdata copy differs from docs/protocol/v2/vectors", name)
		}
	}
}

type varintVectors struct {
	Valid []struct {
		Value   uint64 `json:"value"`
		Encoded string `json:"encoded"`
	} `json:"valid"`
	Invalid []struct {
		Encoded string `json:"encoded"`
		Reason  string `json:"reason"`
	} `json:"invalid"`
}

func TestVarintVectors(t *testing.T) {
	var v varintVectors
	loadJSON(t, "varint.json", &v)
	if len(v.Valid) == 0 || len(v.Invalid) == 0 {
		t.Fatal("empty vector file")
	}
	for _, c := range v.Valid {
		want := unhex(t, c.Encoded)
		if got := AppendVarint(nil, c.Value); !bytes.Equal(got, want) {
			t.Errorf("encode %d = %x, want %x", c.Value, got, want)
		}
		if VarintLen(c.Value) != len(want) {
			t.Errorf("VarintLen(%d) = %d, want %d", c.Value, VarintLen(c.Value), len(want))
		}
		got, n, err := ParseVarint(want)
		if err != nil || got != c.Value || n != len(want) {
			t.Errorf("decode %x = %d,%d,%v; want %d", want, got, n, err, c.Value)
		}
		// Truncated input is short, never a different value.
		if _, _, err := ParseVarint(want[:len(want)-1]); err != ErrShort {
			t.Errorf("decode truncated %x: err %v, want ErrShort", want, err)
		}
		r, err := ReadVarint(bytes.NewReader(want))
		if err != nil || r != c.Value {
			t.Errorf("ReadVarint %x = %d,%v", want, r, err)
		}
	}
	for _, c := range v.Invalid {
		in := unhex(t, c.Encoded)
		if _, _, err := ParseVarint(in); err != ErrNonMinimal {
			t.Errorf("decode %x: err %v, want ErrNonMinimal (%s)", in, err, c.Reason)
		}
		if _, err := ReadVarint(bytes.NewReader(in)); err != ErrNonMinimal {
			t.Errorf("ReadVarint %x: err %v, want ErrNonMinimal", in, err)
		}
	}
}

type frameVectors struct {
	Frames []struct {
		Name    string `json:"name"`
		Type    uint64 `json:"type"`
		Flags   byte   `json:"flags"`
		Payload string `json:"payload"`
		Encoded string `json:"encoded"`
	} `json:"frames"`
	Datagrams []struct {
		Name    string `json:"name"`
		Type    uint64 `json:"type"`
		Payload string `json:"payload"`
		Encoded string `json:"encoded"`
	} `json:"datagrams"`
}

func TestFrameVectors(t *testing.T) {
	var v frameVectors
	loadJSON(t, "frames.json", &v)
	if len(v.Frames) == 0 || len(v.Datagrams) == 0 {
		t.Fatal("empty vector file")
	}
	for _, c := range v.Frames {
		t.Run(c.Name, func(t *testing.T) {
			payload := unhex(t, c.Payload)
			want := unhex(t, c.Encoded)
			f := Frame{Type: c.Type, Flags: c.Flags, Payload: payload}
			if got := AppendFrame(nil, f); !bytes.Equal(got, want) {
				t.Fatalf("encode = %x, want %x", got, want)
			}
			// dht/2 FIND_NODE (0x40, max 64) is service-local: it needs the service's limiter.
			var lim Limiter
			if c.Type >= ServiceLocalMin {
				lim = func(typ uint64) (int, bool) { return 64, typ == 0x40 }
			}
			got, n, err := ParseFrame(want, lim)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if n != len(want) || got.Type != c.Type || got.Flags != c.Flags || !bytes.Equal(got.Payload, payload) {
				t.Fatalf("parse = %+v (%d bytes)", got, n)
			}
			// The streaming reader agrees with the slice parser. Unknown ignorable frames are
			// skipped by the reader, so the vector "unknown ignorable" reads as end of input.
			rd := NewReader(bytes.NewReader(want), lim)
			rf, err := rd.ReadFrame()
			if _, known := SessionType(c.Type); !known && c.Type < ServiceLocalMin {
				if err == nil || rd.Skipped() != 1 {
					t.Fatalf("reader: unknown ignorable not skipped: %+v %v", rf, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("reader: %v", err)
			}
			if c.Type != TypePadding && !bytes.Equal(rf.Payload, payload) {
				t.Fatalf("reader payload %x, want %x", rf.Payload, payload)
			}
			// Every known session-level vector also parses as its typed payload and
			// re-encodes to the same bytes.
			if c.Type < ServiceLocalMin {
				checkTypedPayload(t, c.Type, payload)
			}
		})
	}
	for _, c := range v.Datagrams {
		t.Run("dgram "+c.Name, func(t *testing.T) {
			payload := unhex(t, c.Payload)
			want := unhex(t, c.Encoded)
			if got := AppendDatagram(nil, c.Type, payload); !bytes.Equal(got, want) {
				t.Fatalf("encode = %x, want %x", got, want)
			}
			typ, p, err := ParseDatagram(want)
			if err != nil || typ != c.Type || !bytes.Equal(p, payload) {
				t.Fatalf("parse = %d %x %v", typ, p, err)
			}
		})
	}
}

// checkTypedPayload decodes a payload with the typed codec of its frame type and re-encodes it.
func checkTypedPayload(t *testing.T, typ uint64, payload []byte) {
	t.Helper()
	var enc []byte
	switch typ {
	case TypePadding, TypeHsHello, TypeHsMsg, TypeHsRetry, TypeCardUpdate:
		return // opaque at this layer (handshake and objects packages own them)
	case TypeReject:
		m, err := ParseReject(payload)
		if err != nil {
			t.Fatalf("reject: %v", err)
		}
		if m.Reason != 1 || m.Min != 200 || m.Max != 200 {
			t.Fatalf("reject = %+v", m)
		}
		enc = m.Append(nil)
	case TypePing, TypePong:
		p, err := ParsePing(payload)
		if err != nil {
			t.Fatal(err)
		}
		enc = p[:]
	case TypeGoAway:
		m, err := ParseGoAway(payload)
		if err != nil {
			t.Fatal(err)
		}
		if m.Code != CodeGoingAway || m.Reason != "restart" || m.RetryAfter != 30 {
			t.Fatalf("goaway = %+v", m)
		}
		enc = m.Append(nil)
	case TypeClose:
		m, err := ParseClose(payload)
		if err != nil {
			t.Fatal(err)
		}
		if m.Code != CodeServiceUnsupported {
			t.Fatalf("close = %+v", m)
		}
		enc = m.Append(nil)
	case TypeOpen:
		m, err := ParseOpen(payload)
		if err != nil {
			t.Fatal(err)
		}
		enc = m.Append(nil)
	case TypeUnsupported:
		m, err := ParseUnsupported(payload)
		if err != nil {
			t.Fatal(err)
		}
		if m.Service != 9 || m.Min != 0 || m.Max != 0 {
			t.Fatalf("unsupported = %+v", m)
		}
		enc = m.Append(nil)
	case TypeObservedAddr:
		m, err := ParseObservedAddr(payload)
		if err != nil {
			t.Fatal(err)
		}
		if s := m.String(); s != "192.0.2.33:51234" && s != "[2001:db8::1]:443" {
			t.Fatalf("observed = %s", s)
		}
		enc = AppendObservedAddr(nil, m)
	case TypePathBindOK:
		m, err := ParsePathBindOK(payload)
		if err != nil || m != 2 {
			t.Fatalf("path_bind_ok = %d %v", m, err)
		}
		enc = AppendVarint(nil, m)
	case TypePathAbandon:
		m, err := ParsePathAbandon(payload)
		if err != nil || m.PathID != 2 || m.Code != CodePathFailed {
			t.Fatalf("path_abandon = %+v %v", m, err)
		}
		enc = m.Append(nil)
	case TypeMuxStream:
		m, err := ParseMuxStream(payload)
		if err != nil || m.StreamID != 4 || !m.Fin() || string(m.Data) != "hi" {
			t.Fatalf("mux_stream = %+v %v", m, err)
		}
		enc = m.Append(nil)
	case TypeMuxReset:
		m, err := ParseMuxReset(payload)
		if err != nil || m.StreamID != 4 || m.Code != CodeRefused {
			t.Fatalf("mux_reset = %+v %v", m, err)
		}
		enc = m.Append(nil)
	case TypeMuxWindow:
		m, err := ParseMuxWindow(payload)
		if err != nil || m.StreamID != 4 || m.Increment != 131072 {
			t.Fatalf("mux_window = %+v %v", m, err)
		}
		enc = m.Append(nil)
	case TypeMuxMaxData:
		m, err := ParseMuxMaxData(payload)
		if err != nil || m != 2097152 {
			t.Fatalf("mux_max_data = %d %v", m, err)
		}
		enc = AppendVarint(nil, m)
	default:
		if _, ok := SessionType(typ); ok {
			t.Fatalf("no typed check for known type %#x", typ)
		}
		return
	}
	if !bytes.Equal(enc, payload) {
		t.Fatalf("re-encode = %x, want %x", enc, payload)
	}
}

type kdfVectors struct {
	Session struct {
		SID       string `json:"sid"`
		PathID    uint64 `json:"path_id"`
		PathMAC   string `json:"path_mac"`
		PathFrame string `json:"PATH_BIND_frame"`
	} `json:"session"`
}

func TestPathBindVector(t *testing.T) {
	var v kdfVectors
	loadJSON(t, "kdf.json", &v)
	raw := unhex(t, v.Session.PathFrame)
	f, n, err := ParseFrame(raw, nil)
	if err != nil || n != len(raw) || f.Type != TypePathBind {
		t.Fatalf("parse: %+v %v", f, err)
	}
	pb, err := ParsePathBind(f.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pb.SID[:], unhex(t, v.Session.SID)) || pb.PathID != v.Session.PathID || !bytes.Equal(pb.MAC[:], unhex(t, v.Session.PathMAC)) {
		t.Fatalf("path_bind = %+v", pb)
	}
	if got := AppendFrame(nil, Frame{Type: TypePathBind, Payload: pb.Append(nil)}); !bytes.Equal(got, raw) {
		t.Fatalf("re-encode %x", got)
	}
}
