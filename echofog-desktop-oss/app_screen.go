package main

import (
	"encoding/base64"
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
	audio    *nativecap.AudioCapture // the screen's sound, when shared
	earlier  nativecap.Stats         // totals of the stream's captures before a quality change
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

// GetScreenPreviews returns live thumbnails of the given sources (JPEG data URLs by ID) for the
// picker, which asks a few times a second while it is open; an empty list ends them.
func (a *App) GetScreenPreviews(ids []string) map[string]string { return nativecap.Previews(ids) }

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
	go logNativeScreen(s, streamID)
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
	streamID, sourceID, cursor, sound := a.screen.streamID, a.screen.sourceID, a.screen.cursor, a.screen.audio != nil
	a.stopNativeScreenLocked("quality changed")
	if err := a.startNativeScreenLocked(streamID, sourceID, qualityID, cursor); err != nil {
		return err
	}
	if sound {
		_ = a.startNativeAudioLocked() // it logs a failure; the picture goes on without sound
	}
	return nil
}

// StopNativeScreen ends the app's own capture, if it runs.
func (a *App) StopNativeScreen() {
	a.screen.mu.Lock()
	a.stopNativeScreenLocked("stopped")
	a.screen.mu.Unlock()
}

// screenPCMChunk is how much sound goes to the interface at once: 20 ms.
const screenPCMChunk = nativecap.AudioRate / 50 * 4

// StartNativeScreenAudio shares the computer's sound with the screen the app captures: all it
// plays except EchoFog, so viewers do not hear the call twice. It reaches the interface as
// "screen:pcm" events (20 ms of 48 kHz stereo 16-bit PCM, base64), which plays it into the
// screen-sound track for the webview to encode.
func (a *App) StartNativeScreenAudio() error {
	a.screen.mu.Lock()
	defer a.screen.mu.Unlock()
	return a.startNativeAudioLocked()
}

func (a *App) startNativeAudioLocked() error {
	if a.screen.stream == nil {
		return errors.New("no native screen capture is running")
	}
	a.stopNativeAudioLocked()
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	var pending []byte
	audio, err := nativecap.StartAudio(func(pcm []byte) {
		pending = append(pending, pcm...)
		n := 0
		for ; len(pending)-n >= screenPCMChunk; n += screenPCMChunk {
			safeEventsEmit(ctx, "screen:pcm", base64.StdEncoding.EncodeToString(pending[n:n+screenPCMChunk]))
		}
		pending = append(pending[:0], pending[n:]...)
	})
	if err != nil {
		screenLog.Warn("Screen sound capture did not start, the screen is shared without sound", "stream", a.screen.streamID, "err", err)
		return err
	}
	a.screen.audio = audio
	screenLog.Info("Screen sound capture started", "stream", a.screen.streamID)
	return nil
}

// StopNativeScreenAudio stops sharing the computer's sound.
func (a *App) StopNativeScreenAudio() {
	a.screen.mu.Lock()
	a.stopNativeAudioLocked()
	a.screen.mu.Unlock()
}

func (a *App) stopNativeAudioLocked() {
	if a.screen.audio != nil {
		a.screen.audio.Stop()
		a.screen.audio = nil
	}
}

func (a *App) stopNativeScreenLocked(why string) {
	a.stopNativeAudioLocked()
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
	if why == "quality changed" {
		a.screen.earlier = st.After(a.screen.earlier)
	} else {
		a.screen.earlier = nativecap.Stats{}
	}
	screenLog.Info("Native screen capture stopped", "stream", a.screen.streamID, "why", why, "captured", st.Captured,
		"encoded", st.Encoded, "sent", st.Sent, "skipped", st.Skipped, "keyframes", st.Keyframes, "mbytes", st.Bytes/1_000_000)
}

// logNativeScreen puts what the capture does into the log every 10 s, so a stream log (which
// takes the call's lines) shows where frames go missing: capture, encoder or sending.
func logNativeScreen(s *nativecap.Stream, streamID string) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	prev, at := s.Stats(), time.Now()
	for {
		select {
		case <-s.Done():
			return
		case now := <-t.C:
			cur, dt := s.Stats(), now.Sub(at).Seconds()
			rate := func(a, b uint64) float64 { return float64(b-a) / dt }
			ms := 0.0
			if n := cur.Encoded - prev.Encoded; n > 0 {
				ms = (cur.EncodeSeconds - prev.EncodeSeconds) * 1000 / float64(n)
			}
			screenLog.Info("Native screen capture", "stream", streamID, "captured_fps", int(rate(prev.Captured, cur.Captured)),
				"encoded_fps", int(rate(prev.Encoded, cur.Encoded)), "sent_fps", int(rate(prev.Sent, cur.Sent)),
				"waits", cur.Skipped-prev.Skipped, "encode_ms", int(ms*10)/10.0, "mbps", int(rate(prev.Bytes, cur.Bytes)*8/1e5)/10.0,
				"kbps_cap", cur.BitrateKbps, "pacer_ms", int(cur.PacerDelayMs*10)/10.0)
			prev, at = cur, now
		}
	}
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
	s, id, earlier := a.screen.stream, a.screen.streamID, a.screen.earlier
	a.screen.mu.Unlock()
	if s == nil {
		return NativeScreenStatsDTO{}
	}
	return NativeScreenStatsDTO{Active: true, StreamID: id, At: float64(time.Now().UnixMicro()) / 1000, Stats: s.Stats().After(earlier)}
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
