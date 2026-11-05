package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"
)

func cmdEcho(args []string) error {
	fs := flag.NewFlagSet("echo", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	cardPath := fs.String("card", "", "peer card JSON file (default: the card `dial` remembered)")
	echoBytes := byteSize(1 << 30)
	fs.Var(&echoBytes, "bytes", "bytes to send through the echo stream (default 1GiB)")
	datagrams := fs.Int("datagrams", 100, "request/reply datagrams to exchange")
	timeout := fs.Duration("timeout", 10*time.Minute, "whole echo timeout")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: efv2 echo --data DIR <device-id> [--card card.json]")
	}
	device := fs.Args()[0]
	if _, err := parseDeviceID(device); err != nil {
		return err
	}
	path := *cardPath
	if path == "" {
		path = knownCardPath(*data, device)
	}
	doc, err := readCardFile(path)
	if err != nil {
		if *cardPath == "" {
			return fmt.Errorf("%w (pass --card, or run `efv2 dial` first)", err)
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	a, err := openApp(ctx, *data, nil, nil)
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	if _, _, err := verifyCardDoc(doc, device, a.clock.Now()); err != nil {
		return err
	}
	if err := a.rt.Start(ctx); err != nil {
		return err
	}
	res, err := a.Echo(ctx, doc, int64(echoBytes), *datagrams)
	if err != nil {
		return err
	}
	if err := a.WriteStatus(); err != nil {
		return err
	}
	return printJSON(res)
}
