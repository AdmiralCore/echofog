package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	profile := fs.String("profile", "", "server, desktop or android")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if *profile == "" {
		return errors.New("--profile is required: server, desktop or android")
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	f, err := createIdentity(*data, *profile)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, `mnemonic (shown once; write it down and keep it offline):
%s

node id:   %s
device id: %s
data:      %s
profile:   %s
`, f.Mnemonic, f.NodeID, f.DeviceID, *data, *profile)
	return nil
}
