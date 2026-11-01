//go:build !windows

package nativecap

import "errors"

var errNotWindows = errors.New("the app's own screen capture exists on Windows only")

// Available reports why this computer cannot stream its screen natively: elsewhere than on
// Windows the webview shares the screen.
func Available() error { return errNotWindows }

// Sources lists nothing outside Windows.
func Sources() []Source { return nil }

// Stream is a native screen stream; there are none outside Windows.
type Stream struct{}

func Start(Options) (*Stream, error)    { return nil, errNotWindows }
func (s *Stream) SetBitrate(uint64)     {}
func (s *Stream) RequestKeyframe()      {}
func (s *Stream) Stats() Stats          { return Stats{} }
func (s *Stream) Stop()                 {}
func (s *Stream) Done() <-chan struct{} { return nil }

// Previews has no thumbnails outside Windows.
func Previews([]string) map[string]string { return nil }

// AudioCapture is the capture of the computer's sound; there is none outside Windows.
type AudioCapture struct{}

func StartAudio(func([]byte)) (*AudioCapture, error) { return nil, errNotWindows }
func (a *AudioCapture) Stop()                        {}
