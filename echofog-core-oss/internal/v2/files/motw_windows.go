//go:build windows

package files

import "os"

// markFromInternet gives a received file the Mark of the Web, as browsers do with downloads, so
// Windows warns before running a program that came from someone else (as v1 does).
func markFromInternet(path string) {
	_ = os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644)
}
