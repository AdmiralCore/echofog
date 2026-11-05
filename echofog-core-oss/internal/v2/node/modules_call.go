package node

// registerCall registers call/2 (T9.1).
//
// The service package owns the handler and calls r.Register with the built-in spec. Keep this
// file present even while the service is not implemented: it is the merge point for call/2, so
// the service branch never edits modules.go or another service's file.
func (r *Runtime) registerCall() {
	// Registered by T9.1 when the service package is merged.
}
