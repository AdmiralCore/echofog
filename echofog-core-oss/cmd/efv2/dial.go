package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"strings"
	"time"

	"github.com/echofog/echofog-core/internal/v2/node"
)

type dialDoc struct {
	PeerNodeID    string  `json:"peer_node_id"`
	PeerDeviceID  string  `json:"peer_device_id"`
	LocalDeviceID string  `json:"local_device_id"`
	Suite         uint16  `json:"suite"`
	Path          string  `json:"path"`
	RTTMs         float64 `json:"rtt_ms"`
	Remote        string  `json:"remote,omitempty"`
	SID           string  `json:"sid"`
}

func cmdDial(args []string) error {
	fs := flag.NewFlagSet("dial", flag.ContinueOnError)
	data := fs.String("data", "", "profile data directory")
	timeout := fs.Duration("timeout", 30*time.Second, "session and RTT timeout")
	if err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *data == "" {
		return errors.New("--data is required")
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: efv2 dial --data DIR <card.json>")
	}
	target := fs.Args()[0]
	if strings.Contains(target, "://") || strings.HasPrefix(target, "efv2:") {
		return errors.New("card links arrive with T4.4/T5.5; pass a card JSON file")
	}
	doc, err := readCardFile(target)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	a, err := openApp(ctx, *data, nil, nil)
	if err != nil {
		return err
	}
	defer a.Close(ctx)
	if err := a.rt.Start(ctx); err != nil {
		return err
	}

	card, _, err := verifyCardDoc(doc, "", a.clock.Now())
	if err != nil {
		return err
	}
	device := node.DeviceID(card.DeviceID)
	localID := a.device.ID()
	peer, sess, err := a.rt.Session(ctx, device, node.DialOpt{Card: card})
	if err != nil {
		return err
	}
	rtt, err := sess.Ping(ctx)
	if err != nil {
		return err
	}
	out := dialDoc{
		PeerNodeID:    hex.EncodeToString(peer.Node[:]),
		PeerDeviceID:  hex.EncodeToString(peer.Device[:]),
		LocalDeviceID: hex.EncodeToString(localID[:]),
		Path:          pathName(peer.Path),
		RTTMs:         float64(rtt) / float64(time.Millisecond),
		SID:           hex.EncodeToString(peer.SID[:]),
	}
	if peer.Remote.IsValid() {
		out.Remote = peer.Remote.String()
	}
	if s, ok := a.sessionStatus(device); ok {
		out.Suite = s.Suite
	}
	if err := saveKnownCard(*data, hex.EncodeToString(device[:]), doc); err != nil {
		return err
	}
	if err := a.WriteStatus(); err != nil {
		return err
	}
	return printJSON(out)
}
