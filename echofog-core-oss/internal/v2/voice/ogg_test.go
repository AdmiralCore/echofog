package voice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

var secrets = []string{"libopus", "Pixel", "R5CT", "ENCODER", "EchoFogDroid", "DATE", "2026-11-06"}

// The clip that leaves the recorder carries the vendor of the encoder, comments with the device and
// the date, the original sample rate and a random stream serial. None of it reaches the
// receiver: Sanitize rewrites the headers to a canonical form.
func TestSanitizeStripsWhatIdentifiesTheRecorder(t *testing.T) {
	in := buildOgg(defaultOpts())
	for _, s := range secrets {
		if !bytes.Contains(in, []byte(s)) {
			t.Fatalf("the test clip lacks %q", s)
		}
	}
	out, ms, err := Sanitize(in)
	if err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if ms != 3000 {
		t.Fatalf("duration %d ms, want 3000", ms)
	}
	for _, s := range secrets {
		if bytes.Contains(out, []byte(s)) {
			t.Fatalf("the clean clip still holds %q", s)
		}
	}
	ps := splitPages(t, out)
	for i, p := range ps {
		if p.serial != 0 {
			t.Fatalf("page %d keeps the serial %#x", i, p.serial)
		}
		if p.seq != uint32(i) {
			t.Fatalf("page %d has sequence %d", i, p.seq)
		}
	}
	if !bytes.HasPrefix(ps[0].body, []byte("OpusHead")) || binary.LittleEndian.Uint32(ps[0].body[12:]) != 0 {
		t.Fatal("the sample rate of the recorder is still in the header")
	}
	if len(ps[1].body) != 16 || !bytes.HasPrefix(ps[1].body, []byte("OpusTags")) {
		t.Fatalf("tags packet is %d bytes, want the empty one of 16", len(ps[1].body))
	}
	if !bytes.Contains(out, audioPacket(0, 60)) || !bytes.Contains(out, audioPacket(149, 60)) {
		t.Fatal("the audio packets were changed")
	}
}

// Two recorders that made the same audio produce the same bytes after Sanitize, and cleaning a clean
// clip changes nothing.
func TestSanitizeIsCanonical(t *testing.T) {
	a := defaultOpts()
	b := defaultOpts()
	b.vendor, b.comments, b.rate, b.serial, b.tagsPad = "Lavf60", nil, 48000, 7, 300
	ca, _, err := Sanitize(buildOgg(a))
	if err != nil {
		t.Fatal(err)
	}
	cb, _, err := Sanitize(buildOgg(b))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca, cb) {
		t.Fatal("the same audio looks different after cleaning: the container leaks the recorder")
	}
	again, ms, err := Sanitize(ca)
	if err != nil || !bytes.Equal(again, ca) || ms != 3000 {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestSanitizeRejects(t *testing.T) {
	good := buildOgg(defaultOpts())
	flip := func(off int) []byte { b := append([]byte(nil), good...); b[off] ^= 1; return b }
	mut := func(f func(*fakeOpts)) []byte { o := defaultOpts(); f(&o); return buildOgg(o) }
	lastPage := bytes.LastIndex(good, []byte("OggS"))
	cases := map[string][]byte{
		"empty":            nil,
		"not ogg":          []byte("RIFF....WAVEfmt "),
		"bad crc":          flip(len(good) - 5),
		"bad magic":        flip(0),
		"truncated":        good[:len(good)-7],
		"trailing garbage": append(append([]byte(nil), good...), 1, 2, 3),
		"stereo":           mut(func(o *fakeOpts) { o.channels = 2 }),
		"zero channels":    mut(func(o *fakeOpts) { o.channels = 0 }),
		"mapping family 1": mut(func(o *fakeOpts) { o.family = 1 }),
		"head version 2":   mut(func(o *fakeOpts) { o.version = 0x20 }),
		"no end of stream": mut(func(o *fakeOpts) { o.noEOS = true }),
		"no tags":          mut(func(o *fakeOpts) { o.noTags = true }),
		"no packets":       mut(func(o *fakeOpts) { o.packets = 0 }),
		"second stream":    append(append([]byte(nil), good...), page(flagBOS, 0, 5, 0, opusHead(defaultOpts()))...),
		"sequence gap":     append(append([]byte(nil), good[:lastPage]...), page(flagEOS, 312+150*960, 0xDEADBEEF, 99, audioPacket(1, 60))...),
	}
	for name, in := range cases {
		if out, _, err := Sanitize(in); !errors.Is(err, ErrOgg) || out != nil {
			t.Errorf("%s: got %v, want ErrOgg", name, err)
		}
	}
	if _, _, err := Sanitize(make([]byte, MaxAudioBytes+8192)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized input: %v", err)
	}
}

// The granule position of a page cannot go backwards.
func TestSanitizeChecksGranules(t *testing.T) {
	o := defaultOpts()
	var b []byte
	b = append(b, page(flagBOS, 0, 1, 0, opusHead(o))...)
	b = append(b, page(0, 0, 1, 1, opusTags(o))...)
	b = append(b, page(0, 312+50*960, 1, 2, audioPacket(0, 60))...)
	b = append(b, page(flagEOS, 312+10*960, 1, 3, audioPacket(1, 60))...)
	if _, _, err := Sanitize(b); !errors.Is(err, ErrOgg) {
		t.Fatalf("a granule that goes backwards: %v", err)
	}
}

func FuzzSanitize(f *testing.F) {
	f.Add(buildOgg(defaultOpts()))
	o := defaultOpts()
	o.packets = 3
	f.Add(buildOgg(o))
	f.Add([]byte("OggS"))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, ms, err := Sanitize(in)
		if err != nil {
			if out != nil {
				t.Fatal("a clip came out with an error")
			}
			return
		}
		again, ms2, err := Sanitize(out)
		if err != nil || !bytes.Equal(again, out) || ms != ms2 {
			t.Fatalf("a clean clip is not stable: %v", err)
		}
	})
}
