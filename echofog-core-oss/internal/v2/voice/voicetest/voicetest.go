// Package voicetest builds Ogg Opus clips for tests of code that handles voice messages. The
// packets are arbitrary bytes under valid TOC bytes: the container is real, no audio is encoded.
package voicetest

import "encoding/binary"

var tab = func() (t [256]uint32) {
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

func crc(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = c<<8 ^ tab[byte(c>>24)^x]
	}
	return c
}

func page(typ byte, granule int64, seq uint32, packets ...[]byte) []byte {
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
	h := make([]byte, 27)
	copy(h, "OggS")
	h[5] = typ
	binary.LittleEndian.PutUint64(h[6:], uint64(granule))
	binary.LittleEndian.PutUint32(h[14:], 0x5EED)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = byte(len(segs))
	out := append(append(h, segs...), body...)
	binary.LittleEndian.PutUint32(out[22:], crc(out))
	return out
}

// Clip returns a mono Ogg Opus stream of the given length at about 24 kbit/s (60-byte packets of
// 20 ms), with a vendor string and comments that a sanitizer is expected to remove.
func Clip(seconds int) []byte {
	const preskip = 312
	head := []byte("OpusHead")
	head = append(head, 1, 1)
	head = binary.LittleEndian.AppendUint16(head, preskip)
	head = binary.LittleEndian.AppendUint32(head, 44100)
	head = binary.LittleEndian.AppendUint16(head, 0)
	head = append(head, 0)
	tags := []byte("OpusTags")
	vendor := "libopus 1.4.0 (Pixel 8)"
	tags = binary.LittleEndian.AppendUint32(tags, uint32(len(vendor)))
	tags = append(tags, vendor...)
	tags = binary.LittleEndian.AppendUint32(tags, 1)
	c := "ENCODER=EchoFogDroid"
	tags = binary.LittleEndian.AppendUint32(tags, uint32(len(c)))
	tags = append(tags, c...)

	out := page(0x02, 0, 0, head)
	out = append(out, page(0, 0, 1, tags)...)
	n := seconds * 50
	seq := uint32(2)
	for i := 0; i < n; i += 50 {
		end := min(i+50, n)
		var ps [][]byte
		for k := i; k < end; k++ {
			p := make([]byte, 60)
			for j := range p {
				p[j] = byte(k*31 + j*7 + 1)
			}
			p[0] = 0xF8
			ps = append(ps, p)
		}
		typ := byte(0)
		if end == n {
			typ = 0x04
		}
		out = append(out, page(typ, int64(preskip)+int64(end)*960, seq, ps...)...)
		seq++
	}
	return out
}
