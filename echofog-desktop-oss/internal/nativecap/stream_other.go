//go:build !windows

package nativecap

import "errors"

var errNotWindows = errors.New("the app's own screen capture exists on Windows only")

// Available reports why this computer cannot stream its screen natively: elsewhere than on
// Windows the webview shares the screen.
func Available() error { return errNotWindows }

// Sources lists nothing outside Windows.
func Sources() []Source { return nil }

// DisplayRate does not know the screen's refresh rate outside Windows.
func DisplayRate() int { return 0 }

// Stream is a native screen stream; there are none outside Windows.
type Stream struct{}

// Start fails off Windows: there is no native capture.
func Start(Options) (*Stream, error) { return nil, errNotWindows }

// SetBitrate, RequestKeyframe, SetViewerRates, Stats, Stop and Done do nothing: no Stream exists off
// Windows.
func (s *Stream) SetBitrate(uint64)     {}
func (s *Stream) RequestKeyframe()      {}
func (s *Stream) SetViewerRates([]int)  {}
func (s *Stream) Stats() Stats          { return Stats{} }
func (s *Stream) Stop()                 {}
func (s *Stream) Done() <-chan struct{} { return nil }

// Previews has no thumbnails outside Windows.
func Previews([]string) map[string]string { return nil }

// AudioCapture is the capture of the computer's sound; there is none outside Windows.
type AudioCapture struct{}

// StartAudio fails off Windows.
func StartAudio(string, func([]byte)) (*AudioCapture, error) { return nil, errNotWindows }

// Stop does nothing: no AudioCapture exists off Windows.
func (a *AudioCapture) Stop() {}
