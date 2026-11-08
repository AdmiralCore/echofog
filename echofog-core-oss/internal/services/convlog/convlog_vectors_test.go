package convlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// vectorFile is the shape of testdata/convlog.json (docs/protocol/v2/vectors/convlog.json).
type vectorFile struct {
	AuthorPublicKey string `json:"author_public_key"`
	Conv            string `json:"conv"`
	Events          []struct {
		Name        string `json:"name"`
		Event       string `json:"event"`
		SignedBytes string `json:"signed_bytes"`
		Body        string `json:"body"`
		BodySHA256  string `json:"body_sha256"`
		Ext         string `json:"ext"`
		Sig         string `json:"sig"`
		ID          string `json:"id"`
		LastSeq     []struct {
			Author string `json:"author"`
			Seq    uint64 `json:"seq"`
		} `json:"last_seq"`
	} `json:"events"`
	Negative []struct {
		Name        string `json:"name"`
		Event       string `json:"event"`
		SignedBytes string `json:"signed_bytes"`
		Reason      string `json:"reason"`
	} `json:"negative"`
}

// The vector file is generated from the spec (docs/protocol/v2/vectors/gen/convlog.go). The copy in
// testdata must match it when the repository copy is present.
func TestConvlogVectorsCopyMatchesSpec(t *testing.T) {
	copyBytes, err := os.ReadFile("testdata/convlog.json")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile("../../../../docs/protocol/v2/vectors/convlog.json")
	if os.IsNotExist(err) {
		t.Skip("spec vectors not in this checkout")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(copyBytes, []byte(""), nil), bytes.ReplaceAll(spec, []byte(""), nil)) {
		t.Fatal("testdata/convlog.json differs from docs/protocol/v2/vectors/convlog.json")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/convlog.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestConvlogVectorsMatchCode checks the positive vectors against the implementation: the signed
// bytes, the event id, the signature and the body hash, through Decode, which verifies the signature.
func TestConvlogVectorsMatchCode(t *testing.T) {
	v := loadVectors(t)
	if len(v.Events) == 0 {
		t.Fatal("no events in the vectors")
	}
	for _, ev := range v.Events {
		t.Run(ev.Name, func(t *testing.T) {
			e, id, err := Decode(mustHex(t, ev.Event))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := hex.EncodeToString(id[:]); got != ev.ID {
				t.Errorf("id = %s, vector %s", got, ev.ID)
			}
			if got := hex.EncodeToString(signedBytes(e)); got != ev.SignedBytes {
				t.Errorf("signed bytes differ:\n code   %s\n vector %s", got, ev.SignedBytes)
			}
			if !bytes.Equal(e.Body, mustHex(t, ev.Body)) {
				t.Error("body differs")
			}
			if h := sha256.Sum256(e.Body); hex.EncodeToString(h[:]) != ev.BodySHA256 {
				t.Error("body hash differs")
			}
			if !bytes.Equal(e.Ext, mustHex(t, ev.Ext)) {
				t.Error("ext differs")
			}
			if !bytes.Equal(e.Sig, mustHex(t, ev.Sig)) {
				t.Error("signature differs")
			}
			if hex.EncodeToString(e.Author) != v.AuthorPublicKey {
				t.Error("author differs")
			}
			if hex.EncodeToString(e.Conv) != v.Conv {
				t.Error("conversation id differs")
			}
		})
	}
}

// TestConvlogAttachmentVectorIsKnown: this build reads the attachment (field 6) of msg-att, and it is
// valid (fid, size, root, name, mime as the vector says), so the file card is shown.
func TestConvlogAttachmentVectorIsKnown(t *testing.T) {
	v := loadVectors(t)
	for _, ev := range v.Events {
		if ev.Name != "msg-att" {
			continue
		}
		e, _, err := Decode(mustHex(t, ev.Event))
		if err != nil {
			t.Fatal(err)
		}
		var body MsgBody
		if err := proto.Unmarshal(e.Body, &body); err != nil {
			t.Fatalf("body: %v", err)
		}
		if body.GetText() != "вот схема" {
			t.Errorf("text = %q", body.GetText())
		}
		if len(body.ProtoReflect().GetUnknown()) != 0 {
			t.Error("the attachment is still an unknown field")
		}
		at := body.GetAtt()
		if at == nil || len(at.Fid) != 16 || at.Size != 4096 || len(at.MerkleRoot) != 32 || at.Name == "" || at.Mime == "" {
			t.Fatalf("attachment %+v", at)
		}
		if err := checkAttachment(at); err != nil {
			t.Errorf("the vector's attachment is refused: %v", err)
		}
		return
	}
	t.Fatal("msg-att missing")
}

// TestConvlogGroupCloseKindIsKnown: kind 10 is a kind of this build (the old-reader behaviour of
// versioning §3.9 is for builds that do not have it).
func TestConvlogGroupCloseKindIsKnown(t *testing.T) {
	v := loadVectors(t)
	for _, ev := range v.Events {
		if ev.Name != "group-close" {
			continue
		}
		e, _, err := Decode(mustHex(t, ev.Event))
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind != 10 || KindClose != 10 {
			t.Errorf("kind = %d, want 10", e.Kind)
		}
		if !KnownKind(e.Kind) {
			t.Error("kind 10 is not known to this build")
		}
		return
	}
	t.Fatal("group-close missing")
}

// TestConvlogNegativeAttachmentNotCovered: an attachment swapped under the original signature is
// rejected, and the vector's signed bytes match the code's.
func TestConvlogNegativeAttachmentNotCovered(t *testing.T) {
	v := loadVectors(t)
	if len(v.Negative) == 0 {
		t.Fatal("no negative vectors")
	}
	for _, n := range v.Negative {
		t.Run(n.Name, func(t *testing.T) {
			raw := mustHex(t, n.Event)
			var e Event
			if err := proto.Unmarshal(raw, &e); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(signedBytes(&e)); got != n.SignedBytes {
				t.Errorf("signed bytes differ: code %s, vector %s", got, n.SignedBytes)
			}
			_, _, err := Decode(raw)
			if err == nil {
				t.Fatal("Decode accepted an event whose attachment is not covered by its signature")
			}
			if err.Error() != n.Reason {
				t.Errorf("error = %q, vector reason %q", err.Error(), n.Reason)
			}
		})
	}
}

// TestConvlogGroupCloseVector checks the CloseBody of group-close-vector: entries sorted by author,
// one per author, the seq values the vector names, and that the close event decodes and is kind 10.
// Acceptance of later events against the vector is tested in attach_test.go.
func TestConvlogGroupCloseVector(t *testing.T) {
	v := loadVectors(t)
	for _, ev := range v.Events {
		if ev.Name != "group-close-vector" {
			continue
		}
		e, _, err := Decode(mustHex(t, ev.Event))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if e.Kind != 10 {
			t.Fatalf("kind = %d, want 10", e.Kind)
		}
		var got []struct {
			author string
			seq    uint64
		}
		b := e.Body
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 || num != 1 || typ != protowire.BytesType {
				t.Fatalf("CloseBody: bad field at %d", len(got))
			}
			b = b[n:]
			entry, m := protowire.ConsumeBytes(b)
			if m < 0 {
				t.Fatal("CloseBody: bad entry")
			}
			b = b[m:]
			var author []byte
			var seq uint64
			for len(entry) > 0 {
				fn, ft, k := protowire.ConsumeTag(entry)
				if k < 0 {
					t.Fatal("SeqEntry: bad tag")
				}
				entry = entry[k:]
				switch {
				case fn == 1 && ft == protowire.BytesType:
					x, j := protowire.ConsumeBytes(entry)
					if j < 0 {
						t.Fatal("SeqEntry: bad author")
					}
					author, entry = x, entry[j:]
				case fn == 2 && ft == protowire.VarintType:
					x, j := protowire.ConsumeVarint(entry)
					if j < 0 {
						t.Fatal("SeqEntry: bad seq")
					}
					seq, entry = x, entry[j:]
				default:
					t.Fatalf("SeqEntry: unexpected field %d", fn)
				}
			}
			got = append(got, struct {
				author string
				seq    uint64
			}{hex.EncodeToString(author), seq})
		}
		if len(got) != len(ev.LastSeq) {
			t.Fatalf("entries = %d, vector %d", len(got), len(ev.LastSeq))
		}
		for i := range got {
			if got[i].author != ev.LastSeq[i].Author || got[i].seq != ev.LastSeq[i].Seq {
				t.Errorf("entry %d = %s/%d, vector %s/%d", i, got[i].author, got[i].seq, ev.LastSeq[i].Author, ev.LastSeq[i].Seq)
			}
			if i > 0 && got[i-1].author >= got[i].author {
				t.Errorf("entries not sorted by author at %d", i)
			}
		}
		return
	}
	t.Fatal("group-close-vector missing")
}
