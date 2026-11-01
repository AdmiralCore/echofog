package main

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/echofog/echofog-desktop/internal/nativecap"
)

// The app's own screen capture (Windows): the webview's capture copies every frame through system
// memory and holds itself to half a core, which keeps a shared screen at 30-50 fps. This one keeps
// frames on the GPU and encodes them in hardware, then hands the call the same H.264 RTP the
// webview would have sent, so viewers see no difference but the frame rate. Where it cannot run
// (another system, Windows before 1903, no hardware encoder, any failure to start) the interface
// shares through the webview as before.

// screenRole is the capture role of a shared screen in the call (bridge.RoleScreen).
const screenRole = "screen"

var screenLog = slog.Default().With("component", "call-screen")

type nativeScreen struct {
	mu       sync.Mutex
	stream   *nativecap.Stream
	streamID string
	sourceID string
	cursor   bool
}

// NativeScreenInfo tells the interface whether the app's own capture can be used, and why not.
type NativeScreenInfo struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// NativeScreenAvailable reports whether screens can be shared with the app's own capture.
func (a *App) NativeScreenAvailable() NativeScreenInfo {
	if err := nativecap.Available(); err != nil {
		return NativeScreenInfo{Reason: err.Error()}
	}
	return NativeScreenInfo{Available: true}
}

// ListScreenSources lists the monitors and windows that can be shared.
func (a *App) ListScreenSources() []nativecap.Source { return nativecap.Sources() }

// StartNativeScreen captures a source into the screen stream streamID, which StartStream already
// announced. On an error nothing runs and the interface can share through the webview instead.
func (a *App) StartNativeScreen(streamID, sourceID, qualityID string, cursor bool) error {
	a.screen.mu.Lock()
	defer a.screen.mu.Unlock()
	a.stopNativeScreenLocked("replaced")
	if err := a.startNativeScreenLocked(streamID, sourceID, qualityID, cursor); err != nil {
		screenLog.Warn("Native screen capture did not start, the webview shares instead", "stream", streamID, "source", sourceKind(sourceID), "quality", qualityID, "err", err)
		return err
	}
	return nil
}

func (a *App) startNativeScreenLocked(streamID, sourceID, qualityID string, cursor bool) error {
	nodeAPI := a.getAPI()
	if nodeAPI == nil {
		return errors.New("node API not initialized")
	}
	var q *struct{ w, h, fps, kbps int }
	for _, p := range nodeAPI.ListStreamQualities() {
		if p.ID == qualityID {
			q = &struct{ w, h, fps, kbps int }{p.Width, p.Height, p.FPS, p.BitrateKbps}
		}
	}
	if q == nil {
		return errors.New("unknown stream quality " + qualityID)
	}
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	var s *nativecap.Stream
	s, err := nativecap.Start(nativecap.Options{
		Source: sourceID, Width: q.w, Height: q.h, FPS: q.fps, BitrateKbps: q.kbps, Cursor: cursor,
		MTU:     min(nodeAPI.CaptureRTPBudget(streamID), 1200),
		Publish: func(p []byte) { nodeAPI.PublishCaptureRTP(screenRole, p) },
		OnEnd: func(err error) {
			screenLog.Warn("Native screen capture ended by itself", "stream", streamID, "err", err)
			a.screen.mu.Lock()
			if a.screen.stream == s {
				a.stopNativeScreenLocked("ended")
			}
			a.screen.mu.Unlock()
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			safeEventsEmit(ctx, "screen:native_ended", map[string]any{"streamId": streamID, "reason": reason})
		},
	})
	if err != nil {
		return err
	}
	if err := nodeAPI.SetCaptureEncoder(screenRole, s); err != nil {
		s.Stop()
		return err
	}
	a.screen.stream, a.screen.streamID, a.screen.sourceID, a.screen.cursor = s, streamID, sourceID, cursor
	st := s.Stats()
	screenLog.Info("Native screen capture started", "stream", streamID, "source", sourceKind(sourceID), "quality", qualityID,
		"width", st.Width, "height", st.Height, "source_width", st.SourceWidth, "source_height", st.SourceHeight,
		"fps", q.fps, "kbps", q.kbps, "encoder", st.Encoder, "adapter", st.Adapter)
	return nil
}

// UpdateNativeScreen restarts the capture of the shared screen at another quality (size and
// frame rate are fixed for an encoder's life); the bitrate alone follows the rate control.
func (a *App) UpdateNativeScreen(qualityID string) error {
	a.screen.mu.Lock()
	defer a.screen.mu.Unlock()
	if a.screen.stream == nil {
		return errors.New("no native screen capture is running")
	}
	streamID, sourceID, cursor := a.screen.streamID, a.screen.sourceID, a.screen.cursor
	a.stopNativeScreenLocked("quality changed")
	return a.startNativeScreenLocked(streamID, sourceID, qualityID, cursor)
}

// StopNativeScreen ends the app's own capture, if it runs.
func (a *App) StopNativeScreen() {
	a.screen.mu.Lock()
	a.stopNativeScreenLocked("stopped")
	a.screen.mu.Unlock()
}

func (a *App) stopNativeScreenLocked(why string) {
	s := a.screen.stream
	if s == nil {
		return
	}
	a.screen.stream = nil
	if nodeAPI := a.getAPI(); nodeAPI != nil {
		_ = nodeAPI.SetCaptureEncoder(screenRole, nil)
	}
	s.Stop()
	st := s.Stats()
	screenLog.Info("Native screen capture stopped", "stream", a.screen.streamID, "why", why, "captured", st.Captured,
		"encoded", st.Encoded, "sent", st.Sent, "skipped", st.Skipped, "keyframes", st.Keyframes, "mbytes", st.Bytes/1_000_000)
}

// NativeScreenStatsDTO is what the app's own capture did so far, for the stream stats and log.
type NativeScreenStatsDTO struct {
	Active   bool    `json:"active"`
	StreamID string  `json:"streamId"`
	At       float64 `json:"at"` // ms since the epoch, for rates
	nativecap.Stats
}

// NativeScreenStats returns the totals of the running capture (Active false: none runs).
func (a *App) NativeScreenStats() NativeScreenStatsDTO {
	a.screen.mu.Lock()
	s, id := a.screen.stream, a.screen.streamID
	a.screen.mu.Unlock()
	if s == nil {
		return NativeScreenStatsDTO{}
	}
	return NativeScreenStatsDTO{Active: true, StreamID: id, At: float64(time.Now().UnixMicro()) / 1000, Stats: s.Stats()}
}

// sourceKind is what the log says of a source: its kind, not which window (titles are private).
func sourceKind(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			return id[:i]
		}
	}
	return id
}
