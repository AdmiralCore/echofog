package dht

import (
	"context"
	"fmt"
	"sync"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// Resolved is what a contact's node ID leads to: the account's device list and, for the devices on
// it, their public cards and prekey bundles.
type Resolved struct {
	Devices *objects.DeviceList
	Cards   []*objects.Card
	Prekeys []*objects.Prekeys
}

// Resolve looks up the account's devices/2 record under its node ID, then each listed device's
// card/2 and prekeys/2 (dht.md §2). Only the device list may be signed by the account; a prekey
// bundle counts only if the list vouches for the device that signed it, and a card only if it is
// the card of the device it was asked for. It fails when nothing usable was found.
func (c *Client) Resolve(ctx context.Context, node [32]byte) (*Resolved, error) {
	env, err := c.Get(ctx, objects.LabelDevices, node)
	if err != nil {
		return nil, fmt.Errorf("dht: devices of %x: %w", node[:4], err)
	}
	list, err := objects.VerifyDeviceList(env.Raw, c.now())
	if err != nil {
		return nil, err
	}
	if list.NodeID != node {
		return nil, fmt.Errorf("dht: device list of another account")
	}

	devices := list.Devices()
	res := &Resolved{Devices: list}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, d := range devices {
		k, err := keys.Parse(d.GetKey())
		if err != nil {
			continue
		}
		id := k.ID()
		wg.Add(2)
		go func() {
			defer wg.Done()
			e, err := c.Get(ctx, objects.LabelCard, id)
			if err == nil {
				var card *objects.Card
				if card, err = objects.VerifyCard(e.Raw, c.now()); err == nil && card.DeviceID != id {
					err = fmt.Errorf("dht: card of another device")
				} else if err == nil {
					mu.Lock()
					res.Cards = append(res.Cards, card)
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			e, err := c.Get(ctx, objects.LabelPrekeys, id)
			if err == nil {
				var p *objects.Prekeys
				if p, err = objects.VerifyPrekeys(e.Raw, c.now()); err == nil && !p.VouchedBy(list) {
					err = fmt.Errorf("dht: prekeys of a device the list does not name")
				} else if err == nil {
					mu.Lock()
					res.Prekeys = append(res.Prekeys, p)
					mu.Unlock()
					return
				}
			}
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(res.Cards) == 0 && len(res.Prekeys) == 0 {
		if firstErr == nil {
			firstErr = ErrNotFound
		}
		return nil, fmt.Errorf("dht: nothing found for the devices of %x: %w", node[:4], firstErr)
	}
	return res, nil
}
