package messenger

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"github.com/echofog/echofog-core/internal/crypto/x3dh"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/crypto"
	"github.com/echofog/echofog-core/internal/crypto/encrypt_decrypt"
	"github.com/echofog/echofog-core/internal/network/net_types"
	"github.com/echofog/echofog-core/internal/p2p/gossip"
	"github.com/echofog/echofog-core/internal/p2p/swarm"
	"github.com/echofog/echofog-core/internal/protection/core/identity"
	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"github.com/echofog/echofog-core/internal/services/contacts"
	"github.com/echofog/echofog-core/internal/storage"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

type MessageBroker struct {
	crypto     *crypto.CryptoEngine
	sw         *swarm.Swarm
	gsp        *gossip.Manager
	identity   *identity.ProtectedIdentity
	keeper     *MessageKeeper
	kv         storage.Storage // raw key-value store (outbox)
	contacts   *contacts.ContactService
	log        *slog.Logger
	sharedKeys map[types.PeerID][]byte

	mu              sync.RWMutex
	handleCallbacks []func(dto *dto.MessageDTO)
	interceptor     func(origin types.PeerID, text string) bool
	eventSink       EventSink                         // conversation-log traffic (nil: treated as plain messages)
	filter          func(origin types.PeerID) Verdict // who may write to us (nil: everybody, as contacts)
	connect         func(types.PeerID)                // asked to open a connection to a peer that cannot be reached
	useAsRelay      func(types.PeerID) bool           // a peer to route through even if it does not advertise (a configured private relay)
	relayFull       func(types.PeerID) bool           // a relay that said it keeps no more messages, tried last for a while
	mailboxesOf     func(types.PeerID) []types.PeerID // the relays a recipient named for its mail, tried first
	connMu          sync.Mutex
	lastConnect     map[types.PeerID]time.Time
	lastReset       map[types.PeerID]time.Time
	lastKnock       map[types.PeerID]time.Time
	stateCallbacks  []func(MessageState) // told when a message of ours went out, was confirmed or expired
	stateMu         sync.Mutex
	stateQ          []MessageState  // states not yet told to the callbacks, in the order they happened
	stateRunning    bool            // a goroutine is telling them
	recent          *recentMessages // which message a ratchet header carried, to confirm repeats

	myIDHexed string
}

func NewMessageBroker(crypto *crypto.CryptoEngine, sw *swarm.Swarm, gsp *gossip.Manager, identity *identity.ProtectedIdentity, storage storage.MessageStorage, contact *contacts.ContactService, logger *slog.Logger) *MessageBroker {
	myID, _ := identity.GetPeerID()
	myIDHex := hex.EncodeToString(myID[:])

	return &MessageBroker{
		crypto:     crypto,
		sw:         sw,
		gsp:        gsp,
		identity:   identity,
		keeper:     NewMessageKeeper(storage),
		kv:         storage,
		contacts:   contact,
		log:        logger,
		sharedKeys: make(map[types.PeerID][]byte),
		myIDHexed:  myIDHex,
	}
}

func (s *MessageBroker) Name() string {
	return "MessageBroker"
}

func (s *MessageBroker) Start(ctx context.Context) error {
	return nil
}

func (s *MessageBroker) Stop() error {
	return nil
}

// RegisterSharedKey stores a fast symmetric 32-byte key for ChaCha20 encryption with a peer
func (s *MessageBroker) RegisterSharedKey(peerID types.PeerID, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sharedKeys[peerID] = key
}

// GetSharedKey retrieves a registered symmetric key for ChaCha20
func (s *MessageBroker) GetSharedKey(peerID types.PeerID) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.sharedKeys[peerID]
	return k, ok
}

// SendChatMessage is the default high-security chat method (TierX3DH with Double Ratchet)
func (s *MessageBroker) SendChatMessage(targetID types.PeerID, text string) error {
	return s.SendChatMessageWithTier(targetID, text, TierX3DH, SendOptions{})
}

// SendChatMessageX3DH sends a message with full Signal X3DH + Double Ratchet forward secrecy
func (s *MessageBroker) SendChatMessageX3DH(targetID types.PeerID, text string, appID string) error {
	return s.SendChatMessageWithTier(targetID, text, TierX3DH, SendOptions{AppID: appID})
}

// SendChatMessageChaCha sends a fast stateless message using XChaCha20-Poly1305 symmetric encryption
func (s *MessageBroker) SendChatMessageChaCha(targetID types.PeerID, text string, sharedKey []byte, appID string) error {
	return s.SendChatMessageWithTier(targetID, text, TierChaCha20, SendOptions{AppID: appID, SharedKey: sharedKey})
}

// SendChatMessagePlain sends an unencrypted message (signed by sender Ed25519 identity envelope)
func (s *MessageBroker) SendChatMessagePlain(targetID types.PeerID, text string, appID string) error {
	return s.SendChatMessageWithTier(targetID, text, TierPlain, SendOptions{AppID: appID})
}

// SendChatMessageWithTier sends a message using the requested cryptographic tier and application options
func (s *MessageBroker) SendChatMessageWithTier(targetID types.PeerID, text string, tier EncryptionTier, opts SendOptions) error {
	_, _, err := s.sendWithTier(targetID, text, tier, opts)
	return err
}

// SendChatMessageState sends text end-to-end encrypted and returns the message id and its state:
// StateSent when the peer or a relay took it, StateOffline when it waits for a way to the peer,
// StateKeys when it waits for the peer's keys.
func (s *MessageBroker) SendChatMessageState(targetID types.PeerID, text string) (id, state string, err error) {
	return s.sendWithTier(targetID, text, TierX3DH, SendOptions{})
}

// sendWithTier is the one send path; see SendChatMessageState for what it returns.
func (s *MessageBroker) sendWithTier(targetID types.PeerID, text string, tier EncryptionTier, opts SendOptions) (string, string, error) {
	meta := opts.Metadata
	if meta == nil {
		meta = &MessageMetadata{}
	}
	if opts.AppID != "" && meta.AppID == "" {
		meta.AppID = opts.AppID
	}
	meta.EncTier = tier.String()
	if meta.NumericAppID == 0 {
		meta.NumericAppID = ResolveNumericAppID(meta.AppID)
	}
	encodedMeta := EncodeMetadata(meta)
	if opts.NoTrack {
		opts.noTrack = true
	}

	innerMsg := &PersonalMessageProto{
		Id:          uuid.New().String(),
		SenderId:    s.myIDHexed,
		Type:        opts.Type,
		Text:        text,
		ContentHash: opts.ContentHash,
		Metadata:    encodedMeta,
		Timestamp:   time.Now().UnixNano(),
		ReplyToId:   opts.ReplyToID,
		ConvEvent:   opts.ConvEvent,
		ConvSync:    opts.ConvSync,
	}
	if opts.ID != "" {
		innerMsg.Id = opts.ID
	}

	innerBytes, err := proto.Marshal(innerMsg)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal inner msg: %w", err)
	}

	var msg *internal_pb.MessageData

	switch tier {
	case TierX3DH:
		var encErr error
		msg, encErr = s.encryptX3DH(targetID, innerBytes)
		if encErr != nil {
			if opts.noTrack {
				return "", "", encErr // receipts are not worth keeping
			}
			// no keys for this peer yet (never met, not connected): keep the message and encrypt it
			// when the peer is reachable
			if err := s.trackIntent(targetID, innerMsg.Id, innerBytes); err != nil {
				return "", "", fmt.Errorf("could not encrypt or queue the message: %w", err)
			}
			// the keys are in the DHT, when it answers; meanwhile ask the peer for them through
			// whatever carries messages (a relay keeps the ask while the peer is away)
			s.sendKnock(targetID, false)
			s.wantConnection(targetID)
			if opts.onQueued != nil {
				opts.onQueued()
			}
			if opts.Silent {
				return innerMsg.Id, StateKeys, nil
			}
			msgDto, err := s.SavePrivateMessage(targetID, innerMsg)
			if err != nil {
				return "", "", err
			}
			s.StartCallbackHandlers(&msgDto)
			return innerMsg.Id, StateKeys, nil
		}

	case TierChaCha20:
		key := opts.SharedKey
		if len(key) != 32 {
			if storedKey, ok := s.GetSharedKey(targetID); ok && len(storedKey) == 32 {
				key = storedKey
			} else {
				return "", "", errors.New("chacha20 requires a 32-byte shared key (none provided or registered)")
			}
		}
		ciphertext, err := encrypt_decrypt.EncryptChaCha(key, innerBytes)
		if err != nil {
			return "", "", fmt.Errorf("chacha20 encryption failed: %w", err)
		}
		// Magic prefix 0x01: ChaCha20 ciphertext
		payload := append([]byte{0x01}, ciphertext...)
		msg = &internal_pb.MessageData{
			MessageId: types.NewMessageID(),
			OriginId:  s.sw.GetSelfID().ToBytes(),
			TargetId:  targetID[:],
			Timestamp: uint64(time.Now().Unix()),
			HopLimit:  types.HOPLIMIT,
			Payload: &internal_pb.MessageData_ChatMessage{
				ChatMessage: &internal_pb.ChatMessage{
					EncryptedPayload: payload,
				},
			},
		}

	case TierPlain:
		// Magic prefix 0x00: Plaintext payload
		payload := append([]byte{0x00}, innerBytes...)
		msg = &internal_pb.MessageData{
			MessageId: types.NewMessageID(),
			OriginId:  s.sw.GetSelfID().ToBytes(),
			TargetId:  targetID[:],
			Timestamp: uint64(time.Now().Unix()),
			HopLimit:  types.HOPLIMIT,
			Payload: &internal_pb.MessageData_ChatMessage{
				ChatMessage: &internal_pb.ChatMessage{
					EncryptedPayload: payload,
				},
			},
		}

	default:
		return "", "", fmt.Errorf("unsupported encryption tier: %v", tier)
	}

	var delivered, direct bool
	if opts.DirectOnly {
		delivered = s.sw.SendDataForPeer(targetID, net_types.TypeChatMessage, msg) == nil
		direct = delivered
	} else {
		delivered, direct = s.deliverVia(targetID, msg)
	}
	if !opts.noTrack {
		// keep it until the recipient confirms (a retry follows if it never does)
		if err := s.trackVia(targetID, innerMsg.Id, msg, innerBytes, delivered, delivered && !direct); err != nil && !delivered {
			return "", "", fmt.Errorf("could not send or queue the message: %w", err)
		}
	}
	state := StateSent
	if delivered && !direct {
		state = StateRelayed
	}
	if !delivered {
		state = StateOffline
		if !s.HasRelay() {
			state = StateNoRoute
		}
		s.wantConnection(targetID)
		if opts.onQueued != nil {
			opts.onQueued()
		}
	}

	if opts.Silent {
		return innerMsg.Id, state, nil
	}
	msgDto, err := s.SavePrivateMessage(targetID, innerMsg)
	if err != nil {
		return "", "", err
	}

	s.StartCallbackHandlers(&msgDto)
	return innerMsg.Id, state, nil
}

// deliverVia hands msg to the peer directly or through a relay and reports whether somebody took it
// and whether that was the peer itself (direct) or a relay.
func (s *MessageBroker) deliverVia(targetID types.PeerID, msg *internal_pb.MessageData) (ok, direct bool) {
	if err := s.sw.SendDataForPeer(targetID, net_types.TypeChatMessage, msg); err == nil {
		s.log.Info("MessageBroker: message sent to the peer directly", "peer", targetID.String()[:8])
		return true, true
	}
	return s.sendViaRelay(targetID, msg) == nil, false
}

// HasRelay reports whether one of the peers we are connected to carries messages for others (a
// relay, or one we were given as ours): without one, a peer we cannot reach directly gets nothing.
func (s *MessageBroker) HasRelay() bool {
	for _, p := range s.sw.GetAllPeers() {
		if p.Relays() || (s.useAsRelay != nil && s.useAsRelay(p.ID())) {
			return true
		}
	}
	return false
}

func (s *MessageBroker) sendViaRelay(targetID types.PeerID, msg *internal_pb.MessageData) error {
	env, err := s.sw.SignMessageData(msg)
	if err != nil {
		return err
	}
	envBytes, err := proto.Marshal(env)
	if err != nil {
		return err
	}

	relayMsg := &internal_pb.MessageData{
		MessageId: types.NewMessageID(),
		OriginId:  s.sw.GetSelfID().ToBytes(),
		TargetId:  targetID[:],
		Timestamp: uint64(time.Now().UnixNano()),
		Payload: &internal_pb.MessageData_RelayReq{
			RelayReq: &internal_pb.RelayRequest{
				TargetPeerId:      targetID[:],
				ForwardedEnvelope: envBytes,
			},
		},
	}

	relayEnv, err := s.sw.SignMessageData(relayMsg)
	if err != nil {
		return err
	}

	allPeers := s.sw.GetAllPeers()
	if len(allPeers) == 0 {
		return errors.New("no active peers to relay message")
	}

	// the relays the recipient named for its mail come first: they keep it for sure; then any
	// other relay we are connected to
	s.mu.RLock()
	mailboxesOf := s.mailboxesOf
	s.mu.RUnlock()
	var mailboxes map[types.PeerID]bool
	if mailboxesOf != nil {
		var first []*swarm.Peer
		mailboxes = map[types.PeerID]bool{}
		for _, id := range mailboxesOf(targetID) {
			if p := s.sw.GetPeer(id); p != nil && !mailboxes[id] {
				mailboxes[id] = true
				first = append(first, p)
			}
		}
		if len(first) > 0 {
			rest := make([]*swarm.Peer, 0, len(allPeers))
			for _, p := range allPeers {
				if !mailboxes[p.ID()] {
					rest = append(rest, p)
				}
			}
			allPeers = append(first, rest...)
		}
	}

	// A relay that answered some message with "mailbox full" goes last, but is still tried: the box
	// that was full may be another recipient's, and a relay forwards to a recipient connected to it
	// without keeping anything. Skipping it left messages to a peer online on that very relay
	// waiting ten minutes because a box of someone offline had filled up.
	var full []*swarm.Peer
	try := make([]*swarm.Peer, 0, len(allPeers))
	for _, p := range allPeers {
		if !p.Relays() && !(s.useAsRelay != nil && s.useAsRelay(p.ID())) && !mailboxes[p.ID()] {
			continue // not a relay (a signaling-only node, or a private relay we were not given)
		}
		if s.relayFull != nil && s.relayFull(p.ID()) {
			full = append(full, p)
			continue
		}
		try = append(try, p)
	}
	for _, p := range append(try, full...) {
		if err := p.Send(net_types.TypeRelayMessageRequest, relayEnv); err == nil {
			s.log.Info("MessageBroker: message handed to a relay", "peer", targetID.String()[:8], "relay", p.ID().String()[:8])
			return nil
		}
	}
	return errors.New("failed to send relay request to any connected peer")
}

func (s *MessageBroker) Handle(msg *internal_pb.MessageData, peerID types.PeerID) {
	var plaintext []byte
	var err error
	authenticated := false // only the end-to-end encrypted tiers prove who the sender is
	originID, err := types.ToPeerID(msg.OriginId)
	if err != nil {
		return
	}

	switch payload := msg.Payload.(type) {
	case *internal_pb.MessageData_SecureMessage:
		authenticated = true
		secureMsg := payload.SecureMessage
		plaintext, err = s.crypto.HandleIncomingMessage(originID, types.EncryptedMessage{
			Header:     types.ToRatchetHeader(secureMsg.Header.RatchetKey, secureMsg.Header.Pn, secureMsg.Header.N),
			Ciphertext: secureMsg.Ciphertext,
		})
	case *internal_pb.MessageData_X3DhInit:
		authenticated = true
		hasSession, _ := s.crypto.HasSession(originID)

		initPacket := types.ToX3DHInitPacket(payload.X3DhInit.EphemeralKey, payload.X3DhInit.IdentityKey, payload.X3DhInit.OneTimeKeyId, types.EncryptedMessage{
			Header:     types.ToRatchetHeader(payload.X3DhInit.InitialCiphertext.Header.RatchetKey, payload.X3DhInit.InitialCiphertext.Header.Pn, payload.X3DhInit.InitialCiphertext.Header.N),
			Ciphertext: payload.X3DhInit.InitialCiphertext.Ciphertext,
		})

		// both sides started a session at once: the one of the larger ID stays
		myID, _ := s.identity.GetPeerID()
		keepOurs := hasSession && string(myID[:]) > string(originID[:])
		// the sender's identity comes with the packet; a forged or foreign card does not verify
		var card *keymanager.PublicBundle
		if raw := payload.X3DhInit.GetSenderBundle(); len(raw) > 0 {
			if b, cerr := s.sw.BundleFromRecord(originID, raw); cerr == nil {
				card = b
			}
		}
		plaintext, err = s.crypto.HandleInitPacket(originID, initPacket, keepOurs, card)
		if err != nil && !hasSession && !errors.Is(err, crypto.ErrInitReplay) {
			// the session the sender started cannot be opened here (a prekey it used is gone):
			// it has to start again from a fresh bundle
			s.sessionBroken(originID)
		}
	case *internal_pb.MessageData_ChatMessage:
		cm := payload.ChatMessage
		if cm == nil || len(cm.EncryptedPayload) == 0 {
			s.log.Debug("Received empty ChatMessage payload", "peer", peerID)
			return
		}
		magic := cm.EncryptedPayload[0]
		raw := cm.EncryptedPayload[1:]
		switch magic {
		case 0x00: // Plain tier
			plaintext = raw
		case 0x01: // ChaCha20 tier
			key, found := s.GetSharedKey(originID)
			if !found {
				s.log.Debug("Received ChaCha20 message without known shared key", "origin", originID)
				return
			}
			plaintext, err = encrypt_decrypt.DecryptChaCha(key, raw)
		default:
			s.log.Debug("Unknown ChatMessage magic tier", "magic", magic)
			return
		}
	}

	if plaintext == nil || err != nil {
		if sm, isSecure := msg.Payload.(*internal_pb.MessageData_SecureMessage); isSecure {
			// a repeat of a message we read (its receipt got lost): confirm it again, the
			// session is fine
			if errors.Is(err, x3dh.ErrOldMessage) {
				if id, ok := s.recentMessages().get(originID, sm.SecureMessage.GetHeader()); ok {
					go s.sendAck(originID, id)
					return
				}
			}
			s.log.Debug("Decryption failed or plaintext is nil", "peer", peerID, "err", err)
			s.sessionBroken(originID)
			return
		}
		s.log.Debug("Decryption failed or plaintext is nil", "peer", peerID, "err", err)
		return
	}

	innerMsg := &PersonalMessageProto{}
	if err = proto.Unmarshal(plaintext, innerMsg); err != nil {
		s.log.Debug("Failed to unmarshal inner PersonalMessage", "err", err)
		return
	}
	// the sender named inside is only a claim; who it is comes from the signed origin
	if innerMsg.SenderId != hex.EncodeToString(originID[:]) {
		s.log.Debug("MessageBroker: message names another sender than the one that sent it", "peer", originID.String()[:8])
		return
	}

	// the peer sends its keys (and may ask for ours): the first message between two nodes that
	// never met, when the DHT does not hand out their bundles
	if strings.HasPrefix(innerMsg.Text, knockPrefix) {
		s.handleKnock(originID, innerMsg.Text)
		return
	}
	// the peer cannot read us: its session with us is gone or never matched ours
	if strings.HasPrefix(innerMsg.Text, resetPrefix) {
		s.sessionReset(originID)
		return
	}
	// a receipt for something we sent
	if authenticated && strings.HasPrefix(innerMsg.Text, ackPrefix) {
		s.confirm(originID, strings.TrimPrefix(innerMsg.Text, ackPrefix))
		return
	}
	if sm, ok := msg.Payload.(*internal_pb.MessageData_SecureMessage); ok && innerMsg.Id != "" {
		s.recentMessages().put(originID, sm.SecureMessage.GetHeader(), innerMsg.Id)
	}
	// every other message is confirmed, also repeats of one that was already handled
	if innerMsg.Id != "" {
		go s.sendAck(originID, innerMsg.Id)
		if !s.firstSight(innerMsg.Id) {
			return
		}
	}

	if authenticated {
		s.mu.RLock()
		hook := s.interceptor
		s.mu.RUnlock()
		if hook != nil && hook(originID, innerMsg.Text) {
			return
		}
	}

	verdict := Accept
	s.mu.RLock()
	filter := s.filter
	sink := s.eventSink
	s.mu.RUnlock()
	if filter != nil {
		verdict = filter(originID)
	}
	if verdict == Drop {
		return // already confirmed above: the sender sees "delivered" and nothing else
	}

	// conversation-log traffic goes to the log; a message among it still reaches the contacts and
	// the listeners below, but the log keeps it rather than the plain history
	store := true
	if len(innerMsg.ConvEvent) > 0 || len(innerMsg.ConvSync) > 0 {
		if sink == nil {
			// the log is not up yet: a message still has its text, anything else is dropped (sync
			// brings it again)
			if innerMsg.Text == "" {
				return
			}
		} else {
			if !authenticated || !sink(originID, innerMsg, verdict == AsRequest) {
				return
			}
			store = false
		}
	}

	myID, _ := s.identity.GetPeerID()
	myIDHex := hex.EncodeToString(myID[:])
	senderIDHex := innerMsg.SenderId

	chatID := GenerateLocalChatID(myIDHex, senderIDHex, false)

	meta := DecodeMetadata(innerMsg.Metadata)
	appID := meta.NumericAppID
	if appID == 0 {
		appID = ResolveNumericAppID(meta.AppID)
	}

	if store {
		if err = s.keeper.Setter.SaveMessage(appID, chatID, innerMsg); err != nil {
			s.log.Error("Failed to save message to DB", "err", err, "app_id", appID)
		}
	}

	receivedDTO := ProtoToDTO(innerMsg, myIDHex, myIDHex)
	receivedDTO.TargetPubKey = hex.EncodeToString(msg.OriginId)

	s.log.Debug("Message received!",
		"from", receivedDTO.SenderID,
		"type", receivedDTO.Type,
		"app_id", receivedDTO.AppID,
	)

	if !s.contacts.Exists(senderIDHex) {
		// the key is needed to answer; a connected peer has shown it in the handshake
		pubKeyHex := ""
		if peer := s.sw.GetPeer(originID); peer != nil {
			pk := peer.PubKey()
			pubKeyHex = hex.EncodeToString(pk[:])
		}
		if verdict == AsRequest {
			// a stranger stays a stranger until the user accepts: no calls, no files, not one of ours
			_ = s.contacts.NoteRequest(senderIDHex, pubKeyHex)
			receivedDTO.Request = true
		} else {
			s.contacts.NewContact(dto.ContactDTO{
				PeerID:   senderIDHex,
				PubKey:   pubKeyHex,
				Alias:    "Unknown User",
				IsOnline: true,
				LastSeen: time.Now().Unix(),
			})
		}
	}

	s.StartCallbackHandlers(&receivedDTO)
}

func (s *MessageBroker) GetSubscribedTypes() []interface{} {
	return []interface{}{
		(*internal_pb.MessageData_SecureMessage)(nil),
		(*internal_pb.MessageData_X3DhInit)(nil),
		(*internal_pb.MessageData_ChatMessage)(nil),
	}
}

// encryptX3DH encrypts innerBytes for target with the double ratchet session (starting one if needed).
func (s *MessageBroker) encryptX3DH(targetID types.PeerID, innerBytes []byte) (*internal_pb.MessageData, error) {
	encryptedData, initPacket, err := s.crypto.EncryptMessage(targetID, innerBytes)
	if err != nil {
		return nil, err
	}
	return s.toMessageData(targetID, encryptedData, initPacket)
}

func (s *MessageBroker) toMessageData(targetID types.PeerID, encryptedData types.EncryptedMessage, initPacket *types.X3DHInit) (*internal_pb.MessageData, error) {
	var msg *internal_pb.MessageData
	id := uuid.New()
	ownPeerID, err := s.identity.GetPeerID()
	if err != nil {
		return nil, err
	}

	if initPacket != nil {
		card, err := s.sw.IdentityCard()
		if err != nil {
			return nil, err
		}
		msg = &internal_pb.MessageData{
			MessageId: id[:],
			OriginId:  ownPeerID[:],
			TargetId:  targetID[:],
			Timestamp: uint64(time.Now().Unix()),
			HopLimit:  types.HOPLIMIT,
			Payload: &internal_pb.MessageData_X3DhInit{
				X3DhInit: &internal_pb.X3DHInit{
					EphemeralKey: initPacket.EphemeralKey[:],
					IdentityKey:  initPacket.IdentityKey[:],
					OneTimeKeyId: opkBytes(initPacket.OneTimeKeyID),
					SenderBundle: card,
					InitialCiphertext: &internal_pb.SecureMessage{
						Header: &internal_pb.RatchetHeader{
							RatchetKey: encryptedData.Header.DH[:],
							Pn:         encryptedData.Header.PN,
							N:          encryptedData.Header.N,
						},
						Ciphertext: encryptedData.Ciphertext,
					},
				},
			},
		}
		return msg, nil
	}

	msg = &internal_pb.MessageData{
		MessageId: id[:],
		OriginId:  ownPeerID[:],
		TargetId:  targetID[:],
		Timestamp: uint64(time.Now().Unix()),
		HopLimit:  types.HOPLIMIT,
		Payload: &internal_pb.MessageData_SecureMessage{
			SecureMessage: &internal_pb.SecureMessage{
				Header: &internal_pb.RatchetHeader{
					RatchetKey: encryptedData.Header.DH[:],
					Pn:         encryptedData.Header.PN,
					N:          encryptedData.Header.N,
				},
				Ciphertext: encryptedData.Ciphertext,
			},
		},
	}
	return msg, nil
}

// SetConnector gives the broker a way to connect to a peer by its ID (through the DHT, hole punching
// or a relay). It is asked when a message cannot be delivered or encrypted because the peer is not
// connected: its keys and its answers come with a connection, and nobody else would open one.
func (s *MessageBroker) SetConnector(f func(types.PeerID)) {
	s.mu.Lock()
	s.connect = f
	s.mu.Unlock()
}

// SetRelaySelector names peers to route messages through even when they do not advertise as a
// relay — a private relay the node was configured to use.
func (s *MessageBroker) SetRelaySelector(f func(types.PeerID) bool) {
	s.useAsRelay = f
}

// SetRelayFullCheck names relays that refused to keep our messages lately; they are tried last then.
func (s *MessageBroker) SetRelayFullCheck(f func(types.PeerID) bool) {
	s.relayFull = f
}

const connectPause = 20 * time.Second

// wantConnection asks for a connection to target, at most once in a while per peer.
func (s *MessageBroker) wantConnection(target types.PeerID) {
	s.mu.RLock()
	connect := s.connect
	s.mu.RUnlock()
	if connect == nil || s.sw.GetPeer(target) != nil {
		return
	}
	s.connMu.Lock()
	if s.lastConnect == nil || len(s.lastConnect) > 4096 {
		s.lastConnect = make(map[types.PeerID]time.Time)
	}
	if last, ok := s.lastConnect[target]; ok && time.Since(last) < connectPause {
		s.connMu.Unlock()
		return
	}
	s.lastConnect[target] = time.Now()
	s.connMu.Unlock()
	go connect(target)
}

const (
	resetPrefix = "fogreset1:"
	resetPause  = 30 * time.Second
)

// sessionBroken: a message from origin could not be decrypted, so our session with it does not
// match theirs (both started one at the same moment, one side lost its state, ...). We drop ours
// and tell origin to drop theirs; the next message of either side starts a new one, and what was
// waiting for confirmation is encrypted again.
func (s *MessageBroker) sessionBroken(origin types.PeerID) {
	s.connMu.Lock()
	if s.lastReset == nil || len(s.lastReset) > 4096 {
		s.lastReset = make(map[types.PeerID]time.Time)
	}
	if last, ok := s.lastReset[origin]; ok && time.Since(last) < resetPause {
		s.connMu.Unlock()
		return
	}
	s.lastReset[origin] = time.Now()
	s.connMu.Unlock()

	s.log.Warn("MessageBroker: a message could not be decrypted, resetting the session", "peer", origin.String()[:8])
	_ = s.crypto.DeleteSession(origin)
	s.sw.ForgetBundle(origin) // the next session starts from a fresh bundle
	s.forgetCiphertexts(origin)
	go func() {
		_ = s.SendChatMessageWithTier(origin, resetPrefix, TierPlain, SendOptions{Silent: true, noTrack: true})
	}()
}

// sessionReset: origin tells us that it could not read us. We drop our session with it and encrypt
// again what it did not confirm.
func (s *MessageBroker) sessionReset(origin types.PeerID) {
	s.log.Warn("MessageBroker: the peer reset the session", "peer", origin.String()[:8])
	_ = s.crypto.DeleteSession(origin)
	s.sw.ForgetBundle(origin) // the next session starts from a fresh bundle
	s.forgetCiphertexts(origin)
	go s.FlushOutbox(origin)
}

// ResetSession forgets the encrypted session with a peer; the next message starts a new one.
func (s *MessageBroker) ResetSession(peer types.PeerID) error {
	if err := s.crypto.DeleteSession(peer); err != nil {
		return err
	}
	s.forgetCiphertexts(peer)
	return nil
}

// SetInterceptor installs a hook that sees the text of every authenticated incoming message before
// it is stored. When it returns true the message is protocol traffic and is not stored or shown.
// origin is the peer that the end-to-end encryption session belongs to, not a claim in the message.
// Verdict is what happens to a direct message from a peer.
type Verdict int

const (
	// Accept: a contact, the message goes into its chat.
	Accept Verdict = iota
	// AsRequest: a stranger, the message is kept as a message request and the sender is not
	// made a contact.
	AsRequest
	// Drop: blocked, or strangers may not write: the message is confirmed (so the sender does not
	// retry and learns nothing) and thrown away.
	Drop
)

// SetFilter decides, for every direct message, whether it is kept and how. Group traffic, which
// the interceptor takes first, is not filtered here.
func (s *MessageBroker) SetFilter(f func(origin types.PeerID) Verdict) {
	s.mu.Lock()
	s.filter = f
	s.mu.Unlock()
}

// MessageState says what became of a message of ours (see the State constants).
type MessageState struct {
	Target types.PeerID
	ID     string
	State  string
}

// OnMessageState calls cb whenever a message of ours is handed on, confirmed or given up.
func (s *MessageBroker) OnMessageState(cb func(MessageState)) {
	s.mu.Lock()
	s.stateCallbacks = append(s.stateCallbacks, cb)
	s.mu.Unlock()
}

// noteState tells the callbacks, off the caller's goroutine but in order: a message confirmed a
// moment after it went out must not look as if it went out after it was confirmed.
func (s *MessageBroker) noteState(target types.PeerID, id, state string) {
	s.stateMu.Lock()
	s.stateQ = append(s.stateQ, MessageState{Target: target, ID: id, State: state})
	start := !s.stateRunning
	s.stateRunning = true
	s.stateMu.Unlock()
	if start {
		go s.runStateCallbacks()
	}
}

// runStateCallbacks tells the callbacks of the queued states and ends when none are left.
func (s *MessageBroker) runStateCallbacks() {
	for {
		s.stateMu.Lock()
		if len(s.stateQ) == 0 {
			s.stateRunning = false
			s.stateMu.Unlock()
			return
		}
		ev := s.stateQ[0]
		s.stateQ = s.stateQ[1:]
		s.stateMu.Unlock()
		s.mu.RLock()
		cbs := append(([]func(MessageState))(nil), s.stateCallbacks...)
		s.mu.RUnlock()
		for _, cb := range cbs {
			cb(ev)
		}
	}
}

func (s *MessageBroker) SetInterceptor(h func(origin types.PeerID, text string) bool) {
	s.mu.Lock()
	s.interceptor = h
	s.mu.Unlock()
}

// SetMailboxResolver sets how to find the relays a recipient named for its mail (they are tried
// first when the recipient cannot be reached directly).
func (s *MessageBroker) SetMailboxResolver(f func(types.PeerID) []types.PeerID) {
	s.mu.Lock()
	s.mailboxesOf = f
	s.mu.Unlock()
}

// EventSink takes the conversation-log traffic of a message (msg.ConvEvent / msg.ConvSync) from an
// authenticated peer the privacy filter lets through; request says the peer is a stranger. It
// reports whether the message was a chat message (as opposed to an operation or sync traffic):
// those still reach the contacts and the message listeners, but are not stored as plain messages.
type EventSink func(origin types.PeerID, msg *PersonalMessageProto, request bool) (isMessage bool)

// SetEventSink sets where conversation-log traffic goes.
func (s *MessageBroker) SetEventSink(f EventSink) {
	s.mu.Lock()
	s.eventSink = f
	s.mu.Unlock()
}

// SendEvent sends conversation-log traffic to a peer; see SendOptions for ID, ConvEvent, ConvSync,
// NoTrack and DirectOnly. text, typ and metadata are what an older node shows for a message.
func (s *MessageBroker) SendEvent(targetID types.PeerID, text string, opts SendOptions) (state string, err error) {
	opts.Silent = true // the log keeps it, not the plain history
	_, state, err = s.sendWithTier(targetID, text, TierX3DH, opts)
	return state, err
}

// SendSilent sends protocol traffic that is not stored in the chat history. queued says the message
// could not be delivered yet and waits in the outbox.
func (s *MessageBroker) SendSilent(targetID types.PeerID, text string) (queued bool, err error) {
	err = s.SendChatMessageWithTier(targetID, text, TierX3DH, SendOptions{Silent: true, onQueued: func() { queued = true }})
	return queued, err
}

func (s *MessageBroker) AddHandleCallback(cb func(dto *dto.MessageDTO)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handleCallbacks = append(s.handleCallbacks, cb)
}

func (s *MessageBroker) SavePrivateMessage(targetID types.PeerID, innerMsg *PersonalMessageProto) (dto.MessageDTO, error) {
	targetIDHex := hex.EncodeToString(targetID[:])
	chatID := GenerateLocalChatID(s.myIDHexed, targetIDHex, false)

	meta := DecodeMetadata(innerMsg.Metadata)
	appID := meta.NumericAppID
	if appID == 0 {
		appID = ResolveNumericAppID(meta.AppID)
	}

	err := s.keeper.Setter.SaveMessage(appID, chatID, innerMsg)
	if err != nil {
		return dto.MessageDTO{}, err
	}

	return ProtoToDTO(innerMsg, s.myIDHexed, targetIDHex), nil
}

func (s *MessageBroker) StartCallbackHandlers(msgDto *dto.MessageDTO) {
	if s.handleCallbacks != nil {
		s.mu.RLock()
		for _, cb := range s.handleCallbacks {
			go cb(msgDto)
		}
		s.mu.RUnlock()
	}
}

func (s *MessageBroker) GetKeeper() *MessageKeeper {
	return s.keeper
}

func opkBytes(k *[32]byte) []byte {
	if k == nil {
		return nil
	}
	return k[:]
}

func (s *MessageBroker) recentMessages() *recentMessages {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.recent == nil {
		s.recent = newRecentMessages(4096)
	}
	return s.recent
}
