package profile

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/echofog/echofog-core/internal/v2/carrier"
)

// KeyOf is the store key of a cover's profile for one transport.
func KeyOf(t Transport, host string) []byte {
	return []byte("reality2/cover/" + string(t) + "/" + host)
}

// Save stores the profile in kv (the node store, through the adapter T4.3 provides).
func Save(ctx context.Context, kv carrier.KV, p *CoverProfile) error {
	if p == nil || p.Host == "" || (p.Transport != TCP && p.Transport != QUIC) {
		return fmt.Errorf("profile: a stored profile needs a transport and a host")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return kv.Put(ctx, KeyOf(p.Transport, p.Host), b)
}

// Load returns the stored profile; ok is false when none is stored. A profile written by a
// different schema is an error, not silently ignored: the caller re-measures.
func Load(ctx context.Context, kv carrier.KV, t Transport, host string) (p *CoverProfile, ok bool, err error) {
	b, ok, err := kv.Get(ctx, KeyOf(t, host))
	if err != nil || !ok {
		return nil, false, err
	}
	var out CoverProfile
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, false, fmt.Errorf("profile: stored profile: %w", err)
	}
	if out.Schema != SchemaVersion {
		return nil, false, fmt.Errorf("profile: stored schema %d, this build reads %d", out.Schema, SchemaVersion)
	}
	return &out, true, nil
}
