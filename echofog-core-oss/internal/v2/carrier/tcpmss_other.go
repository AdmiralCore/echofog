//go:build !(linux || darwin || freebsd)

package carrier

// mssSupported is false here: Windows and the rest have no settable TCP_MAXSEG, and the retry with
// a limited MSS is reported as unsupported instead of being worked around (T3.12).
const mssSupported = false

func setTCPMSS(uintptr, int) error { return ErrMSSUnsupported }
