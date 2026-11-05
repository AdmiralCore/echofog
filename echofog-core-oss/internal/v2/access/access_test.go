package access_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/handshake"
	"github.com/echofog/echofog-core/internal/v2/identity"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestMintCreatesInvitationCardAndLink covers the invitation material of objects.md §10.3 and
// handshake.md §5: one fresh masking key, the pass_id as invite_id, a link that verifies offline.
func TestMintCreatesInvitationCardAndLink(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(minted.Secret) != 16 {
		t.Fatalf("secret is %d bytes", len(minted.Secret))
	}
	if got := objects.PassID(minted.Secret); !bytes.Equal(got[:], minted.PassID[:]) {
		t.Fatal("pass_id is not derived from the secret")
	}
	if minted.Link == "" || len(minted.Card) == 0 {
		t.Fatal("no link or card")
	}

	card := linkCard(t, minted.Link)
	if card.Scope != objects.ScopeInvite {
		t.Fatalf("scope %v", card.Scope)
	}
	if !bytes.Equal(card.Body.GetInviteId(), minted.PassID[:]) {
		t.Fatal("invite_id is not the pass_id")
	}
	if len(card.Body.GetAddrs()) != 1 || len(card.Body.GetMaskKeys()) != 1 {
		t.Fatalf("invitation card carries %d addrs and %d mask keys",
			len(card.Body.GetAddrs()), len(card.Body.GetMaskKeys()))
	}
	if got := cardMasks(t, card); len(got) != 1 || !bytes.Equal(got[0], minted.MaskKey) {
		t.Fatal("address does not point to the invitation's masking key")
	}
	if !hasMask(card, minted.MaskKey) {
		t.Fatal("invitation masking key missing")
	}
	// The runtime gets the private half only through UseMaskKey; it must match the published key.
	var privCopy []byte
	err = r.mgr.UseMaskKey(minted.MaskKey, func(priv []byte) error {
		privCopy = append([]byte(nil), priv...)
		return nil
	})
	if err != nil {
		t.Fatalf("UseMaskKey: %v", err)
	}
	kp, err := identity.KeyPairFromPrivate(privCopy)
	if err != nil {
		t.Fatalf("private masking key: %v", err)
	}
	if !bytes.Equal(kp.Public(), minted.MaskKey) {
		t.Fatal("UseMaskKey returned the wrong private key")
	}
	kp.Destroy()

	pass, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now())
	if !ok || !bytes.Equal(pass.Secret, minted.Secret) || pass.Personal {
		t.Fatalf("LookupPass: %+v %v", pass, ok)
	}

	list, err := r.mgr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d passes", len(list))
	}
	p := list[0]
	if p.Kind != access.Invitation || p.State != access.Active {
		t.Fatalf("pass %+v", p)
	}
	if !bytes.Equal(p.Secret, minted.Secret) || !bytes.Equal(p.MaskKey, minted.MaskKey) {
		t.Fatal("list does not describe the minted pass")
	}
	if p.LinkExpires.Sub(r.clock.Now()) != access.DefaultLinkTTL {
		t.Fatalf("link expires in %v", p.LinkExpires.Sub(r.clock.Now()))
	}
	if p.MaxEntries != 1 || p.Entries != 0 {
		t.Fatalf("entries %d/%d", p.Entries, p.MaxEntries)
	}
}

// TestInviteCardNeverCarriesTheContactsKeyOrAddress is the explicit decision on the T4.2a
// Medium risk: an invitation card names only the invitation's own key and listener, never the
// contacts masking key or the contacts listener (objects.md §10.3, wire.md §2.1).
func TestInviteCardNeverCarriesTheContactsKeyOrAddress(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	card := linkCard(t, minted.Link)
	contacts := r.device.Masking().Public()
	if hasMask(card, contacts) {
		t.Fatal("invitation card leaks the contacts masking key")
	}
	for _, a := range card.Body.GetAddrs() {
		if a.GetHost() == "198.51.100.20" || a.GetPort() == 5555 {
			t.Fatal("invitation card leaks the contacts listener")
		}
	}
	if len(card.Body.GetMaskKeys()) != 1 {
		t.Fatalf("%d masking keys in an invitation card", len(card.Body.GetMaskKeys()))
	}

	// The peer card handed out after redemption is the opposite: only the contacts key.
	_, peer := redeem(t, r, minted, r.clientNode(t))
	pc, err := objects.VerifyCard(peer, r.clock.Now())
	if err != nil {
		t.Fatalf("peer card: %v", err)
	}
	if pc.Scope != objects.ScopePeer {
		t.Fatalf("scope %v", pc.Scope)
	}
	if !hasMask(pc, contacts) {
		t.Fatal("peer card does not carry the contacts key")
	}
	if hasMask(pc, minted.MaskKey) {
		t.Fatal("peer card leaks the invitation key")
	}
}

// TestRevokedUnredeemedInvitationStopsAtL0 checks that revoking an invitation deletes its masking
// key from the live set immediately, so its link stops working at L0 (objects.md §10.3).
func TestRevokedUnredeemedInvitationStopsAtL0(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !hasKey(r.mgr.MaskKeys(), minted.MaskKey) {
		t.Fatal("invitation key is not live after Mint")
	}

	rev, err := r.mgr.Revoke(ctx, minted.PassID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if rev.Pass.State != access.Revoked {
		t.Fatalf("state %v", rev.Pass.State)
	}
	if hasKey(r.mgr.MaskKeys(), minted.MaskKey) {
		t.Fatal("revoked invitation key is still live at L0")
	}
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); ok {
		t.Fatal("revoked invitation pass still admits")
	}
	if len(rev.Reissued) != 0 {
		t.Fatalf("%d reissued passes for a private key", len(rev.Reissued))
	}
}

// TestSharedKeyRevokeReissuesTheOtherInvitations covers the shared-key branch of objects.md §10.3:
// a listener holds at most 8 keys (wire.md §2.1), so a ninth invitation shares a key; revoking one
// reissues the rest on a fresh key and invalidates the old links.
func TestSharedKeyRevokeReissuesTheOtherInvitations(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	var min []*access.Minted
	for i := 0; i < access.KeysPerListener+1; i++ {
		m, err := r.mgr.Mint(ctx, access.MintOptions{})
		if err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
		min = append(min, m)
	}
	seen := map[string]int{}
	for _, m := range min {
		seen[string(m.MaskKey)]++
	}
	if len(seen) != access.KeysPerListener {
		t.Fatalf("%d distinct keys, want %d", len(seen), access.KeysPerListener)
	}

	// Find the pair that shares a key.
	var a, b *access.Minted
	var shared []byte
	for i := range min {
		for j := i + 1; j < len(min); j++ {
			if bytes.Equal(min[i].MaskKey, min[j].MaskKey) {
				a, b, shared = min[i], min[j], min[i].MaskKey
			}
		}
	}
	if a == nil {
		t.Fatal("no shared key")
	}

	rev, err := r.mgr.Revoke(ctx, a.PassID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(rev.Reissued) != 1 {
		t.Fatalf("%d reissued, want 1", len(rev.Reissued))
	}
	again := rev.Reissued[0]
	if again.PassID == b.PassID {
		t.Fatal("reissued invitation kept the old pass_id")
	}
	if bytes.Equal(again.Secret, b.Secret) {
		t.Fatal("reissued invitation kept the old secret")
	}
	if bytes.Equal(again.MaskKey, shared) {
		t.Fatal("reissued invitation kept the shared key")
	}
	if hasKey(r.mgr.MaskKeys(), shared) {
		t.Fatal("the shared key is still live after revoking one of its invitations")
	}
	if _, ok := r.mgr.LookupPass(b.PassID[:], r.clock.Now()); ok {
		t.Fatal("the old link still admits")
	}
	if _, ok := r.mgr.LookupPass(again.PassID[:], r.clock.Now()); !ok {
		t.Fatal("the reissued pass does not admit")
	}
	card := linkCard(t, again.Link)
	if !bytes.Equal(card.Body.GetInviteId(), again.PassID[:]) {
		t.Fatal("reissued card names another pass")
	}
	if !hasMask(card, again.MaskKey) {
		t.Fatal("reissued card does not use the new key")
	}
}

// TestRedeemIssuesPersonalPassAndPeerCard covers the first complete session (handshake.md §5).
func TestRedeemIssuesPersonalPassAndPeerCard(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := r.clientNode(t)
	red, peer := redeem(t, r, minted, client)

	if len(red.Secret) != 16 || red.PassID == minted.PassID {
		t.Fatalf("personal pass %+v", red)
	}
	if red.Expires.Sub(r.clock.Now()) != access.DefaultAccessTTL {
		t.Fatalf("access expires in %v", red.Expires.Sub(r.clock.Now()))
	}
	pc, err := objects.VerifyCard(peer, r.clock.Now())
	if err != nil {
		t.Fatalf("peer card: %v", err)
	}
	if pc.Scope != objects.ScopePeer || !hasMask(pc, r.device.Masking().Public()) {
		t.Fatal("peer card is not the contacts card")
	}

	pass, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now())
	if !ok || !pass.Personal || !bytes.Equal(pass.Secret, red.Secret) {
		t.Fatalf("personal pass does not admit: %+v %v", pass, ok)
	}
	// One-use invitation: consumed, its key gone, and a second redemption is refused.
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); ok {
		t.Fatal("consumed invitation still admits")
	}
	if hasKey(r.mgr.MaskKeys(), minted.MaskKey) {
		t.Fatal("consumed invitation key is still live")
	}
	if _, err := r.mgr.Redeem(ctx, access.RedeemRequest{PassID: minted.PassID, Node: client.account.Public().ID(), Device: client.device.ID()}); !errors.Is(err, access.ErrUsedUp) {
		t.Fatalf("second Redeem: %v", err)
	}

	list, err := r.mgr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var invitation, personal *access.Info
	for i := range list {
		switch list[i].Kind {
		case access.Invitation:
			invitation = &list[i]
		case access.Personal:
			personal = &list[i]
		}
	}
	if invitation == nil || invitation.State != access.Used {
		t.Fatalf("invitation record: %+v", invitation)
	}
	if personal == nil || personal.State != access.Active {
		t.Fatalf("personal record: %+v", personal)
	}
	if personal.Contact != client.account.Public().ID() || personal.Device != client.device.ID() {
		t.Fatal("personal pass is not bound to the contact")
	}
}

// TestMultipleEntries covers the use count of an invitation: each entry is a distinct contact and
// the invitation dies when its entries are spent.
func TestMultipleEntries(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{MaxEntries: 2})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	first := r.clientNode(t)
	second := r.clientNode(t)
	redeem(t, r, minted, first)
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); !ok {
		t.Fatal("invitation died after the first of two entries")
	}
	redeem(t, r, minted, second)
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); ok {
		t.Fatal("invitation still admits after both entries")
	}
	if hasKey(r.mgr.MaskKeys(), minted.MaskKey) {
		t.Fatal("spent invitation key is still live")
	}
}

// TestRevokedContactGetsSilentClose covers handshake.md §5: deleting the personal pass makes the
// next handshake fail with the uniform silent close, and the contacts key is rotated early
// (objects.md §10.3).
func TestRevokedContactGetsSilentClose(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := r.clientNode(t)
	red, _ := redeem(t, r, minted, client)

	old := r.mgr.ContactsKeys()
	if len(old) == 0 {
		t.Fatal("no contacts key")
	}
	oldPub := old[0].Public

	rev, err := r.mgr.RevokeContact(ctx, client.account.Public().ID())
	if err != nil {
		t.Fatalf("RevokeContact: %v", err)
	}
	if rev.Contacts == nil || bytes.Equal(rev.Contacts.ContactsKey, oldPub) {
		t.Fatalf("contacts key was not rotated: %+v", rev.Contacts)
	}
	if _, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now()); ok {
		t.Fatal("revoked contact still admits")
	}
	// No other contact needs the old card, so the previous key may retire immediately; when a
	// contact remains it stays accepted during the overlap (covered by TestRenewAndContactsKeyCoverage).

	sr, rep := r.handshake(t, client, red.Secret, minted.Card)
	if sr != nil {
		t.Fatal("revoked contact completed a handshake")
	}
	if rep.Action != handshake.ActSilentClose || len(rep.Payload) != 0 {
		t.Fatalf("reply %+v, want a silent close", rep)
	}
}

// TestExtendAndExpiry covers the link term, the access term after entry and the one-year cap.
func TestExtendAndExpiry(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{LinkTTL: time.Hour, AccessTTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	r.clock.Advance(2 * time.Hour)
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); ok {
		t.Fatal("expired link still admits")
	}
	list, err := r.mgr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list[0].State != access.Expired {
		t.Fatalf("state %v", list[0].State)
	}
	if _, err := r.mgr.Extend(ctx, minted.PassID, 3*time.Hour); err != nil {
		t.Fatalf("Extend invitation: %v", err)
	}
	if _, ok := r.mgr.LookupPass(minted.PassID[:], r.clock.Now()); !ok {
		t.Fatal("extended link does not admit")
	}

	client := r.clientNode(t)
	red, _ := redeem(t, r, minted, client)
	r.clock.Advance(23 * time.Hour)
	if _, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now()); !ok {
		t.Fatal("personal pass expired before its term")
	}
	r.clock.Advance(2 * time.Hour)
	if _, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now()); ok {
		t.Fatal("personal pass outlived its term")
	}

	// Extending a personal pass caps at one year from now (handshake.md §5).
	info, err := r.mgr.Extend(ctx, red.PassID, 400*24*time.Hour)
	if err != nil {
		t.Fatalf("Extend personal: %v", err)
	}
	max := r.clock.Now().Add(access.MaxPersonalTTL)
	if info.AccessExpires.After(max) {
		t.Fatalf("personal pass expires %v beyond %v", info.AccessExpires, max)
	}
	if _, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now()); !ok {
		t.Fatal("extended personal pass does not admit")
	}
}

// TestRenewAndContactsKeyCoverage covers renewal in later sessions and retirement of the previous
// contacts key once every recently seen contact has the new card (objects.md §10.3).
func TestRenewAndContactsKeyCoverage(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := r.clientNode(t)
	red, _ := redeem(t, r, minted, client)

	before, _ := r.mgr.List(ctx)
	var oldExpiry time.Time
	for _, p := range before {
		if p.Kind == access.Personal {
			oldExpiry = p.AccessExpires
		}
	}
	r.clock.Advance(10 * 24 * time.Hour)
	info, err := r.mgr.Renew(ctx, red.PassID)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !info.AccessExpires.After(oldExpiry.Add(9 * 24 * time.Hour)) {
		t.Fatalf("renew did not extend: %v", info.AccessExpires)
	}

	rot, err := r.mgr.RotateKeys(ctx)
	if err != nil {
		t.Fatalf("RotateKeys: %v", err)
	}
	if len(rot.Reissued) != 0 {
		t.Fatalf("%d reissued invitations, want none", len(rot.Reissued))
	}
	if !hasKey(r.mgr.ContactsKeys(), rot.PreviousKey) {
		t.Fatal("previous contacts key is not in the overlap")
	}
	// The contact has not been told about the new card yet, so the old key stays.
	if _, err := r.mgr.Renew(ctx, red.PassID); err != nil {
		t.Fatalf("Renew after rotation: %v", err)
	}
	if hasKey(r.mgr.ContactsKeys(), rot.PreviousKey) {
		t.Fatal("previous contacts key still live after every contact has the new card")
	}
	if !hasKey(r.mgr.ContactsKeys(), rot.ContactsKey) {
		t.Fatal("current contacts key missing")
	}
}

// TestRotateKeysReissuesInvitations covers D11: after a possible masking-key theft every invitation
// gets new key material and a new link, and the old invitation keys stop at L0 at once.
func TestRotateKeysReissuesInvitations(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	var minted []*access.Minted
	for i := 0; i < 3; i++ {
		m, err := r.mgr.Mint(ctx, access.MintOptions{})
		if err != nil {
			t.Fatalf("Mint %d: %v", i, err)
		}
		minted = append(minted, m)
	}
	rot, err := r.mgr.RotateKeys(ctx)
	if err != nil {
		t.Fatalf("RotateKeys: %v", err)
	}
	if len(rot.Reissued) != len(minted) {
		t.Fatalf("%d reissued, want %d", len(rot.Reissued), len(minted))
	}
	for i, old := range minted {
		if hasKey(r.mgr.MaskKeys(), old.MaskKey) {
			t.Fatalf("invitation %d key still live after rotation", i)
		}
		if _, ok := r.mgr.LookupPass(old.PassID[:], r.clock.Now()); ok {
			t.Fatalf("invitation %d still admits after rotation", i)
		}
		again := rot.Reissued[i]
		if _, ok := r.mgr.LookupPass(again.PassID[:], r.clock.Now()); !ok {
			t.Fatalf("reissued invitation %d does not admit", i)
		}
		if _, err := linkCardAt(t, again.Link, r.clock.Now()); err != nil {
			t.Fatalf("reissued link %d: %v", i, err)
		}
	}
}

// TestStateSurvivesRestart covers the persistent state of AD13.
func TestStateSurvivesRestart(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := r.clientNode(t)
	red, _ := redeem(t, r, minted, client)
	before := r.mgr.MaskKeys()
	if len(before) == 0 {
		t.Fatal("no keys")
	}

	r.reopen(t)

	if _, ok := r.mgr.LookupPass(red.PassID[:], r.clock.Now()); !ok {
		t.Fatal("personal pass lost across restart")
	}
	list, err := r.mgr.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("%d passes after restart", len(list))
	}
	for _, p := range list {
		if p.Kind == access.Personal && !bytes.Equal(p.Secret, red.Secret) {
			t.Fatal("personal secret changed across restart")
		}
	}
	if len(r.mgr.MaskKeys()) == 0 {
		t.Fatal("masking keys lost across restart")
	}
}

// TestErrorsAndCaps pins the error distinctions the API promises and the invitation card bound:
// an extension never moves a link past the card's not_after (T4.2a, objects.md §3.2).
func TestErrorsAndCaps(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	var unknown access.PassID
	unknown[0] = 0xff
	if _, err := r.mgr.Revoke(ctx, unknown); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("Revoke unknown: %v", err)
	}
	if _, err := r.mgr.Redeem(ctx, access.RedeemRequest{PassID: unknown, Node: [32]byte{1}}); !errors.Is(err, access.ErrNotFound) {
		t.Fatalf("Redeem unknown: %v", err)
	}
	info, err := r.mgr.Extend(ctx, minted.PassID, 100*24*time.Hour)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	card, err := linkCardAt(t, info.Link, r.clock.Now())
	if err != nil {
		t.Fatalf("link after extension: %v", err)
	}
	if info.LinkExpires.Unix() > int64(card.Env.NotAfter)+1 {
		t.Fatalf("link %v past the card's not_after %d", info.LinkExpires, card.Env.NotAfter)
	}
}

func TestHandshakeReportsAccessPass(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()

	minted, err := r.mgr.Mint(ctx, access.MintOptions{})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	client := r.clientNode(t)

	sr, rep := r.handshake(t, client, minted.Secret, minted.Card)
	if sr == nil {
		t.Fatalf("handshake failed: %+v", rep)
	}
	if sr.Access == nil || sr.Access.Personal {
		t.Fatalf("access info %+v", sr.Access)
	}
	if !bytes.Equal(sr.Access.PassID[:], minted.PassID[:]) {
		t.Fatal("wrong pass reported")
	}

	red, _ := redeem(t, r, minted, client)
	sr, rep = r.handshake(t, client, red.Secret, minted.Card)
	if sr == nil {
		t.Fatalf("personal handshake failed: %+v", rep)
	}
	if sr.Access == nil || !sr.Access.Personal || !bytes.Equal(sr.Access.PassID[:], red.PassID[:]) {
		t.Fatalf("personal access info %+v", sr.Access)
	}
}

// redeem performs a redemption with the client identity and returns the personal pass and peer card.
func redeem(t *testing.T, r *rig, m *access.Minted, client *node) (*access.Redeemed, []byte) {
	t.Helper()
	red, err := r.mgr.Redeem(t.Context(), access.RedeemRequest{
		PassID: m.PassID,
		Node:   client.account.Public().ID(),
		Device: client.device.ID(),
	})
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	return red, red.PeerCard
}

// handshake runs an XK handshake against the hidden node with the pass secret. It returns the
// responder session (nil when the responder did not send message 2) and the responder's reply.
func (r *rig) handshake(t *testing.T, client *node, secret, remoteCard []byte) (*handshake.Session, handshake.Reply) {
	t.Helper()
	remote, err := handshake.DefaultCardVerifier{}.VerifyCard(remoteCard, r.clock.Now())
	if err != nil {
		t.Fatalf("remote card: %v", err)
	}
	cb := bytes.Repeat([]byte{0x42}, 32)
	ini, err := handshake.NewInitiator(handshake.InitiatorConfig{
		Static:       client.static(t),
		Card:         client.peerCard(t),
		Remote:       remote,
		Suites:       []uint16{handshake.SuiteXK},
		AccessSecret: secret,
		Now:          r.clock.Now,
	}, cb)
	if err != nil {
		t.Fatalf("initiator: %v", err)
	}
	resp := handshake.NewResponder(handshake.ResponderConfig{
		Static:   r.srv.static(t),
		DeviceID: r.srv.device.ID(),
		OwnCards: r.srv.ownCards(t),
		Hidden:   true,
		Passes:   r.mgr,
		Now:      r.clock.Now,
	})
	hello, m1, err := ini.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	reply := resp.HandleFirst(cb, hello, m1, r.clock.Now())
	if reply.Action != handshake.ActSend {
		return nil, reply
	}
	m3, _, err := ini.HandleMsg2(reply.Payload)
	if err != nil {
		t.Fatalf("message 2: %v", err)
	}
	sr, err := resp.HandleFinish(m3)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return sr, reply
}

func hasKey(keys []access.KeyInfo, pub []byte) bool {
	for _, k := range keys {
		if bytes.Equal(k.Public, pub) {
			return true
		}
	}
	return false
}
