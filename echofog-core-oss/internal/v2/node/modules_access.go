package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/echofog/echofog-core/internal/v2/access"
	"github.com/echofog/echofog-core/internal/v2/carrier"
	"github.com/echofog/echofog-core/internal/v2/carrier/noise3"
	"github.com/echofog/echofog-core/internal/v2/frame"
	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/objects"
	"github.com/echofog/echofog-core/internal/v2/session"
)

// ErrNoAccess means the runtime was built without an access manager.
var ErrNoAccess = errors.New("node: access is not configured")

// AccessMaskKey is one live masking key of an invitation slot. Private is valid only inside an
// AccessListenerFactory call: the runtime wipes it afterwards.
type AccessMaskKey struct {
	Public   keys.AKey
	Private  []byte
	NotAfter time.Time
}

// AccessListenerFactory builds the L0 listener of one invitation slot from the live masking keys.
// It is called at Start and whenever the slot's key set changes. Nil builds the built-in noise3
// listener from the slot address reported by access.Manager.Listener.
type AccessListenerFactory func(slot int, keys []AccessMaskKey) (carrier.Listener, error)

// accessSlot is one installed invitation listener. The key prints are the live key set it was
// built for; a change rebuilds the listener (or updates it in place when it supports that).
type accessSlot struct {
	listener carrier.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	keys     []accessKeyPrint
}

type accessKeyPrint struct {
	public   string
	notAfter int64
}

// maskKeyUpdater is implemented by listeners that can replace their key set in place. The
// built-in noise3 listener implements it, so a revocation does not restart the UDP port.
type maskKeyUpdater interface {
	SetKeys([]noise3.MaskKey) error
}

// registerAccess is the module registration point of the access service (T3.6b). It runs before
// the accept loops; the listeners themselves are installed by startAccess.
func (r *Runtime) registerAccess() {
	if r.accessMgr == nil {
		return
	}
	if r.cfg.Passes == nil {
		r.cfg.Passes = r.accessMgr
	}
}

// startAccess installs the invitation listeners of the access manager.
func (r *Runtime) startAccess(ctx context.Context) error {
	if r.accessMgr == nil {
		return nil
	}
	return r.refreshAccess(ctx)
}

// stopAccess closes every access listener. It is idempotent and does not close the manager: the
// application owns it across runtime restarts.
func (r *Runtime) stopAccess() {
	r.accessMu.Lock()
	defer r.accessMu.Unlock()
	for slot, st := range r.accessSlots {
		r.closeAccessSlotLocked(st)
		delete(r.accessSlots, slot)
	}
}

// Access returns the invitation and pass manager, or nil for a node without access (T3.6b).
func (r *Runtime) Access() *access.Manager { return r.accessMgr }

// RefreshAccess installs the current masking keys of the access manager. The mutating runtime
// helpers call it themselves; an application that uses the manager directly must call it after
// Mint, Revoke, Extend or RotateKeys, otherwise a new key is not reachable at L0.
func (r *Runtime) RefreshAccess(ctx context.Context) error {
	if r.accessMgr == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	run, stopped := r.ctx, r.stopped
	r.mu.Unlock()
	if stopped {
		return ErrClosed
	}
	if run == nil {
		// Listeners are installed by Start; the manager state is already persistent.
		return nil
	}
	return r.refreshAccess(run)
}

// refreshAccess first moves the contacts key (card builder, contacts listeners, card push) and
// then the invitation listeners. The two parts use separate locks: the first one writes the card
// store and sends frames, the second one binds sockets; neither holds up the other.
func (r *Runtime) refreshAccess(ctx context.Context) error {
	var errs []error
	r.contactsMu.Lock()
	if err := r.syncAccessContactsLocked(ctx); err != nil {
		errs = append(errs, err)
	}
	r.contactsMu.Unlock()

	r.accessMu.Lock()
	if err := r.refreshAccessLocked(ctx); err != nil {
		errs = append(errs, err)
	}
	r.accessMu.Unlock()
	return errors.Join(errs...)
}

func (r *Runtime) accessContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// maxAdmitting bounds the pass bookings that run beside the accept loop.
const maxAdmitting = 64

// admission is the pass a hidden-node session was booked with. Only such sessions receive the
// peer card; the pass is the personal pass (an invitation is exchanged for one on redemption).
type admission struct {
	pass access.PassID
	node NodeID
}

// admit books a hidden-node session before it enters the session table: an invitation is
// redeemed, a personal pass renewed (handshake.md section 5). A pass that cannot be booked is an
// error and the caller closes the session without it ever having displaced a live one (wire.md
// 7.3). It returns the peer card to deliver.
func (r *Runtime) admit(ctx context.Context, est *session.Established) (admission, []byte, error) {
	id := access.PassID(est.Access.PassID)
	if est.Peer == nil {
		return admission{}, nil, fmt.Errorf("%w: no peer card for a pass", ErrConfig)
	}
	adm := admission{pass: id, node: NodeID(est.Peer.NodeID)}
	if est.Access.Personal {
		if _, err := r.accessMgr.Renew(ctx, id); err != nil {
			return admission{}, nil, fmt.Errorf("node: renew personal pass: %w", err)
		}
		var card []byte
		if ic, err := r.cfg.Cards.Current(ctx, objects.ScopePeer); err == nil {
			card = ic.Envelope
		}
		return adm, card, nil
	}
	redeemed, err := r.accessMgr.Redeem(ctx, access.RedeemRequest{
		PassID: id, Node: est.Peer.NodeID, Device: est.Peer.DeviceID,
	})
	if err != nil {
		return admission{}, nil, fmt.Errorf("node: redeem invitation: %w", err)
	}
	adm.pass = redeemed.PassID
	return adm, redeemed.PeerCard, nil
}

func (r *Runtime) markAdmitted(est *session.Established, adm admission) {
	r.admitMu.Lock()
	r.admitted[est] = adm
	r.admitMu.Unlock()
}

func (r *Runtime) forgetAdmitted(est *session.Established) {
	if r.accessMgr == nil {
		return
	}
	r.admitMu.Lock()
	delete(r.admitted, est)
	r.admitMu.Unlock()
}

// liveAdmitted returns the admitted sessions whose pass is still active and unrevoked. A session
// whose pass is gone (revoked contact, expired pass) is closed and forgotten: it must neither
// stay up nor learn the rotated contacts key (D11).
func (r *Runtime) liveAdmitted() []*session.Established {
	if r.accessMgr == nil {
		return nil
	}
	now := r.clock.Now()
	r.admitMu.Lock()
	var live, dead []*session.Established
	for est, adm := range r.admitted {
		if _, ok := r.accessMgr.LookupPass(adm.pass[:], now); ok && est.Session != nil {
			live = append(live, est)
			continue
		}
		dead = append(dead, est)
		delete(r.admitted, est)
	}
	r.admitMu.Unlock()
	for _, est := range dead {
		if est.Session != nil {
			go closeRevoked(est)
		}
	}
	return live
}

// closeRevoked ends the session of a contact whose pass is gone: a GOAWAY without a reason tells
// the peer to stop, then the session is closed even if the peer does not drain.
func closeRevoked(est *session.Established) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = est.GoAway(ctx, frame.CodeGoingAway, "", 0)
	_ = est.Session.Close()
}

// pushPeerCard sends the peer card to the sessions that hold an active pass. Other sessions of
// the same local identity (outgoing ones, sessions that never booked a pass) never get it: the
// card carries the contacts key and the hidden listener address.
func (r *Runtime) pushPeerCard(card []byte) {
	if len(card) == 0 {
		return
	}
	for _, est := range r.liveAdmitted() {
		if err := est.PushCard(card); err != nil && !errors.Is(err, mux.ErrFeatureNotSelected) {
			// The peer card also travels in P2 on later sessions; a push failure is not fatal.
			r.log.Debug("node: access peer card push failed", "err", err)
		}
	}
}

// refreshAccessLocked compares the desired invitation key set with the installed listeners.
// Existing noise3 listeners update in place; any other listener is rebuilt because its key set
// is part of its configuration. A slot without keys is closed (fail closed).
func (r *Runtime) refreshAccessLocked(ctx context.Context) error {
	var errs []error
	desired := make(map[int][]access.KeyInfo)
	for _, k := range r.accessMgr.MaskKeys() {
		if k.Contacts || k.Slot < 0 {
			continue
		}
		desired[k.Slot] = append(desired[k.Slot], k)
	}

	for slot, st := range r.accessSlots {
		if len(desired[slot]) == 0 {
			r.closeAccessSlotLocked(st)
			delete(r.accessSlots, slot)
		}
	}

	for slot, infos := range desired {
		prints := accessPrints(infos)
		if st := r.accessSlots[slot]; st != nil {
			if equalAccessPrints(st.keys, prints) {
				continue
			}
			if updater, ok := st.listener.(maskKeyUpdater); ok {
				masks, err := r.accessMasks(infos)
				if err != nil {
					r.closeAccessSlotLocked(st)
					delete(r.accessSlots, slot)
					errs = append(errs, err)
					continue
				}
				if err := updater.SetKeys(masks); err != nil {
					r.closeAccessSlotLocked(st)
					delete(r.accessSlots, slot)
					errs = append(errs, fmt.Errorf("node: access slot %d keys: %w", slot, err))
					continue
				}
				st.keys = prints
				continue
			}
			r.closeAccessSlotLocked(st)
			delete(r.accessSlots, slot)
		}

		st, err := r.openAccessSlotLocked(ctx, slot, infos)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		r.accessSlots[slot] = st
		if !r.addAcceptLoop(st) {
			// Stop already began: keep the listener closed rather than leak a goroutine.
			r.closeAccessSlotLocked(st)
			delete(r.accessSlots, slot)
			continue
		}
	}
	return errors.Join(errs...)
}

// addAcceptLoop counts one accept loop in the runtime wait group unless Stop already began.
func (r *Runtime) addAcceptLoop(st *accessSlot) bool {
	r.mu.Lock()
	if r.ctx == nil || r.stopped {
		r.mu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go r.acceptLoop(st.ctx, st.listener)
	return true
}

// syncAccessContactsLocked (contactsMu held) keeps the contacts key consistent everywhere it
// lives: the contacts listeners (L0) accept the current key and the previous one until its
// RetireAfter, the card builder signs the peer card with the current key, and after a rotation the
// new peer card goes to the sessions that hold an active pass (objects.md 10.3, D11).
func (r *Runtime) syncAccessContactsLocked(ctx context.Context) error {
	infos := r.accessMgr.ContactsKeys()
	if len(infos) == 0 {
		return nil
	}
	var errs []error
	if err := r.syncContactListenersLocked(infos); err != nil {
		errs = append(errs, err)
	}

	next := append(keys.AKey(nil), infos[0].Public...)
	old := r.accessContacts
	if !r.accessContactsSet {
		old = r.cfg.Device.Masking().Public()
	}
	if r.accessContactsSet && bytes.Equal(old, next) {
		return errors.Join(errs...)
	}

	changed, err := r.cfg.Cards.ReplaceMask(old, next)
	if err != nil {
		errs = append(errs, fmt.Errorf("node: contacts key in the card builder: %w", err))
		return errors.Join(errs...)
	}
	r.accessContacts = next
	r.accessContactsSet = true
	if changed == 0 {
		if !bytes.Equal(old, next) {
			r.log.Warn("node: card builder does not use the configured contacts key")
		}
		return errors.Join(errs...)
	}
	card, err := r.cfg.Cards.Current(ctx, objects.ScopePeer)
	if err != nil {
		errs = append(errs, fmt.Errorf("node: peer card after contacts rotation: %w", err))
		return errors.Join(errs...)
	}
	r.pushPeerCard(card.Envelope)
	return errors.Join(errs...)
}

// contactKeyDeadline is the instant after which L0 stops accepting a contacts key: its RetireAfter
// during the overlap, otherwise its NotAfter.
func contactKeyDeadline(k access.KeyInfo) time.Time {
	d := k.NotAfter
	if !k.RetireAfter.IsZero() && (d.IsZero() || k.RetireAfter.Before(d)) {
		d = k.RetireAfter
	}
	return d
}

// syncContactListenersLocked installs the live contacts keys in every contacts listener. A
// listener that cannot take them in place, or refuses them, is closed: it would go on accepting a
// key that was rotated away.
func (r *Runtime) syncContactListenersLocked(infos []access.KeyInfo) error {
	if len(r.contactLs) == 0 {
		return nil
	}
	prints := make([]accessKeyPrint, 0, len(infos))
	for _, k := range infos {
		var na int64
		if d := contactKeyDeadline(k); !d.IsZero() {
			na = d.Unix()
		}
		prints = append(prints, accessKeyPrint{public: string(k.Public), notAfter: na})
	}
	if equalAccessPrints(r.contactPrints, prints) {
		return nil
	}
	masks, err := r.accessMasks(infos)
	if err != nil {
		return err
	}
	for i := range masks {
		masks[i].NotAfter = contactKeyDeadline(infos[i])
	}
	var errs []error
	for i, l := range r.contactLs {
		if l == nil {
			continue
		}
		updater, ok := l.(maskKeyUpdater)
		if !ok {
			_ = l.Close()
			r.contactLs[i] = nil
			errs = append(errs, fmt.Errorf("%w: contacts listener %d cannot replace its keys in place", ErrConfig, i))
			continue
		}
		if err := updater.SetKeys(masks); err != nil {
			_ = l.Close()
			r.contactLs[i] = nil
			errs = append(errs, fmt.Errorf("node: contacts listener %d keys: %w", i, err))
		}
	}
	for i := range masks {
		masks[i].Private = [32]byte{}
	}
	if len(errs) == 0 {
		r.contactPrints = prints
	}
	return errors.Join(errs...)
}

func (r *Runtime) openAccessSlotLocked(ctx context.Context, slot int, infos []access.KeyInfo) (*accessSlot, error) {
	material, masks, err := r.accessKeyMaterial(infos)
	if err != nil {
		return nil, err
	}
	defer wipeAccessMaterial(material)

	var l carrier.Listener
	if r.cfg.AccessListener != nil {
		l, err = r.cfg.AccessListener(slot, material)
	} else {
		l, err = r.builtinAccessListener(ctx, slot, masks)
	}
	if err != nil {
		return nil, fmt.Errorf("node: access slot %d listener: %w", slot, err)
	}
	run := r.accessContext()
	if run == nil {
		_ = l.Close()
		return nil, fmt.Errorf("%w: runtime is not started", ErrClosed)
	}
	slotCtx, cancel := context.WithCancel(run)
	return &accessSlot{listener: l, ctx: slotCtx, cancel: cancel, keys: accessPrints(infos)}, nil
}

// builtinAccessListener builds the noise3 listener of an invitation slot. Other suites need an
// AccessListenerFactory: their listener material (cover certificates, URLs) is not in the card.
func (r *Runtime) builtinAccessListener(ctx context.Context, slot int, masks []noise3.MaskKey) (carrier.Listener, error) {
	l, ok := r.accessMgr.Listener(slot)
	if !ok {
		return nil, fmt.Errorf("%w: access slot %d is not configured", ErrConfig, slot)
	}
	if l.Suite != objects.SuiteNoise3 {
		return nil, fmt.Errorf("%w: access suite %d needs Config.AccessListener", ErrConfig, l.Suite)
	}
	addr := net.JoinHostPort(l.Host, strconv.Itoa(int(l.Port)))
	src := Noise3Listener{
		Addr:     addr,
		Masks:    masks,
		LowPower: r.cfg.Profile.LowPower,
		ReplayKV: r.cfg.Store.Store("v2/carrier/noise3/access/" + strconv.Itoa(slot)),
	}
	return src.Listen(ctx)
}

// accessKeyMaterial copies the private masks out of the manager. The caller must wipe the result.
func (r *Runtime) accessKeyMaterial(infos []access.KeyInfo) ([]AccessMaskKey, []noise3.MaskKey, error) {
	material := make([]AccessMaskKey, 0, len(infos))
	masks := make([]noise3.MaskKey, 0, len(infos))
	for _, k := range infos {
		var priv []byte
		if err := r.accessMgr.UseMaskKey(k.Public, func(b []byte) error {
			priv = append([]byte(nil), b...)
			return nil
		}); err != nil {
			wipeAccessMaterial(material)
			return nil, nil, fmt.Errorf("node: access mask key: %w", err)
		}
		if len(priv) != 32 {
			wipe(priv)
			wipeAccessMaterial(material)
			return nil, nil, fmt.Errorf("%w: masking key of %d bytes", ErrConfig, len(priv))
		}
		var mk noise3.MaskKey
		copy(mk.Private[:], priv)
		mk.NotAfter = k.NotAfter
		material = append(material, AccessMaskKey{
			Public: append(keys.AKey(nil), k.Public...), Private: priv, NotAfter: k.NotAfter,
		})
		masks = append(masks, mk)
	}
	return material, masks, nil
}

// accessMasks returns only the mask keys, for an in-place listener update.
func (r *Runtime) accessMasks(infos []access.KeyInfo) ([]noise3.MaskKey, error) {
	material, masks, err := r.accessKeyMaterial(infos)
	if err != nil {
		return nil, err
	}
	wipeAccessMaterial(material)
	return masks, nil
}

func (r *Runtime) closeAccessSlotLocked(st *accessSlot) {
	if st == nil {
		return
	}
	if st.cancel != nil {
		st.cancel()
	}
	if st.listener != nil {
		_ = st.listener.Close()
	}
}

func accessPrints(infos []access.KeyInfo) []accessKeyPrint {
	out := make([]accessKeyPrint, 0, len(infos))
	for _, k := range infos {
		var na int64
		if !k.NotAfter.IsZero() {
			na = k.NotAfter.Unix()
		}
		out = append(out, accessKeyPrint{public: string(k.Public), notAfter: na})
	}
	return out
}

func equalAccessPrints(a, b []accessKeyPrint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wipeAccessMaterial(material []AccessMaskKey) {
	for i := range material {
		wipe(material[i].Private)
	}
}

// wipe zeroes a secret buffer owned by this package.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---- owner-facing mutating API (T3.6b) -------------------------------------------------------

// MintInvitation creates an invitation and installs its new masking key at L0.
func (r *Runtime) MintInvitation(ctx context.Context, opt access.MintOptions) (*access.Minted, error) {
	if r.accessMgr == nil {
		return nil, ErrNoAccess
	}
	minted, err := r.accessMgr.Mint(ctx, opt)
	if err != nil {
		return nil, err
	}
	if err := r.RefreshAccess(ctx); err != nil {
		return minted, err
	}
	return minted, nil
}

// RevokeAccess revokes a pass and installs the new key set. The returned Reissued invitations
// carry new secrets and links: the caller delivers them to their holders and must never log them.
func (r *Runtime) RevokeAccess(ctx context.Context, id access.PassID) (*access.RevokeResult, error) {
	if r.accessMgr == nil {
		return nil, ErrNoAccess
	}
	res, err := r.accessMgr.Revoke(ctx, id)
	if err != nil {
		return nil, err
	}
	r.liveAdmitted()
	return res, r.refreshThenDeliver(ctx, res.Reissued)
}

// RevokeContactAccess revokes a contact's personal passes and rotates the contacts key. The
// returned Reissued invitations are for the owner to deliver; never log them.
func (r *Runtime) RevokeContactAccess(ctx context.Context, node NodeID) (*access.RevokeResult, error) {
	if r.accessMgr == nil {
		return nil, ErrNoAccess
	}
	res, err := r.accessMgr.RevokeContact(ctx, node)
	if err != nil {
		return nil, err
	}
	r.liveAdmitted()
	return res, r.refreshThenDeliver(ctx, res.Reissued)
}

// ExtendAccess extends a pass and refreshes the masking-key deadlines.
func (r *Runtime) ExtendAccess(ctx context.Context, id access.PassID, d time.Duration) (access.Info, error) {
	if r.accessMgr == nil {
		return access.Info{}, ErrNoAccess
	}
	info, err := r.accessMgr.Extend(ctx, id, d)
	if err != nil {
		return access.Info{}, err
	}
	if err := r.RefreshAccess(ctx); err != nil {
		return info, err
	}
	return info, nil
}

// RotateAccessKeys rotates the contacts and invitation keys (D11). The returned Reissued
// invitations carry new secrets and links: the caller delivers them to their holders and must
// never log them.
func (r *Runtime) RotateAccessKeys(ctx context.Context) (*access.Rotation, error) {
	if r.accessMgr == nil {
		return nil, ErrNoAccess
	}
	rot, err := r.accessMgr.RotateKeys(ctx)
	if err != nil {
		return nil, err
	}
	r.liveAdmitted()
	return rot, r.refreshThenDeliver(ctx, rot.Reissued)
}

// refreshThenDeliver installs the new keys at L0 and only then hands the reissued links to the
// application: a holder must not get a link that L0 does not accept yet. A failed refresh does not
// withhold them, since the manager already revoked the old ones; the error is returned.
func (r *Runtime) refreshThenDeliver(ctx context.Context, list []access.Minted) error {
	err := r.RefreshAccess(ctx)
	r.deliverReissued(list)
	return err
}

// deliverReissued hands new invitation links to the application, the only channel that can
// reach an unredeemed holder. The payload carries secrets: it is never logged here.
func (r *Runtime) deliverReissued(list []access.Minted) {
	if len(list) == 0 || r.cfg.AccessReissued == nil {
		return
	}
	r.cfg.AccessReissued(list)
}

// AccessList returns the owner-facing pass list. The entries contain invitation secrets and
// links: never log them.
func (r *Runtime) AccessList(ctx context.Context) ([]access.Info, error) {
	if r.accessMgr == nil {
		return nil, ErrNoAccess
	}
	return r.accessMgr.List(ctx)
}
