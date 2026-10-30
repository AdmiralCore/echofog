package tree

import (
	"errors"
	"fmt"
)

var (
	ErrUnknownQuality     = errors.New("unknown stream quality preset")
	ErrInsufficientUpload = errors.New("upload bandwidth is too low for the selected stream quality")
)

// StreamQuality describes the capture and encoding target of a broadcast.
type StreamQuality struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPS         int    `json:"fps"`
	BitrateKbps int    `json:"bitrate_kbps"`
	Codec       string `json:"codec"`
}

// QualityPresets are ordered from the lightest to the heaviest profile.
//
// The bitrates are what H.264 needs for a moving picture (a game, a film) to stay sharp; text on a
// still screen needs much less, and the encoder sends less when the picture does not change. The
// 120 fps presets are for screens that refresh that fast; whether the capture keeps up depends on
// the computer, and the stats line in the log says what it really sends.
var QualityPresets = []StreamQuality{
	{ID: "360p30", Label: "360p 30 fps", Width: 640, Height: 360, FPS: 30, BitrateKbps: 1000, Codec: "h264"},
	{ID: "720p30", Label: "720p 30 fps", Width: 1280, Height: 720, FPS: 30, BitrateKbps: 3000, Codec: "h264"},
	{ID: "720p60", Label: "720p 60 fps", Width: 1280, Height: 720, FPS: 60, BitrateKbps: 6000, Codec: "h264"},
	{ID: "1080p30", Label: "1080p 30 fps", Width: 1920, Height: 1080, FPS: 30, BitrateKbps: 7000, Codec: "h264"},
	{ID: "1080p60", Label: "1080p 60 fps", Width: 1920, Height: 1080, FPS: 60, BitrateKbps: 12000, Codec: "h264"},
	{ID: "1080p120", Label: "1080p 120 fps", Width: 1920, Height: 1080, FPS: 120, BitrateKbps: 20000, Codec: "h264"},
	{ID: "1440p60", Label: "1440p 60 fps", Width: 2560, Height: 1440, FPS: 60, BitrateKbps: 20000, Codec: "h264"},
	{ID: "1440p120", Label: "1440p 120 fps", Width: 2560, Height: 1440, FPS: 120, BitrateKbps: 32000, Codec: "h264"},
	{ID: "2160p30", Label: "4K 30 fps", Width: 3840, Height: 2160, FPS: 30, BitrateKbps: 20000, Codec: "h264"},
	{ID: "2160p60", Label: "4K 60 fps", Width: 3840, Height: 2160, FPS: 60, BitrateKbps: 35000, Codec: "h264"},
}

// DefaultQualityID is used when the caller does not pick a preset.
const DefaultQualityID = "1080p30"

// QualityByID returns the preset with the given ID.
func QualityByID(id string) (StreamQuality, error) {
	if id == "" {
		id = DefaultQualityID
	}
	for _, q := range QualityPresets {
		if q.ID == id {
			return q, nil
		}
	}
	return StreamQuality{}, fmt.Errorf("%w: %q", ErrUnknownQuality, id)
}

// CustomQuality builds a quality from a bare bitrate, used by the legacy bitrate-only API.
func CustomQuality(bitrateKbps int) StreamQuality {
	return StreamQuality{ID: "custom", Label: fmt.Sprintf("%d kbps", bitrateKbps), BitrateKbps: bitrateKbps, Codec: "h264"}
}

// FitsUpload reports whether the broadcaster can feed at least one downstream peer.
// A zero uploadKbps means the capacity is unknown and is not enforced.
func (q StreamQuality) FitsUpload(uploadKbps int) bool {
	return uploadKbps <= 0 || q.BitrateKbps <= uploadKbps
}

// BestQualityFor returns the heaviest preset that fits the given upload capacity.
func BestQualityFor(uploadKbps int) StreamQuality {
	best := QualityPresets[0]
	for _, q := range QualityPresets {
		if q.FitsUpload(uploadKbps) {
			best = q
		}
	}
	return best
}
