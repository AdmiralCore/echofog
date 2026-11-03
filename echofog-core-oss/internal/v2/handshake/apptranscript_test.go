package handshake

import (
	"crypto/mlkem"
	"crypto/mlkem/mlkemtest"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestApplicationTranscripts drives the real Initiator and Responder, with every payload and
// card check of handshake.md §7, from the vector keys, and requires the normative message bytes
// and final values of handshake-transcript.json.
func TestApplicationTranscripts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(vectorDir, transcriptFile))
	if err != nil {
		t.Fatal(err)
	}
	var want tFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	byName := map[string]tTranscript{}
	for _, tr := range want.Transcripts {
		byName[tr.Name] = tr
	}

	hsv := loadVector(t, "handshake.json")
	card := loadVector(t, "node-card.json")
	link := loadVector(t, "link.json")
	in := obj(t, hsv, "transcript_inputs")
	env := func(scope string) []byte { return hexField(t, obj(t, card, scope), "envelope") }
	seq := func(scope string) uint64 { return num(t, obj(t, card, scope), "seq") }
	verify := func(scope string) *CardInfo {
		info, err := (DefaultCardVerifier{}).VerifyCard(env(scope), fixedNow)
		if err != nil {
			t.Fatalf("%s card: %v", scope, err)
		}
		return info
	}
	var devID [32]byte
	copy(devID[:], hexField(t, card, "device_id (SHA-256 of the device akey; the card signer)"))
	secret := hexField(t, obj(t, link, "invitation"), "access_secret")
	m := hexField(t, in, "encaps_randomness_m")
	clock := func() time.Time { return fixedNow }
	services := []ServiceOffer{}
	for id := uint32(1); id <= 8; id++ {
		services = append(services, ServiceOffer{ID: id, Versions: []uint32{2}})
	}

	cases := []struct {
		name        string
		remote      string // responder card the initiator holds; "" = bare-address XX
		own         string // initiator card for P3
		hidden      bool
		greaseSuite uint16
	}{
		{"XKhfs", "public", "initiator", false, 0},
		{"XKhfs-access-proof", "invite", "initiator", true, 0x0a0a},
		{"XXhfs", "", "initiator_throwaway", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := byName[c.name]
			ic := InitiatorConfig{
				Static: xKey(t, hexField(t, in, "initiator_s_sk")), Card: env(c.own),
				Features: []uint32{1, 3, 4, 5}, Services: services, Now: clock, Grease: true,
				testEphemeral: xKey(t, hexField(t, in, "initiator_e_sk")),
				testKEMSeed:   hexField(t, in, "initiator_e1_seed_dz"),
			}
			if c.remote != "" {
				ic.Remote = verify(c.remote)
			}
			if c.hidden {
				ic.AccessSecret = secret
			}
			rc := ResponderConfig{
				Static: xKey(t, hexField(t, card, "hs_sk")), DeviceID: devID,
				OwnCards: OwnCards{
					Current: map[uint32]OwnCard{0: {seq("public"), env("public")}, 1: {seq("peer"), env("peer")}},
					Issued:  map[uint64]uint32{seq("public"): 0, seq("peer"): 1, seq("invite"): 2},
				},
				Hidden: c.hidden, Passes: passMap{string(PassID(secret)): secret},
				Features: []uint32{1, 3, 4, 5, 6},
				Services: []ServiceOffer{{ID: 1, Versions: []uint32{2}}, {ID: 2, Versions: []uint32{2}}, {ID: 3, Versions: []uint32{2}}},
				Now:      clock, Observed: []byte{4, 192, 0, 2, 33, 0xc8, 0x22},
				testEphemeral: xKey(t, hexField(t, in, "responder_e_sk")),
				testEncap: func(ek *mlkem.EncapsulationKey768) ([]byte, []byte, error) {
					return mlkemtest.Encapsulate768(ek, m)
				},
			}
			ini, err := NewInitiator(ic, hexField(t, obj(t, hsv, "prologue_xk"), "cb"))
			if err != nil {
				t.Fatal(err)
			}
			ini.greaseSuite, ini.greaseFeature, ini.greaseService = c.greaseSuite, 0x2a2a, 0x3a3a
			hello, m1, err := ini.Start()
			if err != nil {
				t.Fatal(err)
			}
			if got := hx(Prologue(ini.cb, ini.rid, hello)); got != tr.Inputs.Prologue {
				t.Fatalf("prologue %s, want %s", got, tr.Inputs.Prologue)
			}
			if hx(m1) != tr.Messages[0].Bytes {
				t.Fatal("message 1 differs")
			}
			res := NewResponder(rc)
			rep := res.HandleFirst(ini.cb, hello, m1, fixedNow)
			if rep.Action != ActSend || rep.FrameType != FrameMsg {
				t.Fatalf("responder: %v %v", rep.Action, rep.Err)
			}
			if hx(rep.Payload) != tr.Messages[1].Bytes {
				t.Fatal("message 2 differs")
			}
			m3, si, err := ini.HandleMsg2(rep.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if hx(m3) != tr.Messages[2].Bytes {
				t.Fatal("message 3 differs")
			}
			sr, err := res.HandleFinish(m3)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []*Session{si, sr} {
				if hx(s.H) != tr.Final.H || hx(s.KI2R.Key()) != tr.Final.KI2R || hx(s.KR2I.Key()) != tr.Final.KR2I ||
					hx(s.SessionSecret) != tr.Final.SessionSecret || hx(s.SID) != tr.Final.SID {
					t.Fatal("final values differ")
				}
			}
			if si.Peer.DeviceID != devID || sr.Peer.Seq != verify(c.own).Seq {
				t.Fatal("peer identities")
			}
			if (c.remote == "") != si.Unverified {
				t.Fatal("unverified flag")
			}
		})
	}
}
