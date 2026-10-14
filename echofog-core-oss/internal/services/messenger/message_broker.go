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
	connect         func(types.PeerID) // asked to open a connection to a peer that cannot be reached
	connMu          sync.Mutex
	lastConnect     map[types.PeerID]time.Time
	lastReset       map[types.PeerID]time.Time
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

	innerMsg := &PersonalMessageProto{
		Id:          uuid.New().String(),
		SenderId:    s.myIDHexed,
		Type:        opts.Type,
		Text:        text,
		ContentHash: opts.ContentHash,
		Metadata:    encodedMeta,
		Timestamp:   time.Now().UnixNano(),
		ReplyToId:   opts.ReplyToID,
	}

	innerBytes, err := proto.Marshal(innerMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal inner msg: %w", err)
	}

	var msg *internal_pb.MessageData

	switch tier {
	case TierX3DH:
		var encErr error
		msg, encErr = s.encryptX3DH(targetID, innerBytes)
		if encErr != nil {
			if opts.noTrack {
				return encErr // receipts are not worth keeping
			}
			// no keys for this peer yet (never met, not connected): keep the message and encrypt it
			// when the peer is reachable
			if err := s.trackIntent(targetID, innerMsg.Id, innerBytes); err != nil {
				return fmt.Errorf("could not encrypt or queue the message: %w", err)
			}
			s.wantConnection(targetID)
			if opts.onQueued != nil {
				opts.onQueued()
			}
			if opts.Silent {
				return nil
			}
			msgDto, err := s.SavePrivateMessage(targetID, innerMsg)
			if err != nil {
				return err
			}
			s.StartCallbackHandlers(&msgDto)
			return nil
		}

	case TierChaCha20:
		key := opts.SharedKey
		if len(key) != 32 {
			if storedKey, ok := s.GetSharedKey(targetID); ok && len(storedKey) == 32 {
				key = storedKey
			} else {
				return errors.New("chacha20 requires a 32-byte shared key (none provided or registered)")
			}
		}
		ciphertext, err := encrypt_decrypt.EncryptChaCha(key, innerBytes)
		if err != nil {
			return fmt.Errorf("chacha20 encryption failed: %w", err)
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
		return fmt.Errorf("unsupported encryption tier: %v", tier)
	}

	delivered := s.deliver(targetID, msg)
	if !opts.noTrack {
		// keep it until the recipient confirms (a retry follows if it never does)
		if err := s.track(targetID, innerMsg.Id, msg, innerBytes, delivered); err != nil && !delivered {
			return fmt.Errorf("could not send or queue the message: %w", err)
		}
	}
	if !delivered {
		s.wantConnection(targetID)
		if opts.onQueued != nil {
			opts.onQueued()
		}
	}

	if opts.Silent {
		return nil
	}
	msgDto, err := s.SavePrivateMessage(targetID, innerMsg)
	if err != nil {
		return err
	}

	s.StartCallbackHandlers(&msgDto)
	return nil
}

// deliver hands msg to the peer directly or through a relay and reports whether somebody took it.
func (s *MessageBroker) deliver(targetID types.PeerID, msg *internal_pb.MessageData) bool {
	if err := s.sw.SendDataForPeer(targetID, net_types.TypeChatMessage, msg); err == nil {
		return true
	}
	return s.sendViaRelay(targetID, msg) == nil
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

	for _, p := range allPeers {
		if !p.Relays() {
			continue // a signaling-only node passes on no messages
		}
		if err := p.Send(net_types.TypeRelayMessageRequest, relayEnv); err == nil {
			s.log.Info("MessageBroker: message routed via relay peer", "target", targetID.String()[:8])
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
			s.log.Warn("Received empty ChatMessage payload", "peer", peerID)
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
				s.log.Error("Received ChaCha20 message without known shared key", "origin", originID)
				return
			}
			plaintext, err = encrypt_decrypt.DecryptChaCha(key, raw)
		default:
			s.log.Warn("Unknown ChatMessage magic tier", "magic", magic)
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
			s.log.Error("Decryption failed or plaintext is nil", "peer", peerID, "err", err)
			s.sessionBroken(originID)
			return
		}
		s.log.Error("Decryption failed or plaintext is nil", "peer", peerID, "err", err)
		return
	}

	innerMsg := &PersonalMessageProto{}
	if err = proto.Unmarshal(plaintext, innerMsg); err != nil {
		s.log.Error("Failed to unmarshal inner PersonalMessage", "err", err)
		return
	}
	// the sender named inside is only a claim; who it is comes from the signed origin
	if innerMsg.SenderId != hex.EncodeToString(originID[:]) {
		s.log.Warn("MessageBroker: message names another sender than the one that sent it", "peer", originID.String()[:8])
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

	myID, _ := s.identity.GetPeerID()
	myIDHex := hex.EncodeToString(myID[:])
	senderIDHex := innerMsg.SenderId

	chatID := GenerateLocalChatID(myIDHex, senderIDHex, false)

	meta := DecodeMetadata(innerMsg.Metadata)
	appID := meta.NumericAppID
	if appID == 0 {
		appID = ResolveNumericAppID(meta.AppID)
	}

	err = s.keeper.Setter.SaveMessage(appID, chatID, innerMsg)
	if err != nil {
		s.log.Error("Failed to save message to DB", "err", err, "app_id", appID)
	}

	receivedDTO := ProtoToDTO(innerMsg, myIDHex, myIDHex)
	receivedDTO.TargetPubKey = hex.EncodeToString(msg.OriginId)

	s.log.Info("Message received!",
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
		s.contacts.NewContact(dto.ContactDTO{
			PeerID:   senderIDHex,
			PubKey:   pubKeyHex,
			Alias:    "Unknown User",
			IsOnline: true,
			LastSeen: time.Now().Unix(),
		})
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
func (s *MessageBroker) SetInterceptor(h func(origin types.PeerID, text string) bool) {
	s.mu.Lock()
	s.interceptor = h
	s.mu.Unlock()
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
