package convlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

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
	if !bytes.Equal(copyBytes, spec) {
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

// TestConvlogAttachmentIsUnknownToThisBuild checks what the vector relies on: this build has no
// attachment field, so an old client keeps MsgBody field 6 as an unknown field of the signed body.
func TestConvlogAttachmentIsUnknownToThisBuild(t *testing.T) {
	v := loadVectors(t)
	var msg *Event
	for _, ev := range v.Events {
		if ev.Name == "msg-att" {
			e, _, err := Decode(mustHex(t, ev.Event))
			if err != nil {
				t.Fatal(err)
			}
			msg = e
		}
	}
	if msg == nil {
		t.Fatal("msg-att missing")
	}
	var body MsgBody
	if err := proto.Unmarshal(msg.Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.GetText() != "вот схема" {
		t.Errorf("text = %q", body.GetText())
	}
	if len(body.ProtoReflect().GetUnknown()) == 0 {
		t.Error("attachment (field 6) not kept as an unknown field")
	}
}

// TestConvlogGroupCloseKindIsUnknownToThisBuild: kind 10 is accepted by Decode but not known to
// this build, so its events are stored and synced, not shown (versioning §3.9).
func TestConvlogGroupCloseKindIsUnknownToThisBuild(t *testing.T) {
	v := loadVectors(t)
	for _, ev := range v.Events {
		if ev.Name != "group-close" {
			continue
		}
		e, _, err := Decode(mustHex(t, ev.Event))
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind != 10 {
			t.Errorf("kind = %d, want 10", e.Kind)
		}
		if KnownKind(e.Kind) {
			t.Error("kind 10 is known to this build")
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
