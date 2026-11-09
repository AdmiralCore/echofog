//go:build race

package callnet_test

// raceOn: under the race detector the stand only smoke-tests; timing limits mean nothing there.
const raceOn = true
