package service

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"slices"

	"github.com/echofog/echofog-core/internal/v2/frame"
)

// Feature IDs (registries.md §5). A feature changes behaviour only when both sides list it.
const (
	FeaturePathMigration uint16 = frame.FeaturePathMigration // 0x0001
	FeatureMultipath     uint16 = 0x0002                     // reserved
	FeatureIdlePadding   uint16 = 0x0003
	FeatureObservedAddr  uint16 = frame.FeatureObservedAddr // 0x0004
	FeatureEarlyData     uint16 = 0x0005
	FeatureCardPush      uint16 = frame.FeatureCardPush // 0x0006
	FeatureStreamResume  uint16 = 0x0007                // reserved
	FeatureBirthdayPunch uint16 = 0x0008                // reserved
	FeatureWakePush      uint16 = 0x0009                // reserved
	FeatureSPQR          uint16 = 0x0010                // reserved
	FeatureSealedHeader  uint16 = 0x0011                // reserved
	FeatureMLS           uint16 = 0x0012                // reserved

	FeatureExperimentalMin uint16 = 0xf000
	FeatureExperimentalMax uint16 = 0xfeff
)

// IsGrease reports whether a feature ID is a GREASE value 0xXAXA, X in 0..e.
func IsGrease(v uint16) bool { return frame.IsGrease16(v) }

// RandomGrease returns one of the 15 GREASE values at random.
func RandomGrease() uint16 {
	x := uint16(mrand.IntN(15))
	return x<<12 | 0x0a00 | x<<4 | 0x0a
}

// OfferFeatures returns the initiator's HsInit.features: the supported IDs ascending and unique,
// GREASE values in the input dropped, and, with grease, one random GREASE value in its numeric
// place (D18, handshake.md §3.4).
func OfferFeatures(supported []uint16, grease bool) []uint16 {
	out := make([]uint16, 0, len(supported)+1)
	for _, f := range supported {
		if f != 0 && !IsGrease(f) {
			out = append(out, f)
		}
	}
	if grease {
		out = append(out, RandomGrease())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SelectFeatures returns the responder's HsResp.features: offered ∩ supported, GREASE removed,
// ascending. Unknown offered features are ignored, never an error.
func SelectFeatures(offered, supported []uint16) []uint16 {
	var out []uint16
	for _, f := range offered {
		if !IsGrease(f) && slices.Contains(supported, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ErrDowngrade: the responder selected something the initiator did not offer; the initiator
// MUST abort (versioning.md §3.3, handshake.md §7).
var ErrDowngrade = errors.New("service: responder selected something not offered")

// VerifyFeatures is the initiator's check of HsResp.features.
func VerifyFeatures(offered, selected []uint16) error {
	for i, f := range selected {
		if IsGrease(f) || !slices.Contains(offered, f) {
			return fmt.Errorf("%w: feature %#04x", ErrDowngrade, f)
		}
		if i > 0 && selected[i-1] >= f {
			return fmt.Errorf("%w: features not ascending and unique", ErrDowngrade)
		}
	}
	return nil
}
