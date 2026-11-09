package voice

import (
	"errors"
	"fmt"
	"testing"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
	"google.golang.org/protobuf/proto"
)

// Findings of the acceptance review of T7.6.

// The chat layer names the sender it verified; the SenderId inside the message is the sender's own
// claim. A message whose claim differs from the verified sender is refused, and the refusal looks
// like any other damaged message (AD20): nothing tells the attacker which check failed.
func TestInboxChecksTheVerifiedSender(t *testing.T) {
	for _, secs := range []int{5, 40} { // an inline clip and a file clip
		d, err := Compose(rec(t, secs), alice, [][32]byte{bob})
		if err != nil {
			t.Fatal(err)
		}
		in := NewInbox(16)

		// carol sends alice's message under alice's name
		e, fresh, err := in.Add(carol, d.Message)
		if !errors.Is(err, ErrInvalid) || e != nil || fresh || in.Len() != 0 {
			t.Fatalf("%d s: a message of another sender: entry=%v fresh=%v err=%v len=%d", secs, e != nil, fresh, err, in.Len())
		}
		// the refusal is the one a malformed sender gets
		bad := proto.Clone(d.Message).(*chatpb.PersonalMessageProto)
		bad.SenderId = "zz"
		_, _, errBad := in.Add(carol, bad)
		if errBad == nil || errBad.Error() != err.Error() {
			t.Fatalf("%d s: the refusals differ: %q and %q", secs, err, errBad)
		}
		// nothing was taken: alice's own message is new
		if _, fresh, err := in.Add(alice, d.Message); err != nil || !fresh {
			t.Fatalf("%d s: the real message after a forgery: fresh=%v err=%v", secs, fresh, err)
		}
		// a later forgery of the same message does not return alice's entry
		if e, _, err := in.Add(carol, d.Message); err == nil || e != nil {
			t.Fatalf("%d s: a forgery got the entry of the real message", secs)
		}
		// a forgery that claims the sender it arrived from is that sender's message, nothing more
		own := proto.Clone(d.Message).(*chatpb.PersonalMessageProto)
		own.SenderId = fmt.Sprintf("%x", carol)
		if secs == 5 {
			if _, fresh, err := in.Add(carol, own); err != nil || !fresh {
				t.Fatalf("carol's own message: fresh=%v err=%v", fresh, err)
			}
		}
	}
}

// ogg makes a stream of one audio page.
func ogg(granule int64, packets ...[]byte) []byte {
	o := defaultOpts()
	var b []byte
	b = append(b, page(flagBOS, 0, 1, 0, opusHead(o))...)
	b = append(b, page(0, 0, 1, 1, opusTags(o))...)
	return append(b, page(flagEOS, granule, 1, 2, packets...)...)
}

func packets(n int, toc byte, more ...byte) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		p := append([]byte{toc}, more...)
		out[i] = append(p, make([]byte, 20)...)
	}
	return out
}

// The length comes from the granule position, but the packets must hold that much audio: three
// packets cannot be a five minute clip.
func TestSanitizeCountsThePackets(t *testing.T) {
	const pre = 312
	reject := map[string][]byte{
		"3 packets claim five minutes":    ogg(pre+300*48000, packets(3, 0xF8)...),
		"150 packets claim a minute more": ogg(pre+150*960+60*48000, packets(150, 0xF8)...),
		"an empty packet":                 ogg(pre+2*960, append(packets(1, 0xF8), []byte{})...),
		"code 3 without a count":          ogg(pre+960, []byte{0xFB}),
		"code 3 with no frames":           ogg(pre+960, []byte{0xFB, 0x00}),
		"code 3 over 120 ms":              ogg(pre+7*960, append([]byte{0xFB, 0x07}, make([]byte, 20)...)),
	}
	for name, in := range reject {
		if out, _, err := Sanitize(in); !errors.Is(err, ErrOgg) || out != nil {
			t.Errorf("%s: got %v, want ErrOgg", name, err)
		}
	}
	accept := []struct {
		name string
		in   []byte
		ms   uint32
	}{
		{"one frame per packet", ogg(pre+150*960, packets(150, 0xF8)...), 3000},
		{"two frames per packet", ogg(pre+50*2*960, packets(50, 0xF9)...), 2000},
		{"code 3, three frames", ogg(pre+20*3*960, packets(20, 0xFB, 0x03)...), 1200},
		{"60 ms SILK frames", ogg(pre+10*2880, packets(10, 0x18)...), 600},
		{"the end trimmed", ogg(pre+150*960-500, packets(150, 0xF8)...), (150*960 - 500) / 48},
	}
	for _, c := range accept {
		out, ms, err := Sanitize(c.in)
		if err != nil || out == nil || ms != c.ms {
			t.Errorf("%s: ms=%d err=%v, want %d", c.name, ms, err, c.ms)
		}
	}
}

func senderN(n int) [32]byte { return [32]byte{0: byte(n), 31: 0x5E} }

func voiceFrom(t *testing.T, from [32]byte, id int) *chatpb.PersonalMessageProto {
	t.Helper()
	d, err := Compose(rec(t, 3), from, [][32]byte{bob})
	if err != nil {
		t.Fatal(err)
	}
	d.Message.Id = fmt.Sprintf("%032x", id)
	return d.Message
}

// One sender cannot push the others out: the limit holds per sender, and a full list drops the
// oldest message of the sender who holds most.
func TestInboxLimitPerSender(t *testing.T) {
	in := NewInbox(8) // two per sender
	mine := []*chatpb.PersonalMessageProto{voiceFrom(t, bob, 1000), voiceFrom(t, bob, 1001)}
	for _, m := range mine {
		if _, fresh, err := in.Add(bob, m); err != nil || !fresh {
			t.Fatalf("bob: fresh=%v err=%v", fresh, err)
		}
	}
	for i := 0; i < 50; i++ {
		if _, _, err := in.Add(alice, voiceFrom(t, alice, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if in.Len() != 4 {
		t.Fatalf("%d entries; bob's two and alice's two were expected", in.Len())
	}
	for _, m := range mine {
		if _, fresh, _ := in.Add(bob, m); fresh {
			t.Fatal("a flood of one sender pushed out the messages of another")
		}
	}

	// a full list: a newcomer takes the place of the sender who holds most
	full := NewInbox(8)
	for s := 1; s <= 3; s++ {
		for k := 0; k < 2; k++ {
			if _, _, err := full.Add(senderN(s), voiceFrom(t, senderN(s), s*10+k)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := full.Add(senderN(4), voiceFrom(t, senderN(4), 40)); err != nil { // 7 entries
		t.Fatal(err)
	}
	if _, _, err := full.Add(senderN(5), voiceFrom(t, senderN(5), 50)); err != nil { // 8
		t.Fatal(err)
	}
	if _, fresh, err := full.Add(senderN(6), voiceFrom(t, senderN(6), 60)); err != nil || !fresh || full.Len() != 8 {
		t.Fatalf("a newcomer to a full list: fresh=%v err=%v len=%d", fresh, err, full.Len())
	}
	for _, s := range []int{4, 5, 6} { // the senders with one message each keep it
		if _, fresh, _ := full.Add(senderN(s), voiceFrom(t, senderN(s), s*10)); fresh {
			t.Fatalf("sender %d lost its only message to a newcomer", s)
		}
	}
}
