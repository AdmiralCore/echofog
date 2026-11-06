package files

import (
	"errors"
	"strings"
	"testing"

	chatpb "github.com/echofog/echofog-core/internal/v2/chat/pb"
)

func goodOffer() Offer {
	var root, sender [32]byte
	root[0], sender[0] = 9, 8
	return Offer{FID: fid(1), Size: 3*ChunkSize + 17, Root: root, ChunkSize: ChunkSize, Name: "отчёт 2026.pdf", Mime: "application/pdf", Sender: sender}
}

func TestOfferRoundTrip(t *testing.T) {
	o := goodOffer()
	got, err := ParseOffer(o.Marshal())
	if err != nil || got != o {
		t.Fatalf("got %+v %v", got, err)
	}
	if o.Chunks() != 4 {
		t.Fatalf("Chunks = %d", o.Chunks())
	}
	if (Offer{Size: 0}).Chunks() != 1 {
		t.Fatal("an empty file has one (empty) chunk")
	}
}

func TestOfferRules(t *testing.T) {
	long := strings.Repeat("я", 200) // 400 bytes
	mut := func(f func(o *Offer)) Offer { o := goodOffer(); f(&o); return o }
	bad := map[string]Offer{
		"chunk size":        mut(func(o *Offer) { o.ChunkSize = 32768 }),
		"no chunk size":     mut(func(o *Offer) { o.ChunkSize = 0 }),
		"size over limit":   mut(func(o *Offer) { o.Size = uint64(MaxOfferSize) + 1 }),
		"no name":           mut(func(o *Offer) { o.Name = "" }),
		"name too long":     mut(func(o *Offer) { o.Name = long }),
		"name with NUL":     mut(func(o *Offer) { o.Name = "a\x00b" }),
		"name not utf-8":    mut(func(o *Offer) { o.Name = "a\xffb" }),
		"mime too long":     mut(func(o *Offer) { o.Mime = strings.Repeat("a", 129) }),
		"zero fid":          mut(func(o *Offer) { o.FID = [16]byte{} }),
		"zero sender":       mut(func(o *Offer) { o.Sender = [32]byte{} }),
		"zero root":         mut(func(o *Offer) { o.Root = [32]byte{} }),
		"path in the name":  mut(func(o *Offer) { o.Name = "../../x" }),
		"drive in the name": mut(func(o *Offer) { o.Name = `C:\x` }),
	}
	for name, o := range bad {
		if err := o.Validate(); !errors.Is(err, ErrOffer) {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if _, err := ParseOffer(o.Marshal()); !errors.Is(err, ErrOffer) {
			t.Errorf("%s: ParseOffer = %v", name, err)
		}
	}
	// the largest allowed size is fine
	o := goodOffer()
	o.Size = uint64(MaxOfferSize)
	if err := o.Validate(); err != nil {
		t.Errorf("max size: %v", err)
	}
}

func TestOfferTravelsInAChatMessage(t *testing.T) {
	o := goodOffer()
	m := o.Message()
	if m == nil || m.Type != chatpb.PersonalMessageType_MSG_TYPE_FILE {
		t.Fatalf("message: %+v", m)
	}
	got, err := OfferFrom(m)
	if err != nil || got != o {
		t.Fatalf("got %+v %v", got, err)
	}
	// the offer is not trusted to be in any other kind of message, or to be intact
	txt := &chatpb.PersonalMessageProto{Type: chatpb.PersonalMessageType_MSG_TYPE_TEXT, Metadata: m.Metadata}
	if _, err := OfferFrom(txt); !errors.Is(err, ErrOffer) {
		t.Errorf("text message: %v", err)
	}
	for _, md := range []string{"", "!!!", m.Metadata + "AAAA", m.Metadata[:len(m.Metadata)/2]} {
		cp := &chatpb.PersonalMessageProto{Type: chatpb.PersonalMessageType_MSG_TYPE_FILE, Metadata: md}
		if _, err := OfferFrom(cp); !errors.Is(err, ErrOffer) {
			t.Errorf("metadata %q: %v", md, err)
		}
	}
	if _, err := OfferFrom(nil); !errors.Is(err, ErrOffer) {
		t.Errorf("nil message: %v", err)
	}
	// the message must fit the small-message buckets: an offer is not a reason for a big message
	if len(m.Metadata) > 1024 {
		t.Errorf("offer metadata is %d bytes", len(m.Metadata))
	}
}

func FuzzParseOffer(f *testing.F) {
	f.Add(goodOffer().Marshal())
	f.Add([]byte{})
	f.Add([]byte{0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		o, err := ParseOffer(b)
		if err != nil {
			return
		}
		if o.Validate() != nil || o.ChunkSize != ChunkSize || int64(o.Size) > MaxOfferSize {
			t.Fatalf("accepted a bad offer: %+v", o)
		}
		if again, err := ParseOffer(o.Marshal()); err != nil || again != o {
			t.Fatalf("not stable: %+v %v", again, err)
		}
	})
}
