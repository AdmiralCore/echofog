package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/echofog/echofog-core/internal/v2/chat"
	chatcrypto "github.com/echofog/echofog-core/internal/v2/chat/crypto"
	"github.com/echofog/echofog-core/internal/v2/dht"
	"github.com/echofog/echofog-core/internal/v2/mux"
	"github.com/echofog/echofog-core/internal/v2/service"
	"github.com/echofog/echofog-core/internal/v2/store"
)

// ChatNamespace is the store namespace of the chat/2 outbox (AD13). The keys and sessions of
// chat/2 live in the namespaces of internal/v2/chat/crypto.
const ChatNamespace = "v2/chat/outbox"

// chatTick is how often the outbox is looked at; the pauses between attempts are kept by the
// outbox itself (2 s, 5 s, 15 s, 45 s, then a minute).
const chatTick = 2 * time.Second

// Chat returns the delivery service of chat/2, or nil when Config.Chat is not set.
func (r *Runtime) Chat() *chat.Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chat
}

// chatDir looks up contacts through the DHT client, which exists only after Start.
type chatDir struct{ r *Runtime }

func (d chatDir) Resolve(ctx context.Context, n [32]byte) (*dht.Resolved, error) {
	cl := d.r.DHT()
	if cl == nil {
		return nil, errors.New("node: the DHT client is not running")
	}
	return cl.Resolve(ctx, n)
}

// registerChat registers chat/2 (T7.1/T7.2) when Config.Chat is set: BUNDLE_GET and DELIVER are
// served, the device's prekeys/2 go into the DHT records the node publishes and into its cards, and
// the outbox runs. Without the flag the service is not offered and peers get UNSUPPORTED.
func (r *Runtime) registerChat() {
	if !r.cfg.Chat {
		return
	}
	spec, ok := service.Builtin(service.Chat)
	if !ok {
		return
	}
	ids, err := store.NewIDs(r.cfg.Account.StorageKey())
	if err != nil {
		r.log.Warn("node: chat ids", "err", err)
		return
	}
	cr, err := chatcrypto.New(chatcrypto.Config{Device: r.cfg.Device, Store: r.cfg.Store.Store, IDs: ids, Now: r.clock.Now})
	if err != nil {
		ids.Close()
		r.log.Warn("node: chat keys", "err", err)
		return
	}
	cfg := chat.ServiceConfig{
		Self: chat.Self{
			Node:      [32]byte(r.identity.NodeID),
			Device:    [32]byte(r.identity.DeviceID),
			Account:   r.cfg.Account.Public(),
			DeviceKey: r.cfg.Device.Public(),
		},
		Crypto: cr,
		Dir:    chatDir{r},
		Link: &chat.StreamLink{Open: func(ctx context.Context, device [32]byte) (chat.Stream, error) {
			_, st, err := r.Open(ctx, DeviceID(device), service.Chat, DialOpt{})
			if err != nil {
				return nil, err
			}
			return st, nil
		}},
		Store:  r.cfg.Store.Store(ChatNamespace),
		Now:    r.clock.Now,
		Events: r.cfg.ChatEvents,
	}
	if r.cfg.ChatMail != nil {
		cfg.Mail = r.cfg.ChatMail
	}
	svc, err := chat.NewService(cfg)
	if err != nil {
		cr.Close()
		ids.Close()
		r.log.Warn("node: chat service", "err", err)
		return
	}
	srv := chat.New(chat.Config{Bundles: cr, Receiver: svc, Now: r.clock.Now})
	r.Register(spec, func(ctx context.Context, p Peer, st *mux.Stream) {
		srv.Handle(ctx, chat.Peer{SID: p.SID, Path: p.Path, Remote: p.Remote}, st)
	})
	r.mu.Lock()
	r.chat, r.chatKeys = svc, cr
	r.mu.Unlock()
	r.chatPrekeys() // the cards carry the hash from the first one

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { cr.Close(); ids.Close() }()
		t := time.NewTicker(chatTick)
		defer t.Stop()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-t.C:
				_ = svc.Tick(r.ctx)
			}
		}
	}()
}

// chatPrekeys returns the device's current prekeys/2 for the DHT records and keeps the hash in the
// cards (objects.md 3, 5) in step with it. It is nil without chat/2.
func (r *Runtime) chatPrekeys() []byte {
	r.mu.Lock()
	cr := r.chatKeys
	r.mu.Unlock()
	if cr == nil {
		return nil
	}
	raw, err := cr.Prekeys()
	if err != nil {
		r.log.Warn("node: chat prekeys", "err", err)
		return nil
	}
	h := sha256.Sum256(raw)
	if err := r.cfg.Cards.SetPrekeysHash(h[:]); err != nil {
		r.log.Warn("node: prekeys hash for the cards", "err", err)
	}
	return raw
}

// mailboxItems counts the items this node's mailbox relay holds in private queues (tests).
func (r *Runtime) mailboxItems() int {
	if r.mailSrv == nil {
		return -1
	}
	return r.mailSrv.PrivateItems()
}
