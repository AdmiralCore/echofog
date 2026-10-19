package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/network/access"
	api "github.com/echofog/echofog-core/pkg/api/node"
)

// localProxyAddr forces the SOCKS5 proxy onto loopback so it is never exposed to the network
func localProxyAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

func waitForPeers(h *api.NodeApi, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(h.GetPeers()) > 0 {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// parsePricePerGB turns "0.05" (dollars per GB) into the offer unit, micro-USD per MB.
func parsePricePerGB(s string) (int64, error) {
	usd, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(usd) || usd < 0 || usd > 1000 { // NaN fails every comparison, so it needs its own check
		return 0, fmt.Errorf("%q is not a price in dollars per GB (for example 0.05)", s)
	}
	return int64(usd*1e6/1024 + 0.5), nil
}

// startSharing makes this node an exit. A public one is published to the DHT, and with no manual
// speed it measures against connected peers first; a hidden or isolated one advertises nothing.
func startSharing(h *api.NodeApi, price int64, freeKB, uploadMbps int, gated bool) {
	kbps := uploadMbps * 1000
	if kbps <= 0 && !gated {
		if !waitForPeers(h, 3*time.Minute) {
			fmt.Println("Exit sharing not started: no peers to measure upload speed against. Set --exit-upload-mbps (ECHOFOG_EXIT_UPLOAD_MBPS), e.g. 100.")
			return
		}
		measured, err := h.MeasureUploadCapacity()
		if err != nil {
			fmt.Printf("Exit sharing not started: upload measurement failed (%v). Set --exit-upload-mbps.\n", err)
			return
		}
		kbps = measured
		fmt.Printf("Measured upload speed: %.1f Mbit/s\n", float64(kbps)/1000)
	} else if kbps > 0 {
		if err := h.SetUploadCapacity(kbps); err != nil {
			fmt.Printf("Exit sharing not started: %v\n", err)
			return
		}
	}

	if err := h.StartSharing(api.SharingConfig{PricePerMB: price, FreeTestKB: freeKB, UploadKbps: kbps}); err != nil {
		fmt.Printf("Exit sharing not started: %v\n", err)
		return
	}
	if gated {
		fmt.Println("Exit is on for the people with the invitation link (echofog-node invite); it is not advertised.")
		return
	}
	fmt.Println("Exit node published to the DHT; the advert is refreshed automatically.")
}

// startClientVPN waits for exits to be discovered and then opens the local SOCKS5 proxy through the chosen one
func startClientVPN(h *api.NodeApi, choice, listen string) {
	listen = localProxyAddr(listen)
	if access.IsLink(choice) {
		id, err := h.JoinInvite(choice)
		if err != nil {
			fmt.Printf("SOCKS5 proxy not started: the invitation link: %v\n", err)
			return
		}
		choice = id
	}
	for attempt := 0; attempt < 20; attempt++ {
		time.Sleep(10 * time.Second)
		if choice == "auto" {
			if _, err := h.RefreshExitNodes(); err != nil {
				continue
			}
		}
		if err := h.StartVPNAt(choice, listen); err != nil {
			if attempt == 19 {
				fmt.Printf("SOCKS5 proxy not started: %v\n", err)
			}
			continue
		}
		fmt.Printf("SOCKS5 proxy listening on %s through exit %s\n", listen, choice)
		return
	}
}

// logTopExits prints the best rated exits now and then so operators and users can see the market
func logTopExits(ctx context.Context, h *api.NodeApi) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
		exits := h.ListExitNodes()
		if len(exits) == 0 {
			continue
		}
		fmt.Printf("Exit market: %d live exits\n", len(exits))
		for i, e := range exits {
			if i >= 5 {
				break
			}
			fmt.Printf("  %s $%.3f/GB claimed %d kbps measured %d kbps score %.2f\n", e.PeerID[:12], float64(e.PricePerMB)*1024/1e6, e.ClaimedKbps, e.MeasuredKbps, e.Score)
		}
	}
}
