package ratelimit

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	now := time.Now()
	b := NewBucket(10, 20)
	for i := 0; i < 20; i++ {
		if !b.AllowAt(1, now) {
			t.Fatalf("token %d of the burst refused", i)
		}
	}
	if b.AllowAt(1, now) {
		t.Fatal("an empty bucket must refuse")
	}
	if !b.AllowAt(10, now.Add(time.Second)) {
		t.Fatal("a second refills rate tokens")
	}
	if !NewBucket(0, 0).AllowAt(1e12, now) {
		t.Fatal("rate 0 is no limit")
	}
}

func TestBucketWait(t *testing.T) {
	b := NewBucket(1000, 1000)
	if d := b.Wait(1000); d != 0 {
		t.Fatalf("the burst needs no wait, got %v", d)
	}
	if d := b.Wait(500); d < 400*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("500 tokens at 1000/s past the burst: wait about 500ms, got %v", d)
	}
}

func TestKeyedBoundedAndPruned(t *testing.T) {
	k := NewKeyed[int](1, 2, 10)
	now := time.Now()
	if !k.AllowAt(1, 2, now) || k.AllowAt(1, 1, now) {
		t.Fatal("each key has its own burst of 2")
	}
	if !k.AllowAt(2, 1, now) {
		t.Fatal("another key is not affected")
	}
	for i := 100; i < 200; i++ {
		k.AllowAt(i, 1, now)
	}
	if k.Len() > 10 {
		t.Fatalf("the table holds %d keys, more than its 10", k.Len())
	}
	k.mu.Lock()
	k.pruneLocked(now.Add(time.Hour))
	k.mu.Unlock()
	if k.Len() != 0 {
		t.Fatalf("keys whose bucket refilled are forgotten, %d left", k.Len())
	}
}

func TestAddrKeys(t *testing.T) {
	for _, a := range []string{"127.0.0.1:1", "10.1.2.3:5", "192.168.0.4", "[::1]:3", "fe80::1", "garbage"} {
		if ip, sub := AddrKeys(a); ip != "" || sub != "" {
			t.Fatalf("%s must not be limited, got %q %q", a, ip, sub)
		}
	}
	if ip, sub := AddrKeys("203.0.113.5:443"); ip != "203.0.113.5" || sub != "203.0.113.0/24" {
		t.Fatalf("IPv4 keys: %q %q", ip, sub)
	}
	ip1, sub1 := AddrKeys("[2001:db8:1:2:aaaa::1]:443")
	ip2, sub2 := AddrKeys("[2001:db8:1:2:bbbb::9]:443")
	ip3, sub3 := AddrKeys("[2001:db8:1:3::1]:443")
	if ip1 != ip2 || sub1 != sub2 {
		t.Fatal("addresses of one IPv6 /64 count as one")
	}
	if ip1 == ip3 || sub1 != sub3 {
		t.Fatal("another /64 of the same /48 is another address but the same subnet")
	}
}
