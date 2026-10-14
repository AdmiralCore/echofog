package node

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/echofog/echofog-core/internal/services/groups"
	"github.com/echofog/echofog-core/pkg/api/types"
)

func FuzzParsePeer(f *testing.F) {
	f.Add(strings.Repeat("ab", 32))
	f.Add("")
	f.Add("zz")
	f.Add(strings.Repeat("A", 64))
	f.Fuzz(func(t *testing.T, s string) {
		if id, ok := parsePeer(s); ok {
			if !strings.EqualFold(hex.EncodeToString(id[:]), s) {
				t.Fatalf("parsed %q to %x", s, id)
			}
		}
		_, _ = parsePub(s)
	})
}

func FuzzParseCallInvite(f *testing.F) {
	f.Add("fogcall://abc/def")
	f.Add("fogcall://")
	f.Add("fogcall:///x")
	f.Add("fogcall://a/b/c")
	f.Fuzz(func(t *testing.T, s string) {
		id, coord, ok := ParseCallInvite(s)
		if ok && (id == "" || coord == "" || CallInvitePrefix+id+"/"+coord != s) {
			t.Fatalf("parsed %q into %q and %q", s, id, coord)
		}
	})
}

// FuzzGroupMessages throws arbitrary group messages, from any member or outsider, at a group and
// checks the invariants that must hold whatever arrives: one owner, members whose id is the hash
// of their key, and nothing that panics.
func FuzzGroupMessages(f *testing.F) {
	f.Add(byte(0), `{"k":"msg","g":"G","i":"1","t":"hello","ts":5}`)
	f.Add(byte(1), `{"k":"role","g":"G","to":"T","r":"admin"}`)
	f.Add(byte(2), `{"k":"rm","g":"G","to":"T","b":true}`)
	f.Add(byte(0), `{"k":"own","g":"G","to":"T"}`)
	f.Add(byte(1), `{"k":"own","g":"G","to":"S"}`)
	f.Add(byte(2), `{"k":"hist","g":"G","h":[{"i":"h","s":"S","t":"old","ts":1}]}`)
	f.Add(byte(0), `{"k":"snap","g":"G","m":[{"id":"T","p":"00","r":"owner"}]}`)
	f.Add(byte(1), `{"k":"callend","g":"G","c":"x"}`)
	f.Add(byte(3), `{"k":"join","g":"G","p":"00"}`)
	f.Add(byte(0), `{`)

	h := newHub()
	owner, member, other := h.node(f, newMemStore()), h.node(f, newMemStore()), h.node(f, newMemStore())
	g, err := owner.f.Create("Fuzz", "", "group")
	if err != nil {
		f.Fatal(err)
	}
	invite, _ := owner.f.Invite(g.ID)
	_, _ = member.f.Join(invite)
	_, _ = other.f.Join(invite)
	nodes := []*testNode{owner, member, other}
	outsider := h.node(f, newMemStore())

	f.Fuzz(func(t *testing.T, who byte, body string) {
		body = strings.NewReplacer("\"G\"", `"`+g.ID+`"`, `"T"`, `"`+member.id.String()+`"`, `"S"`, `"`+owner.id.String()+`"`).Replace(body)
		sender := outsider.id
		if int(who) < len(nodes) {
			sender = nodes[who].id
		}
		for _, n := range nodes {
			n.f.handle(sender, wirePrefix+body)
		}
		for _, n := range nodes {
			members, err := n.f.mgr.ListMembers(g.ID)
			if err != nil {
				continue // the group was deleted by a valid message
			}
			owners := 0
			for _, m := range members {
				if m.Role.Rank >= groups.RoleOwner.Rank {
					owners++
				}
				if groups.IDOfKey(m.PubKey) != m.PeerID {
					t.Fatalf("member %x does not match its key", m.PeerID)
				}
			}
			if owners != 1 {
				t.Fatalf("%d owners after %q from %x", owners, body, sender[:4])
			}
		}
	})
}

var _ = types.PeerID{}
