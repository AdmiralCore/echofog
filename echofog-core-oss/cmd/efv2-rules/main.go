// Command efv2-rules is the offline tool of the rules curator (the owner, H3a): it makes the rules
// key, signs the country rules file and checks one. The key is separate from the release keys and
// lives offline.
//
//	efv2-rules keygen --out BASE                         BASE.key (private seed, hex, 0600), BASE.pub
//	efv2-rules sign   --key K --body body.json --out rules.bin [--prev old.bin] [--seq N]
//	                  [--valid-days 30] [--now UNIX]
//	efv2-rules verify --pub PUBHEX --in rules.bin [--min-seq N] [--now UNIX]
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "efv2-rules:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	return errors.New("not implemented")
}
