package main

import (
	"errors"
	"flag"
	"fmt"
	"time"
)

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	doc, err := readStatusFile(statusPath(*data))
	if err != nil {
		return err
	}
	doc.Stale = time.Since(doc.UpdatedAt) > 15*time.Second
	return printJSON(doc)
}
