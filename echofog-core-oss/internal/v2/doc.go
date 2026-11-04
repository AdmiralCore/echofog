// Package v2 is the root of the EchoFog protocol v2 tree.
//
// The protocol itself lives in the subpackages (frame, handshake, keys, mux, noise, objects,
// service). This package exists so that the tree-level checks in imports_test.go and
// platform_test.go have a package to run in, and so that ./internal/v2/... can be listed and
// built as a whole.
package v2
