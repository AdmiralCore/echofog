package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/echofog/echofog-core/internal/crypto/keymanager"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/network"

	"github.com/echofog/echofog-core/internal/services/call"
	"github.com/echofog/echofog-core/internal/services/call/signaling"
	"github.com/echofog/echofog-core/internal/services/call/tree"
	"github.com/echofog/echofog-core/internal/services/messenger"
	"github.com/echofog/echofog-core/pkg/api/types"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/echofog/echofog-core/pkg/events"
	"github.com/echofog/echofog-core/pkg/node"
)

type NodeApi struct {
	node *node.Node

	uploadKbps int // declared upstream bandwidth used to size call streams

	vpn   vpnState
	tun   tunState
	pay   payState    // client: paying the connected exit
	earn  earnState   // operator: booking incoming payments
	vault vaultClient // client: the TON vault and the vouchers signed from it

	groupsH groupsHolder
	convsH  convsHolder

	geoOnce sync.Once
	geoLoc  *geoLocator // where exits are, looked up online

	relayChosen bool // the user picked the relay settings; false: the defaults are in effect
	startPort   int  // the listen port the node was configured with when it started

	exitFreePeers []string // peers the exit serves for free (persisted; guarded by settingsMu)
	noAutoSwitch  bool     // the proxy keeps its exit even when it fails (persisted; guarded by settingsMu)

	bootstrapBuiltIn []string // bootstrap nodes of this release (guarded by settingsMu)
	bootstrapCustom  []string // bootstrap nodes the user added (persisted; guarded by settingsMu)
	nextAccessMode   string   // access mode saved for the next start (guarded by settingsMu)

	Logger *slog.Logger
}

func NewNodeApi(node *node.Node) *NodeApi {
	builtIn := node.BootstrapNodes()
	saved, chosen := loadPersistedSettings(node.Cfg)
	if saved.Privacy != nil {
		_ = node.SetPrivacy(*saved.Privacy) // an invalid hand edit keeps the defaults
	}
	node.SetBootstrapNodes(bootstrapList(builtIn, saved.BootstrapNodes))
	api := &NodeApi{node: node, Logger: node.Logger, relayChosen: chosen, startPort: relaySettingsFromConfig(node.Cfg).ListenPort,
		exitFreePeers: saved.ExitFreePeers, noAutoSwitch: saved.VPNNoAutoSwitch, bootstrapBuiltIn: builtIn, bootstrapCustom: saved.BootstrapNodes,
		nextAccessMode: saved.AccessMode}
	// the conversation logs take chat traffic from the very first message on
	node.OnMessengerReady(func() { _, _ = api.convsFacade() })
	return api
}

func (n *NodeApi) IsRegistered() bool {
	return n.node.IsRegistered()
}

func (n *NodeApi) Register(appPassword string, mnemonicPassphrase string) (string, error) {
	return n.node.Register([]byte(appPassword), []byte(mnemonicPassphrase))
}

func (n *NodeApi) Login(appPassword string) error {
	return n.node.Login([]byte(appPassword))
}

// VerifyPassword reports nil when appPassword unlocks the running node's identity. It gates
// sensitive actions without a second database handle.
func (n *NodeApi) VerifyPassword(appPassword string) error {
	return n.node.VerifyPassword([]byte(appPassword))
}

func (n *NodeApi) Recover(mnemonic string, mnemonicPassphrase string, newAppPassword string) error {
	// a pasted phrase may differ in case, spaces and line breaks: the same words, the same identity
	phrase, err := keymanager.CheckPhrase(mnemonic)
	if err != nil {
		return err
	}
	return n.node.Recover(phrase, []byte(mnemonicPassphrase), []byte(newAppPassword))
}

func (n *NodeApi) StartNode() error {
	if err := n.node.Start(); err != nil {
		return err
	}
	// domestic connections use the cheaper noise obfs; cross-border keeps REALITY
	n.applyRegionObfs()
	// guard_mode "auto" stands down in a country that does not censor
	go n.runGuardAuto()
	// create the conversation logs and the group service now: they listen for chat traffic from
	// the first moment on
	_, _ = n.convsFacade()
	_, _ = n.groupsFacade()
	return nil
}

func (n *NodeApi) StopNode() {
	if n.node != nil {
		n.node.Stop()
	}
}

func (n *NodeApi) GetPeers() []dto.PeerDTO {
	internalPeers := n.node.Swarm.GetAllPeers()

	result := make([]dto.PeerDTO, 0, len(internalPeers))

	for _, p := range internalPeers {
		id := p.ID()
		idStr := hex.EncodeToString(id[:])

		dtoPeer := dto.PeerDTO{
			PeerID:      idStr,
			ShortID:     idStr[:6],
			Address:     p.Addr(),
			IsConnected: true,
			IsLocal:     false,
		}
		result = append(result, dtoPeer)
	}

	return result

}

func (n *NodeApi) GetMyID() string {
	if n.node.ID == (types.PeerID{}) {
		return ""
	}
	return hex.EncodeToString(n.node.ID[:])
}

func (n *NodeApi) GetMyPubKey() string {
	if n.node.PubKey == (types.PeerPublicKey{}) {
		return ""
	}
	return hex.EncodeToString(n.node.PubKey[:])
}

// ConnectToPeer dials "host:port/<peer id>". The peer ID is needed because the packets to a node
// are obfuscated with a key derived from it; without it only a node running without obfuscation
// can be reached.
func (n *NodeApi) ConnectToPeer(addr string) error {
	ctx, cancel := context.WithTimeout(n.node.Ctx, 15*time.Second)
	defer cancel()
	info := network.ParseAddrInfo(addr)
	host, id, sni, named := info.Addr, info.ID, info.SNI, info.Named
	if named {
		if info.Server && n.node.Swarm != nil {
			n.node.Swarm.SetPeerServer(id, true)
		}
		if sni != "" {
			n.node.Net.ExpectPeerSNI(host, id, sni)
			if n.node.Swarm != nil {
				n.node.Swarm.SetPeerSNI(id, sni)
			}
		} else {
			n.node.Net.ExpectPeer(host, id)
		}
	}
	return n.node.Net.DialEarly(ctx, host)
}

func (n *NodeApi) SendMessage(pubKeyHex string, text string) error {
	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(pubKeyBytes)
	var targetPubKey types.PeerID
	copy(targetPubKey[:], hash[:])

	if n.node.Messenger == nil {
		return errNoChat
	}
	if _, err := n.convsFacade(); err == nil {
		_, err := n.ChatSend(ChatRef{Kind: "dm", ID: hex.EncodeToString(targetPubKey[:])}, SendRequest{Text: text})
		return err
	}
	return n.node.Messenger.SendChatMessage(targetPubKey, text)
}

// errNoChat: a server-role node runs no messenger or contacts.
var errNoChat = errors.New("chat is off on a server-role node")

// MessageStateDTO: a message of ours was handed on ("sent"), confirmed by the recipient
// ("delivered") or given up ("expired").
type MessageStateDTO struct {
	PeerID string `json:"peer_id"`
	ID     string `json:"id"`
	State  string `json:"state"`
}

// OnMessageState calls cb whenever the state of a message of ours changes.
func (n *NodeApi) OnMessageState(cb func(MessageStateDTO)) {
	if n.node == nil || n.node.Messenger == nil {
		return
	}
	n.node.Messenger.OnMessageState(func(ev messenger.MessageState) {
		cb(MessageStateDTO{PeerID: ev.Target.String(), ID: ev.ID, State: ev.State})
	})
}

func (n *NodeApi) MessageHandler() <-chan *dto.MessageDTO {
	readCh := make(chan *dto.MessageDTO, 100)
	if n.node.Messenger == nil {
		return readCh // nothing ever arrives
	}
	n.node.Messenger.AddHandleCallback(func(m *dto.MessageDTO) {
		c := scrubMessage(*m) // a copy: other callbacks read the same message
		select {
		case readCh <- &c:
		default:
		}
	})

	return readCh
}

func (n *NodeApi) GetContacts() ([]dto.ContactDTO, error) {
	if n.node.Contact == nil {
		return nil, errNoChat
	}
	contacts, err := n.node.Contact.GiveMyContacts()
	if err != nil {
		return nil, err
	}
	for i := range contacts {
		contacts[i].IsOnline = n.peerOnline(contacts[i].PeerID)
	}
	return contacts, nil
}

// peerOnline says whether the swarm has a live connection to the peer (hex id).
func (n *NodeApi) peerOnline(peerID string) bool {
	if n.node.Swarm == nil {
		return false
	}
	id, err := types.PeerIDFromString(peerID)
	return err == nil && n.node.Swarm.GetPeer(id) != nil
}

// PeerStateDTO says that the connection to a peer came up or went away.
type PeerStateDTO struct {
	PeerID string `json:"peer_id"`
	Online bool   `json:"online"`
}

// OnPeerState calls cb whenever a connection to a peer comes up or goes away, so a contact's
// online mark follows it without reloading the contacts.
func (n *NodeApi) OnPeerState(cb func(PeerStateDTO)) func() {
	if n.node == nil || n.node.EventBus == nil {
		return func() {}
	}
	return n.node.EventBus.On("network:peer_state_changed", func(ev events.Event) {
		m, ok := ev.Payload.(map[string]any)
		if !ok {
			return
		}
		peer, _ := m["peer"].(string)
		if peer == "" {
			return
		}
		// the bus runs each handler on its own goroutine, so a quick drop and redial can arrive out
		// of order: report what the swarm holds now rather than what the event said
		cb(PeerStateDTO{PeerID: peer, Online: n.peerOnline(peer)})
	})
}

func (n *NodeApi) NewContact(contact dto.ContactDTO) error {
	if n.node.Contact == nil {
		return errNoChat
	}
	return n.node.Contact.NewContact(contact)
}

func (n *NodeApi) DeleteContact(peerID string) error {
	if n.node.Contact == nil {
		return errNoChat
	}
	return n.node.Contact.RemoveContact(peerID)
}

// BanContact blocks a peer (see BlockPeer); it stays in the contacts, so unblocking restores it.
func (n *NodeApi) BanContact(peerID string) error {
	return n.BlockPeer(peerID)
}

func (n *NodeApi) GetMessages(contactID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error) {
	if n.node.Messenger == nil {
		return nil, errNoChat
	}
	if _, err := n.convsFacade(); err == nil {
		return n.plainHistory(contactID, limit)
	}
	myID := n.GetMyID()

	chatID := messenger.GenerateLocalChatID(myID, contactID, false)

	msgs, err := n.node.Messenger.GetKeeper().Getter.GetMessages(
		0,
		chatID,
		limit,
		beforeTimestamp,
		myID,
		contactID,
	)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		msgs[i] = scrubMessage(msgs[i])
	}
	if raw, derr := hex.DecodeString(contactID); derr == nil && len(raw) == len(types.PeerID{}) {
		var peer types.PeerID
		copy(peer[:], raw)
		if pending := n.node.Messenger.PendingStates(peer); len(pending) > 0 {
			for i := range msgs {
				if msgs[i].IsMine {
					msgs[i].State = pending[msgs[i].ID]
					msgs[i].Pending = msgs[i].State != ""
				}
			}
		}
	}
	return msgs, nil
}

func (n *NodeApi) GetCallManager() *call.CallManager {
	if n.node == nil {
		return nil
	}
	return n.node.CallManager
}

// JoinCall connects to a call room, sets up local loopback bridge, and returns the SDP Offer.
func (n *NodeApi) JoinCall(callID string, coordPeerID string) (string, error) {
	if n.node == nil || n.node.CallManager == nil {
		return "", errors.New("call manager not initialized")
	}

	var coordID types.PeerID
	if coordPeerID != "" {
		bytes, err := hex.DecodeString(coordPeerID)
		if err == nil && len(bytes) == 32 {
			copy(coordID[:], bytes)
		}
	}
	if coordID == (types.PeerID{}) {
		coordID = n.node.ID
	}

	// The invite is the room secret; only its hash goes on the wire.
	roomID, roomKey := call.DeriveRoom(callID)
	profile := tree.PeerNodeProfile{
		PeerID:           n.node.ID,
		UploadKbps:       n.callUploadKbps(),
		MaxRelayChildren: n.node.CallRelayCapacity(),
		RelayOnly:        n.node.CallRelayOnly(),
		RTT:              10 * time.Millisecond,
		LastHeartbeat:    time.Now(),
	}

	err := n.node.CallManager.JoinRoom(roomID, coordID, roomKey, profile)
	if err != nil {
		return "", err
	}

	return n.node.CallManager.GetBridgeOffer()
}

// SubmitCallAnswer delivers the frontend WebRTC SDP Answer to the Go loopback bridge.
func (n *NodeApi) SubmitCallAnswer(sdpAnswer string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	return n.node.CallManager.SubmitBridgeAnswer(sdpAnswer)
}

// LeaveCall cleanly exits the call room and releases media resources.
func (n *NodeApi) LeaveCall() error {
	if n.node == nil || n.node.CallManager == nil {
		return nil
	}
	return n.node.CallManager.LeaveRoom()
}

// StartScreenShare registers and announces a screen share stream in the P2P network.
func (n *NodeApi) StartScreenShare(streamID string, bitrate int) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	return n.node.CallManager.StartScreenShare(streamID, "screen", bitrate)
}

// StopScreenShare closes the screen broadcast.
func (n *NodeApi) StopScreenShare(streamID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	return n.node.CallManager.StopScreenShare(streamID)
}

// WatchStream subscribes to another peer's stream.
func (n *NodeApi) WatchStream(streamID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	return n.node.CallManager.WatchStream(streamID)
}

// UnwatchStream cancels subscription to another peer's stream.
func (n *NodeApi) UnwatchStream(streamID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	return n.node.CallManager.UnwatchStream(streamID)
}

// SetVADState broadcasts voice activity detection status to the call room.
func (n *NodeApi) SetVADState(speaking bool, audioDb float64) {
	if n.node == nil || n.node.CallManager == nil {
		return
	}
	n.node.CallManager.SetVADState(speaking, float32(audioDb))
}

// SetupCallCallbacks wires up CallManager events to external listeners.
func (n *NodeApi) SetupCallCallbacks(
	onActiveSpeaker func(speaker string, speaking bool, db float32),
	onStreamAnnounced func(streamID string, broadcaster string, mediaType string, bitrate int),
	onStreamClosed func(streamID string),
	onTopologyUpdated func(epoch uint64, coordinator string),
) {
	if n.node == nil || n.node.CallManager == nil {
		return
	}
	cm := n.node.CallManager
	if onActiveSpeaker != nil {
		cm.OnActiveSpeaker = func(speaker types.PeerID, speaking bool, db float32) {
			onActiveSpeaker(speaker.String(), speaking, db)
		}
	}
	if onStreamAnnounced != nil {
		cm.OnStreamAnnounced = func(streamID string, broadcaster types.PeerID, mediaType string, bitrate int) {
			onStreamAnnounced(streamID, broadcaster.String(), mediaType, bitrate)
		}
	}
	if onStreamClosed != nil {
		cm.OnStreamClosed = func(streamID string) {
			onStreamClosed(streamID)
		}
	}
	if onTopologyUpdated != nil {
		cm.OnTopologyUpdated = func(manifest *tree.TreeManifest) {
			if manifest != nil {
				onTopologyUpdated(manifest.Epoch, manifest.Coordinator.String())
			}
		}
		cm.OnCoordinatorChanged = func(newCoordinator types.PeerID, epoch uint64) {
			onTopologyUpdated(epoch, newCoordinator.String())
		}
	}
}

const defaultUploadKbps = 10000

func (n *NodeApi) callUploadKbps() int {
	if n.uploadKbps > 0 {
		return n.uploadKbps
	}
	return defaultUploadKbps
}

// SetUploadCapacity declares the local upstream bandwidth (kbps) used to validate stream quality.
func (n *NodeApi) SetUploadCapacity(kbps int) error {
	if kbps <= 0 {
		return errors.New("upload capacity must be positive")
	}
	n.uploadKbps = kbps
	if n.node != nil && n.node.CallManager != nil {
		n.node.CallManager.SetUploadCapacity(kbps)
	}
	return nil
}

// ListStreamQualities returns every broadcast preset and whether the current upload capacity sustains it.
func (n *NodeApi) ListStreamQualities() []dto.StreamQualityDTO {
	upload := n.callUploadKbps()
	out := make([]dto.StreamQualityDTO, 0, len(tree.QualityPresets))
	for _, q := range tree.QualityPresets {
		out = append(out, dto.StreamQualityDTO{
			ID: q.ID, Label: q.Label, Width: q.Width, Height: q.Height, FPS: q.FPS,
			BitrateKbps: q.BitrateKbps, Codec: q.Codec, Fits: q.FitsUpload(upload),
		})
	}
	return out
}

// StartStream announces a broadcast at a named quality preset (e.g. "2160p60").
func (n *NodeApi) StartStream(streamID, mediaType, qualityID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	q, err := tree.QualityByID(qualityID)
	if err != nil {
		return err
	}
	if mediaType == "" {
		mediaType = "screen"
	}
	n.allowQuality(q)
	return n.node.CallManager.StartStream(streamID, mediaType, q)
}

// allowQuality honours the user's choice of a preset above the declared upload: nobody knows their
// uplink better than the person who picks the quality, and the measured or default figure is only
// a guess. The declared capacity is raised to what the preset needs; the picture may stutter if the
// connection really cannot carry it, which the interface says.
func (n *NodeApi) allowQuality(q tree.StreamQuality) {
	if !q.FitsUpload(n.callUploadKbps()) {
		_ = n.SetUploadCapacity(q.BitrateKbps)
	}
}

// UpdateStreamQuality switches a running broadcast to another preset.
func (n *NodeApi) UpdateStreamQuality(streamID, qualityID string) error {
	if n.node == nil || n.node.CallManager == nil {
		return errors.New("call manager not initialized")
	}
	q, err := tree.QualityByID(qualityID)
	if err != nil {
		return err
	}
	n.allowQuality(q)
	return n.node.CallManager.UpdateStreamQuality(streamID, q)
}

// SetupStreamInfoCallback delivers announced streams together with their quality metadata.
func (n *NodeApi) SetupStreamInfoCallback(fn func(info dto.StreamInfoDTO)) {
	if fn == nil || n.node == nil || n.node.CallManager == nil {
		return
	}
	n.node.CallManager.OnStreamInfo = func(a signaling.StreamAnnouncePayload) {
		fn(dto.StreamInfoDTO{
			StreamID: a.StreamID, Broadcaster: a.Broadcaster.String(), MediaType: a.MediaType,
			Bitrate: a.BitrateKbps, Width: a.Width, Height: a.Height, FPS: a.FPS, Codec: a.Codec, QualityID: a.QualityID,
		})
	}
}

// TriggerPanicButton wipes the identity's key material from memory at once, so a device seized
// while unlocked yields no keys from RAM. It does NOT erase anything on disk: the encrypted
// identity stays, and the same app password unlocks it again (log in, or restart the app). To make
// the data unrecoverable, delete the profile. This is a lock, not a self-destruct.
func (n *NodeApi) TriggerPanicButton() error {
	return n.node.PanicWipe()
}

// ResetEncryptionSession forgets the end-to-end session with a contact. Use it when messages with
// them stop arriving although both are online; the next message starts a fresh session.
func (n *NodeApi) ResetEncryptionSession(peerID string) error {
	raw, err := hex.DecodeString(peerID)
	if err != nil || len(raw) != len(types.PeerID{}) {
		return errors.New("bad peer id")
	}
	var id types.PeerID
	copy(id[:], raw)
	if n.node.Messenger == nil {
		return errNoChat
	}
	return n.node.Messenger.ResetSession(id)
}

// CheckRecoveryPhrase says what is wrong with a typed recovery phrase ("" when it is fine), before
// anything is restored from it.
func CheckRecoveryPhrase(phrase string) string {
	if _, err := keymanager.CheckPhrase(phrase); err != nil {
		return err.Error()
	}
	return ""
}
