package bootstrap_test

import (
	"github.com/echofog/echofog-core/internal/v2/objects"
)

// fakeCard is a card shell: the fake host never inspects it.
func fakeCard() *objects.Card {
	return &objects.Card{DeviceID: [32]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}}
}
