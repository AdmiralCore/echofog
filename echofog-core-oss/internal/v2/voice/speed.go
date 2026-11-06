package voice

// Speed is a playback speed.
type Speed uint8

const (
	Speed1x Speed = iota
	Speed15x
	Speed2x
)

func (s Speed) Factor() float64 { return 0 }
func (s Speed) Next() Speed     { return s }
