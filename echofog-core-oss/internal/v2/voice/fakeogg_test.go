package voice

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// A fake codec: the "packets" are arbitrary bytes, the container is a real Ogg Opus stream
// (RFC 3533, RFC 7845). The tests never decode audio.

var refTab = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

func refCRC(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = c<<8 ^ refTab[byte(c>>24)^x]
	}
	return c
}

const (
	flagBOS = 0x02
	flagEOS = 0x04
)

type fakeOpts struct {
	vendor   string
	comments []string
	rate     uint32
	channels byte
	family   byte
	version  byte
	preskip  uint16
	serial   uint32
	packets  int // 20 ms each
	pktSize  int
	noEOS    bool
	noTags   bool
	tagsPad  int
}

func defaultOpts() fakeOpts {
	return fakeOpts{vendor: "libopus 1.4.0 (Pixel 8, serial R5CT)", comments: []string{"ENCODER=EchoFogDroid", "DATE=2026-11-06T10:15:00"},
		rate: 44100, channels: 1, version: 1, preskip: 312, serial: 0xDEADBEEF, packets: 150, pktSize: 60}
}

func page(typ byte, granule int64, serial, seq uint32, packets ...[]byte) []byte {
	var segs, body []byte
	for _, p := range packets {
		n := len(p)
		for n >= 255 {
			segs = append(segs, 255)
			n -= 255
		}
		segs = append(segs, byte(n))
		body = append(body, p...)
	}
	if len(segs) > 255 {
		panic("too many segments")
	}
	h := make([]byte, 27)
	copy(h, "OggS")
	h[5] = typ
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	binary.LittleEndian.PutUint32(h[14:], serial)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = byte(len(segs))
	out := append(append(h, segs...), body...)
	binary.LittleEndian.PutUint32(out[22:], refCRC(out))
	return out
}

func opusHead(o fakeOpts) []byte {
	h := []byte("OpusHead")
	h = append(h, o.version, o.channels)
	h = binary.LittleEndian.AppendUint16(h, o.preskip)
	h = binary.LittleEndian.AppendUint32(h, o.rate)
	h = binary.LittleEndian.AppendUint16(h, 0)
	return append(h, o.family)
}

func opusTags(o fakeOpts) []byte {
	t := []byte("OpusTags")
	t = binary.LittleEndian.AppendUint32(t, uint32(len(o.vendor)))
	t = append(t, o.vendor...)
	t = binary.LittleEndian.AppendUint32(t, uint32(len(o.comments)))
	for _, c := range o.comments {
		t = binary.LittleEndian.AppendUint32(t, uint32(len(c)))
		t = append(t, c...)
	}
	return append(t, bytes.Repeat([]byte("x"), o.tagsPad)...)
}

// audioPacket i of a clip: recognisable and different for every i.
func audioPacket(i, size int) []byte {
	p := make([]byte, size)
	for k := range p {
		p[k] = byte(i*31 + k*7 + 1)
	}
	return p
}

func buildOgg(o fakeOpts) []byte {
	var out []byte
	seq := uint32(0)
	out = append(out, page(flagBOS, 0, o.serial, seq, opusHead(o))...)
	seq++
	if !o.noTags {
		out = append(out, page(0, 0, o.serial, seq, opusTags(o))...)
		seq++
	}
	const perPage = 50
	for i := 0; i < o.packets; i += perPage {
		end := min(i+perPage, o.packets)
		var ps [][]byte
		for k := i; k < end; k++ {
			ps = append(ps, audioPacket(k, o.pktSize))
		}
		typ := byte(0)
		if end == o.packets && !o.noEOS {
			typ = flagEOS
		}
		out = append(out, page(typ, int64(o.preskip)+int64(end)*960, o.serial, seq, ps...)...)
		seq++
	}
	return out
}

// clip builds a clip of the given length at about 24 kbit/s.
func clip(t testing.TB, seconds int) []byte {
	t.Helper()
	o := defaultOpts()
	o.packets = seconds * 50
	return buildOgg(o)
}

type testPage struct {
	typ     byte
	granule int64
	serial  uint32
	seq     uint32
	body    []byte
}

// splitPages splits an Ogg stream (test helper; the code under test has its own reader).
func splitPages(t testing.TB, b []byte) []testPage {
	t.Helper()
	var out []testPage
	for len(b) > 0 {
		if len(b) < 27 || string(b[:4]) != "OggS" {
			t.Fatal("not an Ogg stream")
		}
		n := int(b[26])
		size := 27 + n
		for _, s := range b[27 : 27+n] {
			size += int(s)
		}
		p := testPage{typ: b[5], granule: int64(binary.LittleEndian.Uint64(b[6:])), serial: binary.LittleEndian.Uint32(b[14:]),
			seq: binary.LittleEndian.Uint32(b[18:]), body: b[27+n : size]}
		out = append(out, p)
		b = b[size:]
	}
	return out
}
