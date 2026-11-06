package media

import (
	"crypto/ed25519"
	"testing"
	"time"
)

type rxHarness struct {
	t       *testing.T
	clk     *fakeClock
	priv    ed25519.PrivateKey
	b       *Batcher
	r       *Receiver
	reports []byte
	units   map[uint32]struct {
		h  Header3
		ct []byte
	}
}

func newRx(t *testing.T, strict bool) *rxHarness {
	t.Helper()
	h := &rxHarness{t: t, clk: newFakeClock(), units: map[uint32]struct {
		h  Header3
		ct []byte
	}{}}
	priv, pub := testMediaKey(5)
	h.priv = priv
	h.b = NewBatcher(priv, h.clk.Now)
	h.r = NewReceiver(ReceiverConfig{
		Now: h.clk.Now, Strict: strict,
		MediaKey: func(sender, gen byte) (ed25519.PublicKey, bool) {
			if sender == 3 && gen == 1 {
				return pub, true
			}
			return nil, false
		},
		Report: func(sender, gen byte, reason error) { h.reports = append(h.reports, sender) },
	})
	return h
}

// send makes unit seq, returns its auth payloads (if the batch closed).
func (h *rxHarness) send(seq uint32) (Header3, []byte, [][]byte) {
	hd, ct := unit3(Kind3Audio, seq)
	h.units[seq] = struct {
		h  Header3
		ct []byte
	}{hd, ct}
	return hd, ct, h.b.Add(hd, ct)
}

func TestUnitsPlayAtOnceAndVerifyOnAuth(t *testing.T) {
	h := newRx(t, false)
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		res := h.r.OnUnit(hd, ct)
		if !res.Play || !res.Forward || res.Verified {
			t.Fatalf("seq %d: %+v, want play+forward, not yet verified (no added playout latency)", seq, res)
		}
		auths = append(auths, a...)
	}
	res, err := h.r.OnAuth(auths[0])
	if err != nil || len(res.Verified) != 5 || res.Forged {
		t.Fatalf("auth: %v %+v", err, res)
	}
	// verified units are marked in the replay window: a copy is a duplicate.
	hd, ct := h.units[2].h, h.units[2].ct
	if r := h.r.OnUnit(hd, ct); !r.Duplicate || r.Play {
		t.Fatalf("copy of a verified unit: %+v", r)
	}
}

func TestLossOfOneCallAuthLosesNothing(t *testing.T) {
	h := newRx(t, true)
	var auths [][]byte
	for seq := uint32(0); seq < 10; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct)
		auths = append(auths, a...)
	}
	if len(auths) != 2 {
		t.Fatalf("%d CALL_AUTH", len(auths))
	}
	// the first CALL_AUTH is lost: the second covers both batches.
	res, err := h.r.OnAuth(auths[1])
	if err != nil || len(res.Verified) != 10 {
		t.Fatalf("verified %d of 10 from one CALL_AUTH: %v", len(res.Verified), err)
	}
}

func TestLostUnitsDoNotBlockOthers(t *testing.T) {
	h := newRx(t, true)
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		if seq != 1 && seq != 3 {
			h.r.OnUnit(hd, ct)
		}
		auths = append(auths, a...)
	}
	res, _ := h.r.OnAuth(auths[0])
	if len(res.Verified) != 3 {
		t.Fatalf("verified %d, want the 3 units that arrived", len(res.Verified))
	}
	// a unit recovered later (NACK, FEC) is verified against the same list for 1 s.
	h.clk.Advance(LateVerify3 - time.Millisecond)
	if r := h.r.OnUnit(h.units[1].h, h.units[1].ct); !r.Verified || !r.Play {
		t.Fatalf("late unit within 1 s: %+v", r)
	}
	h.clk.Advance(2 * time.Millisecond)
	h.r.Sweep()
	if r := h.r.OnUnit(h.units[3].h, h.units[3].ct); r.Verified {
		t.Fatalf("late unit after 1 s verified: %+v", r)
	}
}

func TestStrictModePlaysOnlyVerified(t *testing.T) {
	h := newRx(t, true)
	hd, ct, _ := h.send(0)
	if r := h.r.OnUnit(hd, ct); r.Play || !r.Forward {
		t.Fatalf("strict, unverified: %+v (relays still forward)", r)
	}
}

func TestInsiderCannotImpersonateBeyondOneBatch(t *testing.T) {
	h := newRx(t, false)
	// the insider knows epoch_secret, so it can seal a unit with the victim's header.
	hd, _ := unit3(Kind3Audio, 0)
	forged := make([]byte, 46)
	for i := range forged {
		forged[i] = 0xee
	}
	if r := h.r.OnUnit(hd, forged); !r.Play {
		t.Fatalf("first candidate is played at once: %+v", r)
	}
	// the real unit with the same number arrives.
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		r := h.r.OnUnit(hd, ct)
		if seq == 0 && (r.Play || !r.Forward && false) {
			t.Fatalf("second candidate with the same number must not play: %+v", r)
		}
		auths = append(auths, a...)
	}
	res, err := h.r.OnAuth(auths[0])
	if err != nil {
		t.Fatal(err)
	}
	if !res.Forged {
		t.Fatal("a unit that does not match the verified authenticator is not reported")
	}
	for _, v := range res.Verified {
		if v.Header.Seq == 0 && string(v.Ciphertext) == string(forged) {
			t.Fatal("the forged unit was verified")
		}
	}
	if len(res.Verified) != 5 {
		t.Fatalf("real units verified: %d, want 5", len(res.Verified))
	}
	if !h.r.Muted(3) || len(h.reports) == 0 || h.reports[0] != 3 {
		t.Fatalf("sender not muted/reported: muted=%v reports=%v", h.r.Muted(3), h.reports)
	}
	// the mute holds back unverified units for 2 s only.
	h.clk.Advance(ForgeryMute3 + time.Millisecond)
	if h.r.Muted(3) {
		t.Fatal("mute did not expire")
	}
}

func TestForgedCandidateDoesNotBlockRealUnitInReplayWindow(t *testing.T) {
	h := newRx(t, false)
	hd, _ := unit3(Kind3Audio, 0)
	h.r.OnUnit(hd, []byte("forged forged forged forged!!!!!"))
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct)
		auths = append(auths, a...)
	}
	res, _ := h.r.OnAuth(auths[0])
	got := map[uint32]bool{}
	for _, v := range res.Verified {
		got[v.Header.Seq] = true
	}
	if !got[0] {
		t.Fatal("the real seq 0 was lost to the forged copy")
	}
}

func TestForgedCallAuthSignatureMutesAndReports(t *testing.T) {
	h := newRx(t, false)
	other, _ := testMediaKey(9)
	hd, ct := unit3(Kind3Audio, 0)
	bad := NewBatcher(other, h.clk.Now)
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct = unit3(Kind3Audio, seq)
		auths = append(auths, bad.Add(hd, ct)...)
	}
	res, err := h.r.OnAuth(auths[0])
	if err == nil || len(res.Verified) != 0 {
		t.Fatalf("forged CALL_AUTH accepted: %v %+v", err, res)
	}
	if !h.r.Muted(3) || len(h.reports) != 1 {
		t.Fatalf("muted=%v reports=%v", h.r.Muted(3), h.reports)
	}
}

func TestUnknownSenderKeyIsDropped(t *testing.T) {
	h := newRx(t, false)
	a := &Auth{Kind: 1, Sender: 77, Gen: 1, Cur: [][UnitHash3Len]byte{{1}}}
	if _, err := h.r.OnAuth(a.Sign(h.priv)); err == nil {
		t.Fatal("CALL_AUTH of a sender with no announced media key accepted")
	}
	if h.r.Muted(77) {
		t.Fatal("an unknown sender must not be muted/reported (nothing proves a forgery)")
	}
}

func TestFifthCandidateDoesNotSuppressRealUnit(t *testing.T) {
	h := newRx(t, false)
	hd, _ := unit3(Kind3Audio, 0)
	played := 0
	for i := 0; i < 4; i++ { // four forged copies fill the slots of seq 0
		ct := make([]byte, 40)
		ct[0] = byte(i + 1)
		if r := h.r.OnUnit(hd, ct); r.Forward {
			played++
		}
	}
	if played != 1 {
		t.Fatalf("%d candidates forwarded at once, want only the first", played)
	}
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct) // the real seq 0 is the fifth candidate: no room
		auths = append(auths, a...)
	}
	res, _ := h.r.OnAuth(auths[0])
	for _, v := range res.Verified {
		if v.Header.Seq == 0 {
			t.Fatal("a forged candidate was verified")
		}
	}
	want := NeedUnit{Sender: 3, Gen: 1, Stream: 0, Kind: Kind3Audio, Seq: 0}
	if len(res.Need) != 1 || res.Need[0] != want {
		t.Fatalf("Need = %+v, want the suppressed seq 0 so it can be asked for again", res.Need)
	}
	// the retransmission is verified against the authenticator that already arrived.
	if r := h.r.OnUnit(h.units[0].h, h.units[0].ct); !r.Verified || !r.Play {
		t.Fatalf("retransmitted real unit: %+v", r)
	}
}

func TestFourCandidatesAreKeptNothingLost(t *testing.T) {
	h := newRx(t, false)
	hd, _ := unit3(Kind3Audio, 0)
	for i := 0; i < 3; i++ { // 3 forged + the real one = 4 candidates
		ct := make([]byte, 40)
		ct[0] = byte(i + 1)
		h.r.OnUnit(hd, ct)
	}
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct)
		auths = append(auths, a...)
	}
	res, _ := h.r.OnAuth(auths[0])
	if len(res.Verified) != 5 || len(res.Need) != 0 {
		t.Fatalf("verified %d need %v, want all 5 and nothing to ask for", len(res.Verified), res.Need)
	}
}

func TestLateVerifiedUnitIsNotReplayed(t *testing.T) {
	h := newRx(t, false)
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		_, _, a := h.send(seq) // no unit reaches the receiver before the authenticator
		auths = append(auths, a...)
	}
	h.r.OnAuth(auths[0])
	u := h.units[2]
	if r := h.r.OnUnit(u.h, u.ct); !r.Verified {
		t.Fatalf("late unit: %+v", r)
	}
	if r := h.r.OnUnit(u.h, u.ct); !r.Duplicate || r.Play {
		t.Fatalf("a verified late unit played twice: %+v", r)
	}
}

func TestUnverifiedCandidatesExpire(t *testing.T) {
	h := newRx(t, false)
	var auths [][]byte
	for seq := uint32(0); seq < 5; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct)
		auths = append(auths, a...)
	}
	h.clk.Advance(CandidateTTL3 + time.Millisecond)
	h.r.Sweep()
	res, _ := h.r.OnAuth(auths[0])
	if len(res.Verified) != 0 {
		t.Fatalf("units older than 500 ms without an authenticator verified: %d", len(res.Verified))
	}
}

func TestReceiverStateIsBounded(t *testing.T) {
	h := newRx(t, false)
	// a flood of numbers from an insider does not grow memory without bound.
	for seq := uint32(0); seq < 200000; seq++ {
		hd, _ := unit3(Kind3Audio, seq)
		h.r.OnUnit(hd, []byte{1, 2, 3})
	}
	h.clk.Advance(time.Second)
	h.r.Sweep()
	// the real stream still works afterwards.
	var auths [][]byte
	for seq := uint32(300000); seq < 300005; seq++ {
		hd, ct, a := h.send(seq)
		h.r.OnUnit(hd, ct)
		auths = append(auths, a...)
	}
	res, _ := h.r.OnAuth(auths[0])
	if len(res.Verified) != 5 {
		t.Fatalf("after a flood: verified %d of 5", len(res.Verified))
	}
}

func BenchmarkFiftyAudioSenders(b *testing.B) {
	// One second of a 50-participant call: every sender sends 50 audio units (20 ms frames) and
	// 10 CALL_AUTH; one receiver seals-checks, hashes and verifies all of it.
	const senders = 50
	clk := newFakeClock()
	privs := make([]ed25519.PrivateKey, senders)
	pubs := make([]ed25519.PublicKey, senders)
	batchers := make([]*Batcher, senders)
	for i := range privs {
		privs[i], pubs[i] = testMediaKey(byte(i + 1))
		batchers[i] = NewBatcher(privs[i], clk.Now)
	}
	rx := NewReceiver(ReceiverConfig{Now: clk.Now,
		MediaKey: func(sender, gen byte) (ed25519.PublicKey, bool) { return pubs[sender], true }})
	key := testKey(3)
	plain := make([]byte, 60)
	b.ReportAllocs()
	b.ResetTimer()
	seq := uint32(0)
	for n := 0; n < b.N; n++ {
		for s := 0; s < senders; s++ {
			for u := 0; u < 50; u++ {
				h := Header3{Kind: Kind3Audio, Sender: byte(s), Gen: 1, Seq: seq + uint32(u)}
				ct, _ := Seal3(h, key, plain)
				auths := batchers[s].Add(h, ct)
				rx.OnUnit(h, ct)
				for _, a := range auths {
					rx.OnAuth(a)
				}
			}
		}
		seq += 50
		clk.Advance(time.Second)
		rx.Sweep()
	}
}
