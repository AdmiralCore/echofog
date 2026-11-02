package crypto

import "time"

// SetClock sets the time the engine's waits are measured in.
func SetClock(e *CryptoEngine, now func() time.Time) { e.now = now }
