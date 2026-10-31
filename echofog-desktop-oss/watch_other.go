//go:build !windows || dev

package main

// watch: outside Windows (and in `wails dev`) the window runs in the process the user started, and
// a crash is offered as a report at the next start only.
func watch() bool { return false }
