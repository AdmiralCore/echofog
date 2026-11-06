package servernode

// Message returns the operator-facing text for a code in a language ("ru" or "en"). Russian texts
// are in the neutral form. An unknown code returns "".
func Message(lang, code string, args ...any) string { return "" }

// Codes lists every message code (tests check that each has both languages).
func Codes() []string { return nil }
