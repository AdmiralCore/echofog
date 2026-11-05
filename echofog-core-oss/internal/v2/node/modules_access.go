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
	r.accessMu.Lock()
	defer r.accessMu.Unlock()
	return r.refreshAccessLocked(ctx)
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
	r.accessMu.Lock()
	defer r.accessMu.Unlock()
	return r.refreshAccessLocked(run)
}

func (r *Runtime) accessContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctx
}

// handleAccess books a hidden-node session: an invitation is redeemed, a personal pass renewed,
// and the contact receives the peer card over the session (handshake.md §5). The caller closes
// the session when this returns an error, so an unbooked pass never keeps a session alive.
func (r *Runtime) handleAccess(ctx context.Context, est *session.Established) error {
	if r.accessMgr == nil || est == nil || est.Access == nil {
		return nil
	}
	id := access.PassID(est.Access.PassID)
	var card []byte
	if est.Access.Personal {
		if _, err := r.accessMgr.Renew(ctx, id); err != nil {
			return fmt.Errorf("node: renew personal pass: %w", err)
		}
		if ic, err := r.cfg.Cards.Current(ctx, objects.ScopePeer); err == nil {
			card = ic.Envelope
		}
	} else {
		if est.Peer == nil {
			return fmt.Errorf("%w: no peer card for redemption", ErrConfig)
		}
		redeemed, err := r.accessMgr.Redeem(ctx, access.RedeemRequest{
			PassID: id, Node: est.Peer.NodeID, Device: est.Peer.DeviceID,
		})
		if err != nil {
			return fmt.Errorf("node: redeem invitation: %w", err)
		}
		card = redeemed.PeerCard
	}
	if len(card) > 0 {
		if err := est.PushCard(card); err != nil && !errors.Is(err, mux.ErrFeatureNotSelected) {
			// The peer card also travels in P2 on later sessions; a push failure is not fatal.
			r.log.Debug("node: access peer card push failed", "err", err)
		}
	}
	if err := r.RefreshAccess(ctx); err != nil {
		// The pass is booked and the session is valid; a failed listener refresh must not drop
		// it. The next RefreshAccess retries from the same persistent state.
		r.log.Warn("node: access refresh after a session", "err", err)
	}
	return nil
}

// refreshAccessLocked compares the desired invitation key set with the installed listeners.
// Existing noise3 listeners update in place; any other listener is rebuilt because its key set
// is part of its configuration. A slot without keys is closed (fail closed).
func (r *Runtime) refreshAccessLocked(ctx context.Context) error {
	var errs []error
	if err := r.syncAccessContactsLocked(ctx); err != nil {
		errs = append(errs, err)
	}

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

// syncAccessContactsLocked keeps the card builder on the current contacts key and, after a
// rotation, sends the new peer card to every live contact (objects.md §10.3, T3.6b).
func (r *Runtime) syncAccessContactsLocked(ctx context.Context) error {
	infos := r.accessMgr.ContactsKeys()
	if len(infos) == 0 {
		return nil
	}
	next := append(keys.AKey(nil), infos[0].Public...)
	old := r.accessContacts
	if !r.accessContactsSet {
		old = r.cfg.Device.Masking().Public()
	}
	if r.accessContactsSet && bytes.Equal(old, next) {
		return nil
	}

	changed, err := r.cfg.Cards.ReplaceMask(old, next)
	if err != nil {
		return fmt.Errorf("node: contacts key in the card builder: %w", err)
	}
	r.accessContacts = next
	r.accessContactsSet = true
	if changed == 0 {
		if !bytes.Equal(old, next) {
			r.log.Warn("node: card builder does not use the configured contacts key")
		}
		return nil
	}
	r.mu.Lock()
	table := r.table
	r.mu.Unlock()
	if table == nil || r.cfg.Device == nil {
		return nil
	}
	card, err := r.cfg.Cards.Current(ctx, objects.ScopePeer)
	if err != nil {
		return fmt.Errorf("node: peer card after contacts rotation: %w", err)
	}
	if _, err := table.PushCard(r.cfg.Device.ID(), card.Envelope); err != nil {
		// A session that is draining cannot receive the card; the contact gets it in P2 of the
		// next session and Renew records the coverage. Never fail the rotation for that.
		r.log.Debug("node: rotated peer card push", "err", err)
	}
	return nil
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
	r.deliverReissued(res.Reissued)
	if err := r.RefreshAccess(ctx); err != nil {
		return res, err
	}
	return res, nil
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
	r.deliverReissued(res.Reissued)
	if err := r.RefreshAccess(ctx); err != nil {
		return res, err
	}
	return res, nil
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
	r.deliverReissued(rot.Reissued)
	if err := r.RefreshAccess(ctx); err != nil {
		return rot, err
	}
	return rot, nil
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
