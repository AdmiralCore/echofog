package main

import (
	"reflect"
	"testing"
)

func TestTakeProtocolFlag(t *testing.T) {
	cases := []struct {
		in   []string
		rest []string
		p    int
	}{
		{[]string{"app"}, []string{"app"}, 0},
		{[]string{"app", "--protocol", "2"}, []string{"app"}, 2},
		{[]string{"app", "--protocol=2", "echofog://join"}, []string{"app", "echofog://join"}, 2},
		{[]string{"app", "--protocol=1"}, []string{"app"}, 1},
		{[]string{"app", "--protocol=3"}, []string{"app"}, 0},
		{[]string{"app", "--protocol=x"}, []string{"app"}, 0},
		{[]string{"app", "--protocol"}, []string{"app", "--protocol"}, 0},
		{[]string{"--protocol", "2"}, []string{"--protocol", "2"}, 0}, // the program name is never a flag
	}
	for _, c := range cases {
		rest, p := takeProtocolFlag(c.in)
		if p != c.p || !reflect.DeepEqual(rest, c.rest) {
			t.Fatalf("%v: got %v, %d; want %v, %d", c.in, rest, p, c.rest, c.p)
		}
	}
}
