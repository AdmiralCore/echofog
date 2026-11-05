package dht

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// TestUnavailableCheckKeepsEntry pins the T3.7b interaction: a suspended runtime cannot dial, so
// its pinger reports ErrUnavailable. That is not evidence that the peer is dead.
func TestUnavailableCheckKeepsEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := fixTable(t, nil, &now, 0, &fakePinger{err: ErrUnavailable})

	c := buildCard(t, 800, 1, now, objects.ScopePublic, 0, testAddr("10.70.1.1", 443))
	if !tbl.AddFromSession(c) {
		t.Fatal("card was not added")
	}
	now = now.Add(6 * time.Minute)
	tbl.Prune(context.Background())
	if !tbl.Has(DeviceID(c.DeviceID)) {
		t.Fatal("an entry was removed because the liveness check could not run")
	}
}

// TestUnavailableReplacementKeepsVictim covers the same rule in the full-bucket path.
func TestUnavailableReplacementKeepsVictim(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := fixTable(t, nil, &now, 0, &fakePinger{err: ErrUnavailable})

	const bucket = 0
	for i := uint64(0); i < K; i++ {
		c := cardInBucket(t, testSelf, bucket, i+500, now, testAddr(fmt.Sprintf("10.71.%d.1", i+1), 443))
		if !tbl.AddFromSession(c) {
			t.Fatalf("entry %d was rejected", i)
		}
	}
	candidate := cardInBucket(t, testSelf, bucket, 600, now, testAddr("10.72.1.1", 443))
	if tbl.AddFromCard(candidate) {
		t.Fatal("a candidate was accepted without a failed ping")
	}
	if tbl.Has(DeviceID(candidate.DeviceID)) {
		t.Fatal("an unavailable check replaced an entry")
	}
	if got := tbl.BucketLen(bucket); got != K {
		t.Fatalf("bucket holds %d entries, want %d", got, K)
	}
}
