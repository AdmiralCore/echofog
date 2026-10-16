package call

import (
	"time"

	"github.com/echofog/echofog-core/internal/services/call/media"
)

// The browser encoder only learns about the loopback to the bridge, which never congests. This
// loop watches the real path (outbox drops and queue depth) and caps the encoder with REMB:
// cut fast when the path backs up, climb back slowly when it is clean, never above the preset.
const (
	rateInterval  = 500 * time.Millisecond
	rateCut       = 0.8
	rateClimb     = 1.05
	rateFloorKbps = 600
)

type sendRateState struct {
	kbps        float64
	lastDropped uint64
	at          time.Time
}

func (m *CallManager) rateTickLocked(room *ActiveRoomState, now time.Time) {
	r := &room.sendRate
	if now.Sub(r.at) < rateInterval {
		return
	}
	r.at = now
	br := room.Bridge
	if br == nil || room.Dispatcher == nil || len(room.localStreams) == 0 {
		r.kbps = 0
		return
	}
	capKbps := 0
	for _, ann := range room.localStreams {
		capKbps += ann.BitrateKbps
	}
	if capKbps <= 0 {
		return
	}

	dropped, backlog := room.Dispatcher.VideoPressure()
	congested := dropped > r.lastDropped || backlog > media.VideoQueueLen/4
	r.lastDropped = dropped
	r.kbps = nextRate(r.kbps, float64(capKbps), float64(rateFloorKbps*len(room.localStreams)), congested)

	type limit struct {
		role string
		bps  uint64
	}
	var limits []limit
	for _, ann := range room.localStreams {
		share := r.kbps * float64(ann.BitrateKbps) / float64(capKbps)
		limits = append(limits, limit{captureRole(ann.MediaType), uint64(share * 1000)})
	}
	m.later(func() {
		for _, l := range limits {
			_ = br.SetSendBitrate(l.role, l.bps)
		}
	})
}

// nextRate is one AIMD step; zero means no rate yet, which starts at the preset.
func nextRate(kbps, capKbps, floor float64, congested bool) float64 {
	switch {
	case kbps == 0:
		kbps = capKbps
	case congested:
		kbps *= rateCut
	default:
		kbps *= rateClimb
	}
	return min(max(kbps, floor), capKbps)
}
