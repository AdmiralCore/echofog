package node

import (
	"errors"
	"fmt"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// maxIngestCard bounds a card handed in from outside (a card is a few kilobytes).
const maxIngestCard = 64 << 10

// IngestPeerCard puts a card a contact sent into the card book (T11.7). The card is verified here
// (objects.VerifyCard against the runtime's clock); a card that does not verify is refused and the
// book does not change.
func (r *Runtime) IngestPeerCard(envelope []byte) error {
	if len(envelope) == 0 || len(envelope) > maxIngestCard {
		return errors.New("node: card of a wrong size")
	}
	c, err := objects.VerifyCard(envelope, r.clock.Now())
	if err != nil {
		return fmt.Errorf("node: card: %w", err)
	}
	if _, err := r.cardBook.Add(c); err != nil {
		return fmt.Errorf("node: card book: %w", err)
	}
	return nil
}
