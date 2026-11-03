package handshake

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestVerifyVectorCards(t *testing.T) {
	c := loadVector(t, "node-card.json")
	devID := hexField(t, c, "device_id (SHA-256 of the device akey; the card signer)")
	nodeID := hexField(t, c, "node_id (SHA-256 of the account akey)")
	hs := hexField(t, c, "hs_akey")[1:]
	for _, scope := range []string{"public", "peer", "invite"} {
		env := hexField(t, obj(t, c, scope), "envelope")
		info, err := DefaultCardVerifier{}.VerifyCard(env, fixedNow)
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		if !bytes.Equal(info.DeviceID[:], devID) || !bytes.Equal(info.NodeID[:], nodeID) || !bytes.Equal(info.HsKey, hs) {
			t.Fatalf("%s: ids/hs key mismatch", scope)
		}
		if info.Seq != num(t, obj(t, c, scope), "seq") {
			t.Fatalf("%s: seq", scope)
		}
		if len(info.L1Suites) != 2 || info.L1Suites[0] != SuiteXK || info.L1Suites[1] != SuiteXX {
			t.Fatalf("%s: l1_suites %v", scope, info.L1Suites)
		}
		if !bytes.Equal(info.Raw, env) {
			t.Fatalf("%s: raw bytes", scope)
		}
	}
	peer, _ := DefaultCardVerifier{}.VerifyCard(hexField(t, obj(t, c, "peer"), "envelope"), fixedNow)
	if !bytes.Equal(peer.PrevHsKey, hexField(t, c, "prev_hs_akey")[1:]) || peer.Scope != 1 {
		t.Fatal("peer prev_hs/scope")
	}
	inv, _ := DefaultCardVerifier{}.VerifyCard(hexField(t, obj(t, c, "invite"), "envelope"), fixedNow)
	if inv.Scope != 2 || !bytes.Equal(inv.InviteID, hexField(t, obj(t, c, "invite"), "invite_id")) {
		t.Fatal("invite scope/id")
	}
}

func TestCardTimeRules(t *testing.T) {
	c := loadVector(t, "node-card.json")
	env := hexField(t, obj(t, c, "public"), "envelope")
	notAfter := int64(num(t, obj(t, c, "public"), "not_after"))
	if _, err := (DefaultCardVerifier{}).VerifyCard(env, time.Unix(notAfter+61, 0)); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired card: %v", err)
	}
	if _, err := (DefaultCardVerifier{}).VerifyCard(env, time.Unix(notAfter+59, 0)); err != nil {
		t.Fatalf("60 s skew: %v", err)
	}
	// Lifetime above 7 days + 60 s for a public card.
	if _, err := (DefaultCardVerifier{}).VerifyCard(env, time.Unix(notAfter-7*86400-61, 0)); !errors.Is(err, ErrCardLifetime) {
		t.Fatalf("long lifetime: %v", err)
	}
	// The device list (30 days) expires later than the card; an invite card may live 30 days.
	inv := hexField(t, obj(t, c, "invite"), "envelope")
	if _, err := (DefaultCardVerifier{}).VerifyCard(inv, fixedNow); err != nil {
		t.Fatalf("invite: %v", err)
	}
}

func TestCardTamperedSignature(t *testing.T) {
	c := loadVector(t, "node-card.json")
	env := hexField(t, obj(t, c, "public"), "envelope")
	for _, pos := range []int{2, 20, 60, len(env) / 2, len(env) - 1} {
		b := append([]byte(nil), env...)
		b[pos] ^= 1
		if _, err := (DefaultCardVerifier{}).VerifyCard(b, fixedNow); err == nil {
			t.Fatalf("tampered byte %d accepted", pos)
		}
	}
	if _, err := (DefaultCardVerifier{}).VerifyCard(append(env, 0), fixedNow); err == nil {
		t.Fatal("trailing byte accepted")
	}
	if _, err := (DefaultCardVerifier{}).VerifyCard(make([]byte, 4097), fixedNow); err == nil {
		t.Fatal("oversized envelope accepted")
	}
}

// negative.json, case by case.
func TestNegativeVectors(t *testing.T) {
	v := loadVector(t, "negative.json")
	for _, e := range v["non_canonical_bodies"].([]any) {
		m := e.(map[string]any)
		body := hexField(t, m, "body")
		if _, err := parseNodeCard(body); err == nil {
			t.Errorf("NodeCard body %x accepted (%s)", body, m["reason"])
		}
	}
	// The same rules apply to the handshake payloads (one encoding; see the package doc).
	for _, b := range []string{"300108c801", "8800c801", "08c80108c901"} {
		if _, err := ParseHsInit(mustHex(t, b)); err == nil {
			t.Errorf("HsInit %s accepted", b)
		}
		if _, err := ParseHsResp(mustHex(t, b)); err == nil {
			t.Errorf("HsResp %s accepted", b)
		}
	}
	// HsResp field 9 (time) is a known non-repeated field: the unpacked pair is a duplicate.
	if _, err := ParseHsResp(mustHex(t, "08c80148014803")); err == nil {
		t.Error("HsResp duplicate time accepted")
	}

	sig := obj(t, v, "signature_s_not_canonical")
	if _, err := (DefaultCardVerifier{}).VerifyCard(hexField(t, sig, "envelope"), fixedNow); !errors.Is(err, ErrCardSignature) {
		t.Errorf("S + L: %v", err)
	}
	so := obj(t, v, "small_order_signer")
	if _, err := (DefaultCardVerifier{}).VerifyCard(hexField(t, so, "envelope"), fixedNow); !errors.Is(err, ErrCardSignature) {
		t.Errorf("small-order signer: %v", err)
	}
	// "varint": see varint.json invalid. The envelope's body_len is a QUIC varint.
	vi := loadVector(t, "varint.json")
	for _, e := range vi["invalid"].([]any) {
		b := hexField(t, e.(map[string]any), "encoded")
		if _, _, err := readQUICVarint(b); err == nil {
			t.Errorf("varint %x accepted", b)
		}
	}
	for _, e := range vi["valid"].([]any) {
		m := e.(map[string]any)
		b := hexField(t, m, "encoded")
		got, n, err := readQUICVarint(b)
		if err != nil || n != len(b) || float64(got) != m["value"].(float64) {
			// float64 cannot hold the largest values exactly; compare by re-encoding instead.
			if err != nil || n != len(b) || !bytes.Equal(appendQUICVarint(nil, got), b) {
				t.Errorf("varint %x: %d %v", b, got, err)
			}
		}
	}
	// noise3_init_out_of_window and reality2_token_wrong_address are L0 cases (wire.md §2–§3):
	// they never reach the handshake and are tested with the L0 suites.
}

func TestDeviceListMustListSigner(t *testing.T) {
	// A card whose embedded device list does not list the signer is invalid.
	k := newTestIdentity(t, 1)
	other := newTestIdentity(t, 2)
	card := k.cardWithDevices(t, other.deviceList(t, fixedTime), CardOptions{})
	if _, err := (DefaultCardVerifier{}).VerifyCard(card, fixedNow); !errors.Is(err, ErrCardDevices) {
		t.Fatalf("foreign device list: %v", err)
	}
	if _, err := (DefaultCardVerifier{}).VerifyCard(k.card(t, CardOptions{}), fixedNow); err != nil {
		t.Fatalf("own card: %v", err)
	}
}
