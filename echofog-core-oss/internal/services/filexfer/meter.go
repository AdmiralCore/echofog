package filexfer

import (
	"sort"
	"time"
)

// meter tells the recent transfer rate and when a progress update is due.
type meter struct {
	winStart time.Time
	winBytes int64
	last     int64 // bytes per second over the last full window
	emitted  time.Time
}

const meterWindow = time.Second

func (m *meter) add(n int64) {
	now := time.Now()
	if m.winStart.IsZero() {
		m.winStart = now
	}
	if el := now.Sub(m.winStart); el >= meterWindow {
		m.last = int64(float64(m.winBytes) / el.Seconds())
		m.winStart, m.winBytes = now, 0
	}
	m.winBytes += n
}

func (m *meter) rate() int64 {
	if !m.winStart.IsZero() && time.Since(m.winStart) > 3*meterWindow {
		return 0 // nothing for a while
	}
	return m.last
}

// due reports whether a progress update should go out now (four a second at most).
func (m *meter) due() bool {
	if time.Since(m.emitted) < 250*time.Millisecond {
		return false
	}
	m.emitted = time.Now()
	return true
}

func sortInfos(list []Info) {
	sort.Slice(list, func(i, j int) bool { return list[i].Created < list[j].Created })
}
