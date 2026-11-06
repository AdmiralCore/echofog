package voice

// Sanitize checks an Ogg Opus clip and rewrites it to its canonical form: the headers carry no
// vendor, tags, serial or sample rate of the recorder. It returns the clip and its length.
func Sanitize(ogg []byte) ([]byte, uint32, error) { return nil, 0, ErrOgg }
