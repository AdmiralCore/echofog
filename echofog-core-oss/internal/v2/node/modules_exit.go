package node

// registerExit registers exit/2 (T10.1).
//
// The service package owns the handler and calls r.Register with the built-in spec. Keep this
// file present even while the service is not implemented: it is the merge point for exit/2, so
// the service branch never edits modules.go or another service's file.
func (r *Runtime) registerExit() {
	// Registered by T10.1 when the service package is merged.
}
