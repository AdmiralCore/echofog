package main

import (
	"encoding/binary"
	"errors"
)

// checkHello is the responder's HS_HELLO check (handshake.md §3.1): first every malformation,
// then the suite rule against its own supported list (most preferred first).
func checkHello(b []byte, supported []uint16) error {
	if len(b) < 1 {
		return errors.New("hello: empty")
	}
	cnt := int(b[0])
	if cnt < 1 || cnt > 8 {
		return errors.New("hello: count outside 1..8")
	}
	if len(b) < 1+2*cnt+3 {
		return errors.New("hello: short")
	}
	offered := make([]uint16, cnt)
	seen := map[uint16]bool{}
	greases, firstReal := 0, uint16(0)
	for i := range offered {
		v := binary.BigEndian.Uint16(b[1+2*i:])
		if seen[v] {
			return errors.New("hello: repeated value in offered")
		}
		seen[v] = true
		offered[i] = v
		if isGrease(v) {
			greases++
		} else if firstReal == 0 {
			firstReal = v
		}
	}
	if greases > 1 {
		return errors.New("hello: more than one GREASE value")
	}
	if greases == cnt {
		return errors.New("hello: offered is only GREASE")
	}
	off := 1 + 2*cnt
	chosen := binary.BigEndian.Uint16(b[off:])
	flags := b[off+2]
	want := off + 3
	if flags&0x01 != 0 {
		want += 16 + 8 // cookie, nonce
	}
	if len(b) != want {
		return errors.New("hello: trailing or missing bytes")
	}
	if !seen[chosen] {
		return errors.New("hello: chosen not in offered")
	}
	if isGrease(chosen) {
		return errors.New("hello: GREASE as chosen")
	}
	if chosen != firstReal {
		return errors.New("hello: chosen is not the first non-GREASE entry")
	}
	// Suite rule: chosen must be our own most preferred suite in offered ∩ supported.
	for _, s := range supported {
		if seen[s] {
			if s != chosen {
				return errors.New("hello: suite (not our most preferred)")
			}
			return nil
		}
	}
	return errors.New("hello: suite (no common suite)")
}
