package node

import (
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/echofog/echofog-core/internal/services/groups/senderkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A group of fifty, run as one long day: people write, drop off and come back (often the author
// is gone again before the reader returns), members are kicked, banned and leave, and the group
// changes owner twice, each new owner removing somebody. Two members run the older version.
// At the end, with everybody online and a few rounds of asking each other for what they missed:
//
//   - every member who stayed has every message written while they were in the group by a member
//     who stayed, exactly once, with its author verified;
//   - everybody who stayed agrees on who is in the group and who owns it;
//   - the current members hold one and the same epoch, made for them only, after the last removal;
//   - nobody removed can open any message written after their removal with the keys they had.
func TestGroupOfFifty(t *testing.T) {
	if testing.Short() {
		t.Skip("fifty members, a few hundred events")
	}
	const (
		size    = 50
		steps   = 300
		maxAway = 15
	)
	seed := int64(7)
	if v, err := strconv.ParseInt(os.Getenv("GROUP50_SEED"), 10, 64); err == nil {
		seed = v // GROUP50_SEED=n go test -run TestGroupOfFifty: another day in the group
	}
	t.Logf("seed %d", seed)
	rng := mrand.New(mrand.NewSource(seed))
	m := newMesh()
	nodes := make([]*meshNode, size)
	for i := range nodes {
		nodes[i] = m.add(t)
	}
	nodes[size-1].legacy, nodes[size-2].legacy = true, true
	owner := nodes[0]
	gd, err := owner.f.Create("Fifty", "", "group")
	require.NoError(t, err)
	g := gd.ID
	joinAll(t, m, owner, g, nodes[1:]...)
	for _, a := range nodes[1:4] {
		require.NoError(t, owner.f.SetRole(g, a.id.String(), "admin"))
	}
	m.settle()

	type posted struct {
		id, text string
		author   *meshNode
		step     int
		members  map[*meshNode]bool // members when it was written
	}
	var all []posted
	inGroup := map[*meshNode]bool{}
	for _, n := range nodes {
		inGroup[n] = true
	}
	removedAt := map[*meshNode]int{}
	stolen := map[*meshNode]*senderkeys.Keyring{} // keys a removed member walked away with
	away := map[*meshNode]bool{}
	admins := map[*meshNode]bool{nodes[1]: true, nodes[2]: true, nodes[3]: true}
	curOwner := owner

	pick := func(ok func(*meshNode) bool) *meshNode {
		var c []*meshNode
		for _, n := range nodes {
			if inGroup[n] && ok(n) {
				c = append(c, n)
			}
		}
		if len(c) == 0 {
			return nil
		}
		return c[rng.Intn(len(c))]
	}
	online := func(n *meshNode) bool { return !away[n] }
	setAway := func(n *meshNode, a bool) {
		away[n] = a
		m.setOnline(n, !a)
	}
	snapshotKeys := func(n *meshNode) {
		if n.legacy {
			return
		}
		raw, _ := json.Marshal(n.keyring(g))
		var kr senderkeys.Keyring
		require.NoError(t, json.Unmarshal(raw, &kr))
		stolen[n] = &kr
	}
	remove := func(step int, actor, target *meshNode, ban bool) {
		snapshotKeys(target)
		require.NoError(t, actor.f.Remove(g, target.id.String(), ban), "step %d", step)
		inGroup[target] = false
		removedAt[target] = step
		m.settle()
	}
	plain := func(n *meshNode) bool { return !admins[n] && n != curOwner && !n.legacy }
	// authors who drop off for good near the end, right after writing while others are away:
	// only the other members can pass on what they wrote
	goneForever := map[*meshNode]bool{}

	for step := 1; step <= steps; step++ {
		switch {
		case step == 60 || step == 140:
			// a kick by an admin (who comes online for it)
			if actor := pick(func(n *meshNode) bool { return admins[n] }); actor != nil {
				setAway(actor, false)
				if target := pick(plain); target != nil {
					remove(step, actor, target, false)
				}
			}
		case step == 90 || step == 220:
			// a ban by the owner
			setAway(curOwner, false)
			if target := pick(plain); target != nil {
				remove(step, curOwner, target, true)
			}
		case step == 120 || step == 250:
			// somebody leaves
			if n := pick(func(n *meshNode) bool { return plain(n) && !goneForever[n] }); n != nil {
				setAway(n, false)
				snapshotKeys(n)
				require.NoError(t, n.f.Leave(g))
				inGroup[n] = false
				removedAt[n] = step
				m.settle()
			}
		case step == 100 || step == 200:
			// the owner hands the group to an admin, who then removes somebody
			setAway(curOwner, false)
			next := pick(func(n *meshNode) bool { return admins[n] && n != curOwner })
			require.NotNil(t, next)
			setAway(next, false)
			require.NoError(t, curOwner.f.TransferOwnership(g, next.id.String()), "step %d", step)
			m.settle()
			admins[curOwner] = true
			delete(admins, next)
			curOwner = next
			if target := pick(plain); target != nil {
				remove(step, curOwner, target, step == 200)
			}
		case step >= 280 && step < 286:
			author := pick(func(n *meshNode) bool { return plain(n) && online(n) && !goneForever[n] })
			if author == nil {
				continue
			}
			// a few readers are away when it is written
			for i := 0; i < 4; i++ {
				if r := pick(func(n *meshNode) bool { return online(n) && n != author && n != curOwner }); r != nil {
					setAway(r, true)
				}
			}
			text := fmt.Sprintf("step %d, last words of %s", step, author.id.String()[:6])
			msg, err := author.f.SendMessage(g, text)
			require.NoError(t, err)
			members := map[*meshNode]bool{}
			for n, in := range inGroup {
				if in {
					members[n] = true
				}
			}
			all = append(all, posted{msg.ID, text, author, step, members})
			m.settle()
			setAway(author, true)
			goneForever[author] = true
		case rng.Intn(100) < 18:
			// somebody drops off or comes back
			n := pick(func(n *meshNode) bool { return !goneForever[n] })
			if n == nil {
				continue
			}
			if away[n] {
				setAway(n, false)
			} else if len(away) < maxAway {
				setAway(n, true)
			}
		default:
			author := pick(online)
			if author == nil {
				continue
			}
			text := fmt.Sprintf("step %d from %s", step, author.id.String()[:6])
			msg, err := author.f.SendMessage(g, text)
			require.NoError(t, err)
			members := map[*meshNode]bool{}
			for n, in := range inGroup {
				if in {
					members[n] = true
				}
			}
			all = append(all, posted{msg.ID, text, author, step, members})
			m.settle()
			// often the author is gone before everybody has it
			if rng.Intn(100) < 25 && len(away) < maxAway && author != curOwner {
				setAway(author, true)
			}
		}
		// clean up the away map for nodes that are no longer in the group
		for n := range away {
			if !inGroup[n] && !away[n] {
				delete(away, n)
			}
		}
	}

	// everybody comes back, but those gone for good; members ask each other for what they missed
	for _, n := range nodes {
		if !goneForever[n] {
			setAway(n, false)
		}
	}
	m.settle()
	m.syncAll(4)

	stay := []*meshNode{} // members who are here at the end
	for _, n := range nodes {
		if inGroup[n] && !goneForever[n] {
			stay = append(stay, n)
		}
	}
	require.NotEmpty(t, goneForever)
	t.Logf("%d here at the end, %d gone for good, %d removed, %d messages; sent: msg=%d smsg=%d key=%d keyreq=%d sync=%d envs=%d",
		len(stay), len(goneForever), len(removedAt), len(all), m.kind("msg"), m.kind("smsg"), m.kind("key"), m.kind("keyreq"), m.kind("sync"), m.kind("envs"))
	require.GreaterOrEqual(t, len(removedAt), 7, "kicks, bans, leaves and the new owners' removals all happened")

	// 1. delivery: exactly once, verified
	for _, n := range stay {
		list, err := n.f.Messages(g, 0)
		require.NoError(t, err, n.id.String()[:6])
		seen := map[string]int{}
		for _, x := range list {
			seen[x.ID]++
			assert.True(t, x.Verified, "%s: %q not verified", n.id.String()[:6], x.Text)
		}
		missing := 0
		for _, p := range all {
			if !inGroup[p.author] || !p.members[n] {
				continue
			}
			if goneForever[p.author] && n.legacy {
				continue // an older node only gets messages from their author
			}
			switch seen[p.id] {
			case 0:
				missing++
				if missing <= 3 {
					t.Errorf("%s (legacy %v) is missing %q", n.id.String()[:6], n.legacy, p.text)
					held := 0
					m.mu.Lock()
					for _, d := range m.held {
						if d.from == p.author.id && d.to == n.id {
							held++
						}
					}
					m.mu.Unlock()
					pend := 0
					for _, ce := range n.f.cachedEnvs(g) {
						if ce.Env.Sender == p.author.id.String() && ce.Pending {
							pend++
							t.Logf("   pending env epoch %d.%s n=%d, holder epochs: %v", ce.Env.Epoch, ce.Env.Tag, ce.Env.N, func() (ids []string) {
								for _, e := range n.keyring(g).List() {
									ids = append(ids, e.ID().String())
								}
								return
							}())
						}
					}
					st := ""
					for _, c := range n.keyring(g).Have() {
						if c.Sender == p.author.id.String() {
							st += fmt.Sprintf(" %s next=%d missing=%v", c.Epoch, c.Next, c.Missing)
						}
					}
					t.Logf("   held=%d pending=%d author in group=%v author legacy=%v chain:%s", held, pend, inGroup[p.author], p.author.legacy, st)
					holders := 0
					for _, o := range stay {
						for _, x := range o.texts(t, g) {
							if x == p.text {
								holders++
							}
						}
					}
					var envs []string
					for _, o := range stay {
						if o.legacy {
							continue
						}
						for _, ce := range o.f.cachedEnvs(g) {
							if ce.Env.Sender == p.author.id.String() {
								envs = append(envs, fmt.Sprintf("%d.%s#%d", ce.Env.Epoch, ce.Env.Tag[:4], ce.Env.N))
							}
						}
						break
					}
					var mine []string
					for _, e := range n.keyring(g).List() {
						mine = append(mine, e.ID().String()[:6])
					}
					t.Logf("   %d members have it; author envelopes at one holder: %v; reader epochs %v; author capable at reader: %v", holders, envs, mine, n.f.capable[p.author.id])
				}
			case 1:
			default:
				t.Errorf("%s has %q %d times", n.id.String()[:6], p.text, seen[p.id])
			}
		}
		if missing > 3 {
			t.Errorf("%s is missing %d messages in all", n.id.String()[:6], missing)
		}
	}

	// 2. one view of the group
	wantMembers := []string{}
	for _, n := range nodes {
		if inGroup[n] {
			wantMembers = append(wantMembers, n.id.String())
		}
	}
	sort.Strings(wantMembers)
	for _, n := range stay {
		list, err := n.f.Members(g)
		require.NoError(t, err)
		var got []string
		for _, x := range list {
			got = append(got, x.PeerID)
		}
		sort.Strings(got)
		assert.Equal(t, wantMembers, got, "%s sees another member list", n.id.String()[:6])
		assert.Equal(t, "owner", roleOf2(t, n, g, curOwner), "%s sees another owner", n.id.String()[:6])
	}

	// 3. one clean epoch, made after the last removal
	var want senderkeys.EpochID
	lastRemoval := 0
	for _, s := range removedAt {
		if s > lastRemoval {
			lastRemoval = s
		}
	}
	for _, n := range stay {
		if n.legacy {
			continue
		}
		cur := n.keyring(g).Current()
		require.NotNil(t, cur, n.id.String()[:6])
		if want.N == 0 {
			want = cur.ID()
		}
		assert.Equal(t, want, cur.ID(), "%s is on another epoch", n.id.String()[:6])
		for gone := range removedAt {
			assert.False(t, cur.Has(gone.id.String()), "the epoch in use was made for a removed member")
		}
	}
	assert.GreaterOrEqual(t, want.N, uint64(len(removedAt)), "every removal brought a new epoch")

	// 4. the keys of the removed open nothing written after they went
	stepOf := map[string]int{}
	for _, p := range all {
		stepOf[p.id] = p.step
	}
	for _, holder := range stay {
		if holder.legacy {
			continue
		}
		for _, ce := range holder.f.cachedEnvs(g) {
			for gone, kr := range stolen {
				raw, _ := json.Marshal(kr)
				var copyKR senderkeys.Keyring
				_ = json.Unmarshal(raw, &copyKR)
				pt, err := copyKR.Open(ce.Env)
				if err != nil {
					continue
				}
				var c senderkeys.Content
				require.NoError(t, json.Unmarshal(pt, &c))
				if s, ok := stepOf[c.ID]; ok && s > removedAt[gone] {
					t.Errorf("a member removed at step %d opened %q (step %d)", removedAt[gone], c.Text, s)
				}
			}
		}
		break // one holder's envelopes are enough: they are the same bytes everywhere
	}
}

func roleOf2(t *testing.T, n *meshNode, group string, who *meshNode) string {
	t.Helper()
	list, err := n.f.Members(group)
	require.NoError(t, err)
	for _, x := range list {
		if x.PeerID == who.id.String() {
			return x.Role
		}
	}
	return ""
}
