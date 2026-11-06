package main

import (
	"strings"
	"testing"
)

// T11.1a left ECHOFOG_PROTOCOL=2 making the server start "without a network", which is nonsense for
// a server. The v1 run path now refuses with a pointer to the v2 command.
func TestProtocolGuard(t *testing.T) {
	v2 := func(k string) string {
		if k == "ECHOFOG_PROTOCOL" {
			return "2"
		}
		return ""
	}
	none := func(string) string { return "" }
	err := protocolGuard("run", v2)
	if err == nil || !strings.Contains(err.Error(), "echofog-node v2") {
		t.Fatalf("run under protocol 2: %v", err)
	}
	for _, other := range []string{"v2", "version", "help", "init"} {
		if err := protocolGuard(other, v2); other == "run" || err != nil {
			t.Errorf("%s under protocol 2 refused: %v", other, err)
		}
	}
	if err := protocolGuard("run", none); err != nil {
		t.Errorf("run without the variable refused: %v", err)
	}
	other := func(k string) string {
		if k == "ECHOFOG_PROTOCOL" {
			return "1"
		}
		return ""
	}
	if err := protocolGuard("run", other); err != nil {
		t.Errorf("run under protocol 1 refused: %v", err)
	}
}
