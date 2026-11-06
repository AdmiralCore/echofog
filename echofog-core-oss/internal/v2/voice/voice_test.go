package voice

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"github.com/echofog/echofog-core/internal/v2/files"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var (
	alice = [32]byte{1: 1, 31: 0xA1}
	bob   = [32]byte{1: 2, 31: 0xB0}
	carol = [32]byte{1: 3, 31: 0xC0}
)

func levels(n int) []byte {
	l := make([]byte, n)
	for i := range l {
		l[i] = byte(i * 5)
	}
	return l
}

func rec(t testing.TB, seconds int) Recording {
	return Recording{Ogg: clip(t, seconds), Levels: levels(seconds * 50)}
}

func metaFrom(t testing.TB, m *chatpb.PersonalMessageProto) Meta {
	t.Helper()
	raw, err := base64.RawURLEncoding.Strict().DecodeString(m.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := ParseMeta(raw)
	if err != nil {
		t.Fatalf("ParseMeta: %v", err)
	}
	return meta
}

// A short clip rides inside the message; a longer one is a file of files/2. Either way the message
// fits the buckets of chat/2 (the largest is 57344 bytes).
func TestComposeInlineAndFile(t *testing.T) {
	short, err := Compose(rec(t, 10), alice, [][32]byte{bob})
	if err != nil {
		t.Fatal(err)
	}
	if short.Share != nil || short.Message.Type != chatpb.PersonalMessageType_MSG_TYPE_VOICE {
		t.Fatalf("10 s: share=%v type=%v, want an inline voice message", short.Share != nil, short.Message.Type)
	}
	if m := metaFrom(t, short.Message); m.Offer != nil || len(m.Inline) != len(short.Audio) || m.DurationMS != 10_000 {
		t.Fatalf("inline meta: %+v", m)
	}
	if _, err := chatpb.PadPersonal(short.Message); err != nil {
		t.Fatalf("an inline clip does not fit a chat/2 bucket: %v", err)
	}

	for _, secs := range []int{30, 300} {
		d, err := Compose(rec(t, secs), alice, [][32]byte{bob})
		if err != nil {
			t.Fatalf("%d s: %v", secs, err)
		}
		if d.Share == nil || d.DurationMS != uint32(secs*1000) {
			t.Fatalf("%d s: share=%v duration=%d", secs, d.Share != nil, d.DurationMS)
		}
		o := d.Share.Offer
		if o.Mime != MIME || o.Name != FileName || o.Size != uint64(len(d.Audio)) || o.Sender != alice {
			t.Fatalf("%d s: offer %+v", secs, o)
		}
		m := metaFrom(t, d.Message)
		if m.Offer == nil || *m.Offer != o || m.Inline != nil {
			t.Fatalf("%d s: the message does not carry the offer", secs)
		}
		if _, err := chatpb.PadPersonal(d.Message); err != nil {
			t.Fatalf("%d s: the offer message does not fit a bucket: %v", secs, err)
		}
		if d.Message.ContentHash != hex.EncodeToString(o.Root[:]) {
			t.Fatalf("%d s: content hash is not the root", secs)
		}
	}
}

// The inline limit is the biggest clip that still fits the message: one byte over goes as a file.
func TestInlineLimitFitsTheLargestBucket(t *testing.T) {
	o := defaultOpts()
	o.pktSize = 60
	// find the largest clip with at most MaxInlineAudio bytes
	for p := 100; ; p++ {
		o.packets = p
		clean, _, err := Sanitize(buildOgg(o))
		if err != nil {
			t.Fatal(err)
		}
		if len(clean) > MaxInlineAudio {
			o.packets = p - 1
			break
		}
	}
	d, err := Compose(Recording{Ogg: buildOgg(o), Levels: levels(o.packets)}, alice, [][32]byte{bob})
	if err != nil {
		t.Fatal(err)
	}
	if d.Share != nil {
		t.Fatalf("a clip of %d bytes (limit %d) went as a file", len(d.Audio), MaxInlineAudio)
	}
	if _, err := chatpb.PadPersonal(d.Message); err != nil {
		t.Fatalf("the largest inline message does not fit: %v", err)
	}
	o.packets++
	d, err = Compose(Recording{Ogg: buildOgg(o), Levels: levels(o.packets)}, alice, [][32]byte{bob})
	if err != nil || d.Share == nil {
		t.Fatalf("a clip over the inline limit must be a file (err %v)", err)
	}
}

func TestComposeRefuses(t *testing.T) {
	if _, err := Compose(Recording{}, alice, nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := Compose(Recording{Ogg: []byte("not a clip")}, alice, [][32]byte{bob}); !errors.Is(err, ErrOgg) {
		t.Fatalf("garbage: %v", err)
	}
	over := clip(t, 301)
	if _, err := Compose(Recording{Ogg: over, Levels: levels(301 * 50)}, alice, [][32]byte{bob}); !errors.Is(err, ErrTooLong) {
		t.Fatalf("301 s: %v", err)
	}
	if _, err := Compose(rec(t, 40), alice, nil); err == nil {
		t.Fatal("a file clip without recipients was accepted")
	}
	// a heavy clip (a stream far above 32 kbit/s) is not a voice message
	o := defaultOpts()
	o.packets, o.pktSize = 6000, 400
	if _, err := Compose(Recording{Ogg: buildOgg(o), Levels: levels(6000)}, alice, [][32]byte{bob}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a 160 kbit/s clip: %v", err)
	}
}

// Nothing that identifies the recorder, the device or the time is in what leaves the node.
func TestComposeLeaksNothing(t *testing.T) {
	for _, secs := range []int{5, 60} {
		d, err := Compose(rec(t, secs), alice, [][32]byte{bob})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if bytes.Contains(d.Audio, []byte(s)) {
				t.Fatalf("%d s: the clip holds %q", secs, s)
			}
		}
		m := d.Message
		if m.Text != "" || m.Timestamp != 0 || m.ReplyToId != "" || m.ConvEvent != nil || m.ConvSync != nil || m.Control != nil {
			t.Fatalf("%d s: unexpected fields in the message: %+v", secs, m)
		}
		if d.Share != nil && (d.Share.Offer.Name != FileName || d.Share.Offer.Mime != MIME) {
			t.Fatalf("%d s: name %q mime %q", secs, d.Share.Offer.Name, d.Share.Offer.Mime)
		}
	}
	// the same recording from two devices gives the same clip bytes
	other := defaultOpts()
	other.vendor, other.comments, other.serial, other.rate = "other", nil, 99, 16000
	a, _ := Compose(Recording{Ogg: buildOgg(defaultOpts()), Levels: levels(150)}, alice, [][32]byte{bob})
	b, _ := Compose(Recording{Ogg: buildOgg(other), Levels: levels(150)}, alice, [][32]byte{bob})
	if !bytes.Equal(a.Audio, b.Audio) {
		t.Fatal("the audio bytes differ between recorders")
	}
}

func TestWaveform(t *testing.T) {
	rising := make([]byte, 500)
	for i := range rising {
		rising[i] = byte(i * 255 / 500)
	}
	w := Waveform(rising, WavePoints)
	if len(w) != WavePoints {
		t.Fatalf("%d points", len(w))
	}
	for i := 1; i < len(w); i++ {
		if w[i] < w[i-1] {
			t.Fatal("peaks of a rising signal must not fall")
		}
	}
	if got := Waveform([]byte{10, 200, 30}, WavePoints); len(got) != WavePoints {
		t.Fatalf("a short clip gives %d points", len(got))
	}
	if got := Waveform(nil, WavePoints); len(got) != WavePoints {
		t.Fatalf("no levels give %d points", len(got))
	}
	if Waveform(levels(500), 0) != nil {
		t.Fatal("zero points")
	}
}

// ---- the metadata body -------------------------------------------------------------------

func wave(n int) []byte { return bytes.Repeat([]byte{7}, n) }

func goodOffer() files.Offer {
	return files.Offer{FID: [16]byte{1}, Size: 20_000, Root: [32]byte{2}, ChunkSize: files.ChunkSize, Name: FileName, Mime: MIME, Sender: alice}
}

func rawMeta(fields ...[]byte) []byte {
	var b []byte
	for _, f := range fields {
		b = append(b, f...)
	}
	return b
}

func vf(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}
func bf(num protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), v)
}

func TestMetaRoundTrip(t *testing.T) {
	o := goodOffer()
	for _, m := range []Meta{
		{Version: MetaVersion, DurationMS: 12_345, Wave: wave(48), Offer: &o},
		{Version: MetaVersion, DurationMS: 1, Wave: wave(MinWavePoints), Inline: []byte{1, 2, 3}},
		{Version: MetaVersion, DurationMS: MaxDurationMS, Wave: wave(MaxWavePoints), Offer: &o},
	} {
		got, err := ParseMeta(m.Marshal())
		if err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
		if got.DurationMS != m.DurationMS || !bytes.Equal(got.Wave, m.Wave) || !bytes.Equal(got.Inline, m.Inline) || (m.Offer != nil) != (got.Offer != nil) {
			t.Fatalf("round trip: %+v -> %+v", m, got)
		}
		if !bytes.Equal(got.Marshal(), m.Marshal()) {
			t.Fatal("not canonical")
		}
	}
}

func TestParseMetaRejects(t *testing.T) {
	o := goodOffer().Marshal()
	big := goodOffer()
	big.Size = uint64(files.MaxOfferSize)
	heavy := goodOffer()
	heavy.Size = MaxAudioBytes
	foreign := goodOffer()
	foreign.Mime = "audio/mpeg"
	named := goodOffer()
	named.Name = "Meeting with Anna 2026-11-06 Pixel8.ogg"
	cases := map[string][]byte{
		"empty":                      nil,
		"no version":                 rawMeta(vf(2, 1000), bf(3, wave(48)), bf(5, o)),
		"zero duration":              rawMeta(vf(1, 1), bf(3, wave(48)), bf(5, o)),
		"duration over five min":     rawMeta(vf(1, 1), vf(2, MaxDurationMS+1), bf(3, wave(48)), bf(5, o)),
		"wave 31":                    rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(31)), bf(5, o)),
		"wave 65":                    rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(65)), bf(5, o)),
		"no wave":                    rawMeta(vf(1, 1), vf(2, 1000), bf(5, o)),
		"neither audio nor offer":    rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48))),
		"both audio and offer":       rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(4, []byte{1}), bf(5, o)),
		"inline over the limit":      rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(4, make([]byte, MaxInlineAudio+1))),
		"repeated duration":          rawMeta(vf(1, 1), vf(2, 1000), vf(2, 1001), bf(3, wave(48)), bf(5, o)),
		"duration of the wrong wire": rawMeta(vf(1, 1), bf(2, []byte{1}), bf(3, wave(48)), bf(5, o)),
		"offer does not parse":       rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(5, []byte{0xff})),
		"offer is not audio/ogg":     rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(5, foreign.Marshal())),
		"offer name is not fixed":    rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(5, named.Marshal())),
		"offer is a huge file":       rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(5, big.Marshal())),
		"size far above the length":  rawMeta(vf(1, 1), vf(2, 5000), bf(3, wave(48)), bf(5, heavy.Marshal())),
	}
	for name, raw := range cases {
		if _, err := ParseMeta(raw); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error %v is not ErrInvalid", name, err)
		}
	}
}

// A newer version can add fields and even change the layout: the old reader skips unknown fields and
// reports a newer version as such (the interface shows the placeholder), never as damage.
func TestParseMetaIsForwardCompatible(t *testing.T) {
	o := goodOffer().Marshal()
	withExtra := rawMeta(vf(1, 1), vf(2, 1000), bf(3, wave(48)), bf(5, o), vf(9, 5), bf(12, []byte("later")))
	if m, err := ParseMeta(withExtra); err != nil || m.DurationMS != 1000 {
		t.Fatalf("unknown fields: %v", err)
	}
	newer := rawMeta(vf(1, 2), bf(7, []byte("new layout")))
	if _, err := ParseMeta(newer); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("version 2: %v", err)
	}
}

func FuzzParseMeta(f *testing.F) {
	o := goodOffer()
	f.Add(Meta{Version: MetaVersion, DurationMS: 1000, Wave: wave(48), Offer: &o}.Marshal())
	f.Add(Meta{Version: MetaVersion, DurationMS: 1000, Wave: wave(48), Inline: []byte{1}}.Marshal())
	f.Add(rawMeta(vf(1, 2), bf(7, []byte("x"))))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseMeta(b)
		if err != nil {
			return
		}
		back, err := ParseMeta(m.Marshal())
		if err != nil || !bytes.Equal(back.Marshal(), m.Marshal()) {
			t.Fatalf("a parsed meta does not survive Marshal: %v", err)
		}
	})
}

// ---- how the interface sees a message ------------------------------------------------------

func TestClassify(t *testing.T) {
	d, _ := Compose(rec(t, 5), alice, [][32]byte{bob})
	if k, m, err := Classify(d.Message); err != nil || k != KindVoice || m.DurationMS != 5000 {
		t.Fatalf("voice: %v %v %v", k, m.DurationMS, err)
	}
	text := &chatpb.PersonalMessageProto{Id: "x", Type: chatpb.PersonalMessageType_MSG_TYPE_TEXT, Text: "hi"}
	if k, _, err := Classify(text); err != nil || k != KindText {
		t.Fatalf("text: %v %v", k, err)
	}
	file := &chatpb.PersonalMessageProto{Id: "x", Type: chatpb.PersonalMessageType_MSG_TYPE_FILE}
	if k, _, err := Classify(file); err != nil || k != KindFile {
		t.Fatalf("file: %v %v", k, err)
	}

	// an attachment type this version has never heard of, and a voice message of a newer version
	unknown := &chatpb.PersonalMessageProto{Id: "x", Type: chatpb.PersonalMessageType(99), Metadata: "AAAA"}
	if k, _, err := Classify(unknown); err != nil || k != KindPlaceholder {
		t.Fatalf("unknown type: %v %v", k, err)
	}
	newer := &chatpb.PersonalMessageProto{Id: "x", SenderId: hex.EncodeToString(alice[:]), Type: chatpb.PersonalMessageType_MSG_TYPE_VOICE,
		Metadata: base64.RawURLEncoding.EncodeToString(rawMeta(vf(1, 2), bf(7, []byte("new"))))}
	if k, _, err := Classify(newer); err != nil || k != KindPlaceholder {
		t.Fatalf("newer voice: %v %v", k, err)
	}
	// damage is an error, not a placeholder
	bad := &chatpb.PersonalMessageProto{Id: "x", Type: chatpb.PersonalMessageType_MSG_TYPE_VOICE, Metadata: "!!!"}
	if _, _, err := Classify(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("damaged: %v", err)
	}
	if k, _, err := Classify(nil); err == nil || k == KindVoice {
		t.Fatalf("nil: %v %v", k, err)
	}
}

func TestPlaceholderTexts(t *testing.T) {
	ru, en := PlaceholderText("ru"), PlaceholderText("en")
	if ru != "Сообщение из новой версии приложения — обновите приложение" {
		t.Fatalf("ru: %q", ru)
	}
	if en == "" || en == ru || PlaceholderText("de") != en {
		t.Fatalf("en: %q", en)
	}
	for _, s := range []string{ru, en, voiceLabel("ru"), voiceLabel("en")} {
		if s == "" || strings.ContainsAny(s, "!") {
			t.Fatalf("text %q", s)
		}
	}
}

func TestSpeeds(t *testing.T) {
	want := []float64{1, 1.5, 2}
	s := Speed1x
	for i, w := range want {
		if s.Factor() != w {
			t.Fatalf("speed %d: %v, want %v", i, s.Factor(), w)
		}
		s = s.Next()
	}
	if s != Speed1x {
		t.Fatal("the speeds do not cycle 1x, 1.5x, 2x")
	}
	if Speed(9).Factor() != 1 || Speed(9).Next() != Speed1x {
		t.Fatal("an unknown speed must play at 1x")
	}
}

// ---- the receiver's list ------------------------------------------------------------------

func TestInboxDedupByID(t *testing.T) {
	d, _ := Compose(rec(t, 5), alice, [][32]byte{bob})
	in := NewInbox(16)
	e1, fresh, err := in.Add(d.Message)
	if err != nil || !fresh {
		t.Fatalf("first: fresh=%v err=%v", fresh, err)
	}
	if a, ok := e1.Audio(); !ok || !bytes.Equal(a, d.Audio) {
		t.Fatal("an inline clip must be playable at once")
	}
	e2, fresh, err := in.Add(d.Message)
	if err != nil || fresh || e2 != e1 {
		t.Fatalf("a repeated message: fresh=%v same=%v err=%v", fresh, e2 == e1, err)
	}
	if in.Len() != 1 {
		t.Fatalf("%d entries", in.Len())
	}
	// the same ID from another sender is another message: an ID of someone else cannot be taken over
	m2 := proto.Clone(d.Message).(*chatpb.PersonalMessageProto)
	m2.SenderId = hex.EncodeToString(carol[:])
	if e3, fresh, err := in.Add(m2); err != nil || !fresh || e3 == e1 {
		t.Fatalf("same ID, another sender: fresh=%v err=%v", fresh, err)
	}
	// another message from the same sender with a repeated body is still a new ID
	d2, _ := Compose(rec(t, 5), alice, [][32]byte{bob})
	if _, fresh, _ := in.Add(d2.Message); !fresh || in.Len() != 3 {
		t.Fatalf("a new message was taken as a repeat (len %d)", in.Len())
	}
}

func TestInboxRefuses(t *testing.T) {
	d, _ := Compose(rec(t, 40), alice, [][32]byte{bob})
	in := NewInbox(4)
	cases := map[string]func(m *chatpb.PersonalMessageProto){
		"no id":                func(m *chatpb.PersonalMessageProto) { m.Id = "" },
		"long id":              func(m *chatpb.PersonalMessageProto) { m.Id = strings.Repeat("a", 65) },
		"no sender":            func(m *chatpb.PersonalMessageProto) { m.SenderId = "" },
		"sender is not author": func(m *chatpb.PersonalMessageProto) { m.SenderId = hex.EncodeToString(carol[:]) }, // the offer names alice
		"content hash differs": func(m *chatpb.PersonalMessageProto) { m.ContentHash = strings.Repeat("0", 64) },
		"not a voice message":  func(m *chatpb.PersonalMessageProto) { m.Type = chatpb.PersonalMessageType_MSG_TYPE_FILE },
		"text is set":          func(m *chatpb.PersonalMessageProto) { m.Text = "Anna's phone" },
	}
	for name, f := range cases {
		m := proto.Clone(d.Message).(*chatpb.PersonalMessageProto)
		f(m)
		if _, _, err := in.Add(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if in.Len() != 0 {
		t.Fatalf("refused messages were kept (%d)", in.Len())
	}
}

func TestInboxIsBounded(t *testing.T) {
	in := NewInbox(8)
	for i := 0; i < 100; i++ {
		d, err := Compose(rec(t, 3), alice, [][32]byte{bob})
		if err != nil {
			t.Fatal(err)
		}
		d.Message.Id = fmt.Sprintf("%032x", i+1)
		if _, _, err := in.Add(d.Message); err != nil {
			t.Fatal(err)
		}
	}
	if in.Len() != 8 {
		t.Fatalf("the list holds %d entries, want its limit 8", in.Len())
	}
}

func TestInboxConcurrent(t *testing.T) {
	d, _ := Compose(rec(t, 3), alice, [][32]byte{bob})
	in := NewInbox(16)
	var wg sync.WaitGroup
	var mu sync.Mutex
	fresh := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, f, err := in.Add(d.Message); err == nil && f {
				mu.Lock()
				fresh++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if fresh != 1 || in.Len() != 1 {
		t.Fatalf("one message added by 32 goroutines: fresh=%d len=%d", fresh, in.Len())
	}
}

// What the file holds must be what the message said: its length is checked against the
// duration, and a file that does not parse is refused.
func TestFinishChecksTheFile(t *testing.T) {
	d, _ := Compose(rec(t, 40), alice, [][32]byte{bob})
	in := NewInbox(4)
	e, _, err := in.Add(d.Message)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Audio(); ok {
		t.Fatal("a file clip is playable before it is fetched")
	}
	if o, ok := e.Offer(); !ok || o != d.Share.Offer {
		t.Fatalf("offer: %v", ok)
	}
	if err := e.Finish([]byte("garbage")); !errors.Is(err, ErrOgg) {
		t.Fatalf("garbage: %v", err)
	}
	if err := e.Finish(clip(t, 90)); !errors.Is(err, ErrDuration) { // the message says 40 s
		t.Fatalf("a longer clip: %v", err)
	}
	if _, ok := e.Audio(); ok {
		t.Fatal("a refused clip became playable")
	}
	if err := e.Finish(d.Audio); err != nil {
		t.Fatalf("the right clip: %v", err)
	}
	if a, ok := e.Audio(); !ok || !bytes.Equal(a, d.Audio) {
		t.Fatal("the clip was changed")
	}
	// the receiver never plays the sender's headers: a clip with tags is cleaned
	e2, _, _ := in.Add(func() *chatpb.PersonalMessageProto {
		m := proto.Clone(d.Message).(*chatpb.PersonalMessageProto)
		m.Id = strings.Repeat("b", 32)
		return m
	}())
	if err := e2.Finish(clip(t, 40)); err != nil {
		t.Fatal(err)
	}
	if a, _ := e2.Audio(); bytes.Contains(a, []byte("Pixel")) {
		t.Fatal("tags of the sender reached the player")
	}
}
