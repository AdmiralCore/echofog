package convlog

import (
	"bytes"
	"testing"
)

// A role with the right to write but not to send files posts words and no attachment: the media
// right is weighed by what the body holds, even when the message names no metadata
// (conversation-log.md §2.5, services.md §8).
func TestSendWithoutMediaRejectsAnAttachment(t *testing.T) {
	f, x := newAccount(t), newAccount(t)
	rules := testRules{founder: f.id}
	c := Group("attachmedia1")
	lf, lx := groupReplica(t, f, rules), groupReplica(t, x, rules)
	ingest(t, lx, c, member(t, lf, c, roster(f, "owner", x, "member")))

	def, _, err := lf.AppendRole(c, &RoleDef{Id: "member", Perms: PermSend})
	if err != nil {
		t.Fatal(err)
	}
	ingest(t, lx, c, def)
	if r := lx.RightsOf(c, x.id); !r.Send || r.Media {
		t.Fatalf("the member's rights: %+v", r)
	}
	att := &Attachment{Fid: bytes.Repeat([]byte{7}, 16), Size: 100,
		MerkleRoot: bytes.Repeat([]byte{8}, 32), Name: "x.bin", Mime: "application/octet-stream"}
	if _, _, err := lx.AppendAttachment(c, "", att, "вложение", EventID{}, EventID{}); err != ErrForbidden {
		t.Fatalf("an attachment without the media right: %v", err)
	}
	if _, _, err := lx.AppendMessage(c, "просто текст", 0, "", EventID{}, EventID{}); err != nil {
		t.Fatalf("plain words without the media right: %v", err)
	}
}
