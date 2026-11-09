package voice

// Speed is a playback speed: 1x, 1.5x or 2x.
type Speed uint8

const (
	Speed1x Speed = iota
	Speed15x
	Speed2x
)

// Factor is the playback rate; an unknown value plays at 1x.
func (s Speed) Factor() float64 {
	switch s {
	case Speed15x:
		return 1.5
	case Speed2x:
		return 2
	}
	return 1
}

// Next is the speed after s when the button is pressed: 1x, 1.5x, 2x, 1x.
func (s Speed) Next() Speed {
	switch s {
	case Speed1x:
		return Speed15x
	case Speed15x:
		return Speed2x
	}
	return Speed1x
}
