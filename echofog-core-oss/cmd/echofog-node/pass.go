package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	api "github.com/echofog/echofog-core/pkg/api/node"
)

// passCmd manages the passes of a running hidden or isolated node by hand, over its local status
// endpoint (the operator API does the same for a bot or a website):
//
//	echofog-node pass list
//	echofog-node pass new --days 30 --devices 2 --gb 100 --label "Anna"
//	echofog-node pass show ID
//	echofog-node pass extend ID --days 30 [--gb 50] [--reset]
//	echofog-node pass revoke ID
//	echofog-node pass delete ID
func passCmd(args []string) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	f := flag.NewFlagSet("pass "+sub, flag.ExitOnError)
	addr := f.String("status-addr", getEnv("ECHOFOG_STATUS_ADDR", "127.0.0.1:7070"), "status endpoint of the running node")
	var req api.PassRequest
	f.StringVar(&req.Label, "label", "", "your note on the pass: a name, an order number")
	f.Float64Var(&req.Days, "days", 0, "term in days (new: 0 = never ends; extend: days added)")
	f.IntVar(&req.Devices, "devices", 0, "how many devices may use it (new: 0 = 2)")
	f.Float64Var(&req.GB, "gb", 0, "traffic through the exit, in GB (new: 0 = no limit; extend: GB added)")
	f.BoolVar(&req.ResetUsage, "reset", false, "extend: count the traffic from zero again (a monthly allowance)")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), `Usage: echofog-node pass <list|new|show|extend|revoke|delete> [ID] [flags]

Passes are personal invitation links of a hidden or isolated node: each with its own term, number
of devices and, if you want, a traffic allowance. Make one when somebody pays (or for a friend),
extend it when they pay again, revoke it to take them off. Their app joins with the link like any
invitation. A bot or a website does the same over the operator API, see docs/operator-api.md.`)
		f.PrintDefaults()
	}
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	_ = f.Parse(args)
	if id == "" && f.NArg() > 0 {
		id = f.Arg(0)
	}
	base := "http://" + *addr + "/api/v1/passes"
	client := &http.Client{Timeout: 10 * time.Second}
	call := func(method, url string, body any, out any) bool {
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		r, _ := http.NewRequest(method, url, rd)
		r.Header.Set(inviteHeader, "1")
		r.Header.Set("Content-Type", "application/json")
		res, err := client.Do(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "the node does not answer on %s: is it running (echofog-node run)? %v\n", *addr, err)
			return false
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		if res.StatusCode >= 300 {
			var e struct{ Error string }
			if json.Unmarshal(raw, &e) == nil && e.Error != "" {
				fmt.Fprintln(os.Stderr, e.Error)
			} else {
				fmt.Fprintln(os.Stderr, strings.TrimSpace(string(raw)))
			}
			return false
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				fmt.Fprintln(os.Stderr, "unreadable answer from the node")
				return false
			}
		}
		return true
	}
	needID := func() bool {
		if id == "" {
			fmt.Fprintln(os.Stderr, "which pass? give its ID (echofog-node pass list)")
			return false
		}
		return true
	}

	var p api.PassView
	switch sub {
	case "list":
		var list []api.PassView
		if !call(http.MethodGet, base, nil, &list) {
			return 1
		}
		if len(list) == 0 {
			fmt.Println("No passes yet. Make one: echofog-node pass new --days 30 --label \"name\"")
			return 0
		}
		for _, p := range list {
			printPassLine(p)
		}
		return 0
	case "new":
		if !call(http.MethodPost, base, req, &p) {
			return 1
		}
	case "show":
		if !needID() || !call(http.MethodGet, base+"/"+id, nil, &p) {
			return 1
		}
	case "extend":
		if !needID() || !call(http.MethodPost, base+"/"+id+"/extend", req, &p) {
			return 1
		}
	case "revoke":
		if !needID() || !call(http.MethodPost, base+"/"+id+"/revoke", nil, &p) {
			return 1
		}
	case "delete":
		if !needID() || !call(http.MethodDelete, base+"/"+id, nil, nil) {
			return 1
		}
		fmt.Println("Deleted; its devices are off.")
		return 0
	default:
		f.Usage()
		return 2
	}
	printPass(p)
	return 0
}

func passTerm(p api.PassView) string {
	if p.Expires == 0 {
		return "no end"
	}
	return "until " + time.Unix(p.Expires, 0).Format("2006-01-02 15:04")
}

func passTraffic(p api.PassView) string {
	if p.QuotaBytes == 0 {
		return human(uint64(p.UsedBytes)) + " used"
	}
	return human(uint64(p.UsedBytes)) + " of " + human(uint64(p.QuotaBytes))
}

func printPassLine(p api.PassView) {
	fmt.Printf("%s  %-8s %-24s %d/%d devices  %-22s %s\n", p.ID, p.State, passTerm(p), len(p.Devices), p.MaxDevices, passTraffic(p), p.Label)
}

func printPass(p api.PassView) {
	fmt.Printf("Pass %s (%s)  %s\n", p.ID, p.State, p.Label)
	fmt.Printf("Term:     %s\nDevices:  %d of %d\nTraffic:  %s\n", passTerm(p), len(p.Devices), p.MaxDevices, passTraffic(p))
	for _, d := range p.Devices {
		state := ""
		if d.Connected {
			state = "  connected"
		}
		fmt.Printf("  %s  joined %s%s\n", d.PeerID, time.Unix(d.JoinedAt, 0).Format("2006-01-02 15:04"), state)
	}
	if p.Link != "" {
		fmt.Printf("Link (give it to that person only):\n%s\n", p.Link)
		if p.WebLink != "" {
			fmt.Printf("The same as a page messengers open:\n%s\n", p.WebLink)
		}
	}
}
