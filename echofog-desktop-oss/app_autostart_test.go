package main

import (
	"reflect"
	"testing"
)

func TestTakeAutostartFlag(t *testing.T) {
	for _, c := range []struct {
		args, want []string
		found      bool
	}{
		{[]string{"EchoFog.exe"}, []string{"EchoFog.exe"}, false},
		{[]string{"EchoFog.exe", "--autostart"}, []string{"EchoFog.exe"}, true},
		{[]string{"EchoFog.exe", "echofog://join?a=1", "--autostart"}, []string{"EchoFog.exe", "echofog://join?a=1"}, true},
		{[]string{"--autostart"}, []string{"--autostart"}, false}, // the program name is not an argument
		{[]string{"EchoFog.exe", "--autostart=1"}, []string{"EchoFog.exe", "--autostart=1"}, false},
	} {
		got, found := takeAutostartFlag(c.args)
		if found != c.found || !reflect.DeepEqual(got, c.want) {
			t.Errorf("takeAutostartFlag(%q) = %q, %v; want %q, %v", c.args, got, found, c.want, c.found)
		}
	}
}

func TestAutostartFlagPassesTheWatcher(t *testing.T) {
	// the watcher starts the window with appChildFlag in front of the arguments it got
	args := []string{"EchoFog.exe", appChildFlag, autostartFlag}
	args = append(args[:1:1], args[2:]...) // main.go drops appChildFlag first
	if _, found := takeAutostartFlag(args); !found {
		t.Fatal("the window process does not see that the system started the app")
	}
	if linkFromArgs([]string{autostartFlag}) != "" {
		t.Fatal("the flag is taken for a link")
	}
}
