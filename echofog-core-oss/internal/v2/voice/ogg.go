package voice

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Ogg (RFC 3533) holding Opus (RFC 7845). The recorder's encoder fills the headers with things that
// say who recorded: the vendor string ("libopus 1.4 ..."), free-form comments (device, date, program),
// the sample rate of the microphone and a random stream serial. Sanitize rewrites the two header
// packets to a fixed form and renumbers the stream; the audio packets are copied untouched.

const (
	oggFixed    = 27
	flagCont    = 0x01
	flagBegin   = 0x02
	flagEnd     = 0x04
	maxRawClip  = MaxAudioBytes + 4096 // room for the headers of a clip at the size limit
	maxSamples  = 48000 * 3600         // a granule beyond an hour is damage, not a long clip
	opusHeadLen = 19

	maxPacketSamples = 48 * 120 // an Opus packet holds at most 120 ms
)

var oggTab = func() (t [256]uint32) {
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

func oggCRC(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = c<<8 ^ oggTab[byte(c>>24)^x]
	}
	return c
}

type oggPage struct {
	typ     byte
	granule int64
	serial  uint32
	seq     uint32
	segs    []byte
	body    []byte
}

func oggErr(what string) error { return fmt.Errorf("%w: %s", ErrOgg, what) }

// readPage reads one page at the start of b and checks its checksum.
func readPage(b []byte) (oggPage, int, error) {
	var p oggPage
	if len(b) < oggFixed || string(b[:4]) != "OggS" || b[4] != 0 {
		return p, 0, oggErr("page header")
	}
	n := int(b[26])
	if len(b) < oggFixed+n {
		return p, 0, oggErr("truncated page")
	}
	p.segs = b[oggFixed : oggFixed+n]
	size := oggFixed + n
	for _, s := range p.segs {
		size += int(s)
	}
	if len(b) < size {
		return p, 0, oggErr("truncated page")
	}
	want := binary.LittleEndian.Uint32(b[22:])
	raw := append([]byte(nil), b[:size]...)
	raw[22], raw[23], raw[24], raw[25] = 0, 0, 0, 0
	if oggCRC(raw) != want {
		return p, 0, oggErr("checksum")
	}
	p.typ = b[5]
	p.granule = int64(binary.LittleEndian.Uint64(b[6:]))
	p.serial = binary.LittleEndian.Uint32(b[14:])
	p.seq = binary.LittleEndian.Uint32(b[18:])
	p.body = b[oggFixed+n : size]
	return p, size, nil
}

func (p oggPage) bytes() []byte {
	out := make([]byte, oggFixed, oggFixed+len(p.segs)+len(p.body))
	copy(out, "OggS")
	out[5] = p.typ
	binary.LittleEndian.PutUint64(out[6:], uint64(p.granule))
	binary.LittleEndian.PutUint32(out[14:], p.serial)
	binary.LittleEndian.PutUint32(out[18:], p.seq)
	out[26] = byte(len(p.segs))
	out = append(append(out, p.segs...), p.body...)
	binary.LittleEndian.PutUint32(out[22:], oggCRC(out))
	return out
}

// onePacket reports whether the page holds exactly one complete packet.
func (p oggPage) onePacket() bool {
	n := len(p.segs)
	if n == 0 || p.segs[n-1] == 255 {
		return false
	}
	for _, s := range p.segs[:n-1] {
		if s != 255 {
			return false
		}
	}
	return true
}

// headerPage makes a page of one small packet (below 255 bytes).
func headerPage(typ byte, seq uint32, body []byte) oggPage {
	return oggPage{typ: typ, seq: seq, segs: []byte{byte(len(body))}, body: body}
}

// Sanitize checks an Ogg Opus clip (one mono stream, headers first, an end-of-stream page last, the
// pages in order with valid checksums) and rewrites it to its canonical form. It returns the
// clean clip and its length in milliseconds. The audio is not decoded.
func Sanitize(ogg []byte) ([]byte, uint32, error) {
	if len(ogg) == 0 {
		return nil, 0, oggErr("empty")
	}
	if len(ogg) > maxRawClip {
		return nil, 0, ErrTooLarge
	}
	var pages []oggPage
	for rest := ogg; len(rest) > 0; {
		p, n, err := readPage(rest)
		if err != nil {
			return nil, 0, err
		}
		pages = append(pages, p)
		rest = rest[n:]
	}
	if len(pages) < 3 {
		return nil, 0, oggErr("no audio")
	}
	head, tags := pages[0], pages[1]
	if head.typ != flagBegin || !head.onePacket() || head.seq != 0 {
		return nil, 0, oggErr("first page")
	}
	preskip, err := checkHead(head.body)
	if err != nil {
		return nil, 0, err
	}
	if tags.typ != 0 || tags.serial != head.serial || tags.seq != 1 || !tags.onePacket() ||
		len(tags.body) < 16 || string(tags.body[:8]) != "OpusTags" {
		return nil, 0, oggErr("tags page")
	}
	audio := pages[2:]
	last := int64(-1)
	for i, p := range audio {
		switch {
		case p.serial != head.serial || p.seq != uint32(i+2):
			return nil, 0, oggErr("page order")
		case p.typ&^(flagCont|flagEnd) != 0 || (p.typ&flagEnd != 0) != (i == len(audio)-1):
			return nil, 0, oggErr("page flags")
		case p.granule < last || p.granule < 0:
			return nil, 0, oggErr("granule")
		}
		prevOpen := i > 0 && len(audio[i-1].segs) > 0 && audio[i-1].segs[len(audio[i-1].segs)-1] == 255
		if (p.typ&flagCont != 0) != prevOpen {
			return nil, 0, oggErr("continuation")
		}
		last = p.granule
	}
	samples := last - int64(preskip)
	if samples <= 0 || samples > maxSamples {
		return nil, 0, oggErr("length")
	}
	// the packets must hold the audio the granule position claims: a few packets cannot be a long
	// clip. The last packet may be cut at its end, by less than the longest Opus packet.
	held, err := packetSamples(audio)
	if err != nil {
		return nil, 0, err
	}
	if samples > held || held > last+maxPacketSamples {
		return nil, 0, oggErr("length does not match the packets")
	}

	// the canonical headers: Opus version 1, mono, the pre-skip; the input rate, the gain, the
	// channel mapping family, the vendor and the comment count are zero
	h := make([]byte, opusHeadLen)
	copy(h, "OpusHead")
	h[8], h[9] = 1, 1
	binary.LittleEndian.PutUint16(h[10:], preskip)
	t := make([]byte, 16)
	copy(t, "OpusTags")

	out := headerPage(flagBegin, 0, h).bytes()
	out = append(out, headerPage(0, 1, t).bytes()...)
	for i, p := range audio {
		p.serial, p.seq = 0, uint32(i+2)
		out = append(out, p.bytes()...)
	}
	if len(out) > MaxAudioBytes {
		return nil, 0, ErrTooLarge
	}
	return out, uint32(samples / 48), nil
}

// checkHead validates the identification header and returns the pre-skip.
func checkHead(b []byte) (uint16, error) {
	if len(b) != opusHeadLen || !bytes.HasPrefix(b, []byte("OpusHead")) {
		return 0, oggErr("not an Opus stream")
	}
	if b[8] == 0 || b[8]>>4 != 0 {
		return 0, oggErr("Opus header version")
	}
	if b[9] != 1 {
		return 0, oggErr("not mono")
	}
	if b[18] != 0 {
		return 0, oggErr("channel mapping")
	}
	return binary.LittleEndian.Uint16(b[10:]), nil
}

// packetSamples counts the 48 kHz samples in the audio packets, from the TOC byte of each (RFC 6716
// 3.1). Packets run across pages: a segment of 255 bytes continues in the next one.
func packetSamples(audio []oggPage) (int64, error) {
	var total int64
	var head [2]byte
	n := 0 // bytes of the open packet
	for _, p := range audio {
		body := p.body
		for _, s := range p.segs {
			seg := body[:s]
			body = body[s:]
			for i := 0; i < len(seg) && n+i < len(head); i++ {
				head[n+i] = seg[i]
			}
			n += int(s)
			if s == 255 {
				continue
			}
			d, err := packetDuration(head[:min(n, len(head))], n)
			if err != nil {
				return 0, err
			}
			total += d
			n = 0
		}
	}
	if n != 0 {
		return 0, oggErr("unfinished packet")
	}
	return total, nil
}

// packetDuration is the length in 48 kHz samples of a packet whose first bytes are head and whose
// size is size.
func packetDuration(head []byte, size int) (int64, error) {
	if size == 0 {
		return 0, oggErr("empty packet")
	}
	toc := head[0]
	var frame int64
	switch cfg := toc >> 3; {
	case cfg < 12: // SILK: 10, 20, 40, 60 ms
		frame = [4]int64{480, 960, 1920, 2880}[cfg%4]
	case cfg < 16: // hybrid: 10, 20 ms
		frame = [2]int64{480, 960}[cfg%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		frame = [4]int64{120, 240, 480, 960}[cfg%4]
	}
	frames := int64(1)
	switch toc & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if size < 2 {
			return 0, oggErr("frame count")
		}
		frames = int64(head[1] & 0x3f)
		if frames == 0 {
			return 0, oggErr("frame count")
		}
	}
	if frames*frame > maxPacketSamples {
		return 0, oggErr("packet longer than 120 ms")
	}
	return frames * frame, nil
}
