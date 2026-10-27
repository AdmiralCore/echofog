package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"

	api "github.com/echofog/echofog-core/pkg/api/node"
	"github.com/echofog/echofog-core/pkg/dto"
	"github.com/echofog/echofog-core/pkg/logging"
	coreNode "github.com/echofog/echofog-core/pkg/node"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the central application facade for the Wails runtime bridge.
// All operations are synchronized via sync.RWMutex to ensure thread-safety
// across concurrent RPC calls from the frontend.
type App struct {
	mu             sync.RWMutex
	ctx            context.Context
	api            *api.NodeApi
	activeProfile  string
	profileService ProfileService
}

// NewApp creates a new App facade instance.
func NewApp() *App {
	return &App{profileService: NewProfileService()}
}

// SetProfileService allows injection of custom or mock ProfileService for testing.
func (a *App) SetProfileService(svc ProfileService) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.profileService = svc
}

// shutdown stops the node when the window closes, so the database is flushed and closed.
func (a *App) shutdown(ctx context.Context) {
	a.Logout()
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()

	// persist logs to a file the Logs view can export, unless the user set their own path
	if os.Getenv("ECHOFOG_LOG_FILE") == "" {
		_ = os.Setenv("ECHOFOG_LOG_FILE", logging.DefaultFilePath())
	}
	runtime.LogInfo(ctx, "Wails frontend connected via bridge")
}

// Safe helpers
func (a *App) getAPI() *api.NodeApi {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.api
}

func (a *App) getActiveProfile() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.activeProfile
}

func (a *App) IsRegistered() bool {
	profile := a.getActiveProfile()
	if profile == "" {
		profile = "default"
	}

	a.mu.RLock()
	svc := a.profileService
	ctx := a.ctx
	a.mu.RUnlock()

	if svc != nil {
		return svc.IsRegistered(ctx, profile)
	}

	cfg := api.GetDefaultConfig(profile)
	tempNode := coreNode.NewNode(ctx, cfg, "Pre-Init-"+profile)
	return tempNode.IsRegistered()
}

func (a *App) Register(appPassword string, passphrase string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.activeProfile == "" {
		a.activeProfile = "default"
	}
	profile := a.activeProfile

	cfg := api.GetDefaultConfig(profile)
	node := coreNode.NewNode(a.ctx, cfg, profile)
	a.api = api.NewNodeApi(node)

	mnemonic, err := a.api.Register(appPassword, passphrase)
	if err != nil {
		slog.Error("Registration failed", "profile", profile, "error", err)
		return "", err
	}

	slog.Info("Successfully registered new node profile", "profile", profile)
	return mnemonic, nil
}

func (a *App) Login(appPassword string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.activeProfile == "" {
		return errors.New("please select a profile first")
	}
	profile := a.activeProfile

	cfg := api.GetDefaultConfig(profile)
	node := coreNode.NewNode(a.ctx, cfg, profile)
	a.api = api.NewNodeApi(node)

	err := a.api.Login(appPassword)
	if err != nil {
		slog.Error("Login failed", "profile", profile, "error", err)
		a.api = nil // the failed node released the database; the next attempt builds a fresh one
		return err
	}

	slog.Info("Successfully logged into node profile", "profile", profile)
	return nil
}

func (a *App) Logout() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.api != nil {
		a.api.StopNode()
		a.api = nil
	}
	a.activeProfile = ""
	slog.Info("Successfully logged out and stopped node")
}

func (a *App) Recover(mnemonic string, passphrase string, appPassword string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.activeProfile == "" {
		return errors.New("please select a profile first")
	}
	profile := a.activeProfile

	cfg := api.GetDefaultConfig(profile)
	node := coreNode.NewNode(a.ctx, cfg, "EchoFog-Node")
	a.api = api.NewNodeApi(node)

	err := a.api.Recover(mnemonic, passphrase, appPassword)
	if err != nil {
		slog.Error("Recovery failed", "profile", profile, "error", err)
		return err
	}

	slog.Info("Successfully recovered node profile", "profile", profile)
	return nil
}

func (a *App) GetProfiles() []string {
	a.mu.RLock()
	svc := a.profileService
	a.mu.RUnlock()

	if svc != nil {
		profiles, err := svc.GetProfiles()
		if err == nil {
			return profiles
		}
	}

	profiles, _ := api.GetAvailableProfiles()
	return profiles
}

func (a *App) SelectProfile(profileName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeProfile = profileName
}

// DeleteProfile removes a profile and its data. It requires the profile's app password: the method
// is callable from the WebView, so without the password a compromised page could wipe the user's
// keys and messages on its own.
func (a *App) DeleteProfile(profileName string, appPassword string) error {
	a.mu.RLock()
	svc := a.profileService
	activeAPI := a.api
	isActive := a.activeProfile == profileName
	a.mu.RUnlock()
	if svc == nil {
		return errors.New("profile service unavailable")
	}
	if appPassword == "" {
		return errors.New("the app password is required to delete a profile")
	}

	// The active profile keeps its database open, so the profile service cannot reopen it to check
	// the password. Verify against the live node first (a typo then leaves the session untouched),
	// then stop the node to release the database before the directory is removed.
	if isActive && activeAPI != nil {
		if err := activeAPI.VerifyPassword(appPassword); err != nil {
			slog.Warn("Profile deletion refused: wrong app password", "profile", profileName)
			return errors.New("wrong app password")
		}
		a.mu.Lock()
		if a.api != nil {
			a.api.StopNode()
			a.api = nil
		}
		a.mu.Unlock()
	}

	if err := svc.DeleteProfile(profileName, appPassword); err != nil {
		return err
	}

	a.mu.Lock()
	if a.activeProfile == profileName {
		a.activeProfile = ""
	}
	a.mu.Unlock()
	return nil
}

func (a *App) StartNode() error {
	a.mu.Lock()
	nodeAPI := a.api
	ctx := a.ctx
	a.mu.Unlock()

	if nodeAPI == nil {
		return errors.New("api not initialized: call Login or Register first")
	}

	err := nodeAPI.StartNode()
	if err != nil {
		return err
	}

	a.startEventListeners(nodeAPI, ctx)

	return nil
}

func safeEventsEmit(ctx context.Context, eventName string, optionalData ...interface{}) {
	if ctx == nil || ctx.Value("events") == nil {
		return // Headless mode or unit testing without active Wails frontend
	}
	defer func() {
		_ = recover()
	}()
	runtime.EventsEmit(ctx, eventName, optionalData...)
}

func (a *App) startEventListeners(nodeAPI *api.NodeApi, ctx context.Context) {
	if nodeAPI == nil {
		return
	}

	msgCh := nodeAPI.MessageHandler()
	go func() {
		for msg := range msgCh {
			safeEventsEmit(ctx, "new_message", msg)
			a.emitIncomingCall(ctx, msg)
		}
	}()
	// a message of ours was handed to a relay, confirmed by the recipient, or given up
	nodeAPI.OnMessageState(func(ev api.MessageStateDTO) {
		safeEventsEmit(ctx, "message:state", ev)
	})

	nodeAPI.OnFileTransfer(func(t dto.FileTransferDTO) {
		safeEventsEmit(ctx, "file:transfer", t)
	})

	nodeAPI.SubscribeGroupEvents(func(ev api.GroupEvent) {
		safeEventsEmit(ctx, "group:event", ev)
	})
	// a chat changed: a new or changed message, or its place in the chat list (unread, last message)
	nodeAPI.SubscribeConversations(func(ch api.ConvChange) {
		safeEventsEmit(ctx, "conv:change", ch)
	})

	peerCh := nodeAPI.PeerDiscoveryHandler()
	go func() {
		for peer := range peerCh {
			safeEventsEmit(ctx, "peer_discovered", peer)
		}
	}()

	nodeAPI.SetupStreamInfoCallback(func(info dto.StreamInfoDTO) {
		safeEventsEmit(ctx, "call:stream_announced", info)
	})

	nodeAPI.SetupCallCallbacks(
		func(speaker string, speaking bool, db float32) {
			safeEventsEmit(ctx, "call:active_speaker", map[string]interface{}{
				"speaker":  speaker,
				"speaking": speaking,
				"db":       db,
			})
		},
		func(streamID string, broadcaster string, mediaType string, bitrate int) {
			safeEventsEmit(ctx, "call:stream_announced", map[string]interface{}{
				"streamId":    streamID,
				"broadcaster": broadcaster,
				"mediaType":   mediaType,
				"bitrate":     bitrate,
			})
		},
		func(streamID string) {
			safeEventsEmit(ctx, "call:stream_closed", map[string]interface{}{
				"streamId": streamID,
			})
		},
		func(epoch uint64, coordinator string) {
			safeEventsEmit(ctx, "call:topology_updated", map[string]interface{}{
				"epoch":       epoch,
				"coordinator": coordinator,
			})
			safeEventsEmit(ctx, "call:roster", nodeAPI.GetCallRoster())
		},
	)
	a.wireCallMediaEvents(nodeAPI, ctx)

	nodeAPI.Logger.Info("Frontend event listeners started")
}

func (a *App) GetMyID() string {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return ""
	}
	return nodeAPI.GetMyID()
}

func (a *App) StartLocalDiscovery(myAlias string) bool {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return false
	}
	nodeAPI.StartLocalDiscovery(myAlias)
	return true
}

func (a *App) StopDiscovery() {
	nodeAPI := a.getAPI()
	if nodeAPI != nil {
		nodeAPI.StopLocalDiscovery()
	}
}

func (a *App) SendMessage(pubKey string, text string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.SendMessage(pubKey, text)
}

func (a *App) ConnectToLocalPeer(addr string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}

	nodeAPI.Logger.Info("UI requested connection to peer", "addr", addr)
	err := nodeAPI.ConnectToPeer(addr)
	if err != nil {
		nodeAPI.Logger.Error("Failed to connect to local peer", "addr", addr, "err", err)
		return err
	}

	nodeAPI.Logger.Info("Successfully connected to local peer", "addr", addr)
	return nil
}

func (a *App) GetChatHistory(targetPeerID string, limit int, beforeTimestamp uint64) ([]dto.MessageDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node not started")
	}
	return nodeAPI.GetMessages(targetPeerID, limit, beforeTimestamp)
}

func (a *App) GetContacts() ([]dto.ContactDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node not started")
	}
	return nodeAPI.GetContacts()
}

func (a *App) AddContact(contact dto.ContactDTO) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.NewContact(contact)
}

func (a *App) DeleteContact(peerID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node not started")
	}
	return nodeAPI.DeleteContact(peerID)
}

func (a *App) GetNetworkStatus() map[string]interface{} {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return map[string]interface{}{"peerCount": 0, "bytesRx": 0, "bytesTx": 0, "status": "OFFLINE"}
	}
	s := nodeAPI.GetNetworkStats()
	status := "OFFLINE"
	if s.Online {
		status = "ONLINE"
	}
	return map[string]interface{}{
		"peerCount": s.ConnectedPeers,
		"bytesRx":   s.BytesRx,
		"bytesTx":   s.BytesTx,
		"status":    status,
	}
}

// JoinRoom enters a call room and returns the SDP Offer for local WebRTC loopback.
func (a *App) JoinRoom(callID string, coordPeerID string) (string, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return "", errors.New("node API not initialized: please log in first")
	}

	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	if ctx != nil {
		nodeAPI.SetupCallCallbacks(
			func(speaker string, speaking bool, db float32) {
				runtime.EventsEmit(ctx, "call:active_speaker", map[string]interface{}{
					"speaker":  speaker,
					"speaking": speaking,
					"db":       db,
				})
			},
			nil,
			func(streamID string) {
				runtime.EventsEmit(ctx, "call:stream_closed", map[string]interface{}{
					"streamId": streamID,
				})
			},
			func(epoch uint64, coordinator string) {
				runtime.EventsEmit(ctx, "call:topology_updated", map[string]interface{}{
					"epoch":       epoch,
					"coordinator": coordinator,
				})
				runtime.EventsEmit(ctx, "call:roster", nodeAPI.GetCallRoster())
			},
		)
		a.wireCallMediaEvents(nodeAPI, ctx)
	}

	return nodeAPI.JoinCall(callID, coordPeerID)
}

// wireCallMediaEvents tells the UI which person or stream each loopback track carries.
func (a *App) wireCallMediaEvents(nodeAPI *api.NodeApi, ctx context.Context) {
	nodeAPI.SetupCallMediaCallbacks(
		func(slot dto.CallMediaSlotDTO) { safeEventsEmit(ctx, "call:media_slot", slot) },
		func(reason string) { safeEventsEmit(ctx, "call:kicked", map[string]interface{}{"reason": reason}) },
	)
}

// GetCallRoster lists the members of the active call.
func (a *App) GetCallRoster() dto.CallRosterDTO {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return dto.CallRosterDTO{}
	}
	return nodeAPI.GetCallRoster()
}

// GetCallSlots lists which remote speaker or stream plays on each loopback track.
func (a *App) GetCallSlots() []dto.CallMediaSlotDTO {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.GetCallSlots()
}

// ReportCallMember flags a call member for spam; enough reports remove and ban it.
func (a *App) ReportCallMember(peerID string, reason string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.ReportCallMember(peerID, reason)
}

// RequestRelayChange asks to receive a watched stream through another relay.
func (a *App) RequestRelayChange(streamID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.RequestRelayChange(streamID)
}

// SubmitCallAnswer forwards the browser SDP Answer to the local Pion WebRTC loopback bridge.
func (a *App) SubmitCallAnswer(sdpAnswer string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.SubmitCallAnswer(sdpAnswer)
}

// LeaveRoom exits the call room and releases media loopback resources.
func (a *App) LeaveRoom() error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil
	}
	return nodeAPI.LeaveCall()
}

// StartScreenShare registers and announces a screen share stream in the P2P tree.
func (a *App) StartScreenShare(streamID string, bitrate int) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.StartScreenShare(streamID, bitrate)
}

// StopScreenShare closes the screen share stream.
func (a *App) StopScreenShare(streamID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.StopScreenShare(streamID)
}

// WatchStream subscribes to another participant's stream.
func (a *App) WatchStream(streamID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.WatchStream(streamID)
}

// UnwatchStream cancels subscription to another participant's stream.
func (a *App) UnwatchStream(streamID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.UnwatchStream(streamID)
}

// SetVADState broadcasts voice activity detection status to the call room.
func (a *App) SetVADState(speaking bool, audioDb float64) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return
	}
	nodeAPI.SetVADState(speaking, audioDb)
}

// GetStreamQualities lists broadcast presets and whether the declared upload bandwidth sustains each.
func (a *App) GetStreamQualities() ([]dto.StreamQualityDTO, error) {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return nil, errors.New("node API not initialized")
	}
	return nodeAPI.ListStreamQualities(), nil
}

// StartStream announces a screen/camera stream at a named quality preset (e.g. "2160p60").
func (a *App) StartStream(streamID string, mediaType string, qualityID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.StartStream(streamID, mediaType, qualityID)
}

// UpdateStreamQuality switches a running stream to another preset.
func (a *App) UpdateStreamQuality(streamID string, qualityID string) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.UpdateStreamQuality(streamID, qualityID)
}

// SetUploadCapacity declares the upstream bandwidth (kbps) used to validate stream quality.
func (a *App) SetUploadCapacity(kbps int) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	return nodeAPI.SetUploadCapacity(kbps)
}
