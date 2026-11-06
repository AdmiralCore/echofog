// Command efv2-release is the offline tool of the project maintainer for release manifests
// (objects.md §9): keygen, assemble, cosign, sign, verify. Release keys are made and used on
// offline media; the tool refuses to write a private key inside a git work tree.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errNotImplemented = errors.New("efv2-release: not implemented")

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "efv2-release:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error { return errNotImplemented }
