package node

import "errors"

// IngestPeerCard puts a card a contact sent into the card book (T11.7). The card is verified here
// (objects.VerifyCard against the runtime's clock); a card that does not verify is refused and the
// book does not change.
func (r *Runtime) IngestPeerCard(envelope []byte) error {
	return errors.New("node: IngestPeerCard is not built")
}
