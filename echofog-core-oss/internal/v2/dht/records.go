package dht

import (
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/objects"
)

// acceptRecord verifies a record a node sent for (label, signer) before the client uses it: the
// signature and lifetime through the label's own verifier, the label, and the signer the key was
// derived from. A record of another signer under the asked key, or with another label, is never
// the answer, however valid it is (a node could otherwise answer with someone else's object).
// The release manifest has a day key independent of the signer, so any signer passes there; its
// threshold signatures are the caller's check (objects.md §9).
func acceptRecord(label string, signer [32]byte, raw []byte, now time.Time) (*objects.Envelope, error) {
	if len(raw) == 0 || len(raw) > maxRecordBytes {
		return nil, fmt.Errorf("dht: record of %d bytes", len(raw))
	}
	var env *objects.Envelope
	var err error
	switch label {
	case objects.LabelCard:
		var card *objects.Card
		if card, err = objects.VerifyCard(raw, now); err == nil {
			if !card.Publishable() {
				return nil, fmt.Errorf("dht: card of a scope that is never stored")
			}
			env = card.Env
		}
	case objects.LabelPrekeys:
		var p *objects.Prekeys
		if p, err = objects.VerifyPrekeys(raw, now); err == nil {
			env = p.Env
		}
	case objects.LabelDevices:
		var l *objects.DeviceList
		if l, err = objects.VerifyDeviceList(raw, now); err == nil {
			env = l.Env
		}
	case objects.LabelMailboxHint:
		var h *objects.MailboxHint
		if h, err = objects.VerifyMailboxHint(raw, now); err == nil {
			env = h.Env
		}
	case objects.LabelName:
		return nil, ErrNotStorable
	default:
		env, err = objects.Verify(raw, now)
	}
	if err != nil {
		return nil, err
	}
	if env.Label != label {
		return nil, fmt.Errorf("dht: record has label %q, asked %q", env.Label, label)
	}
	if label != objects.LabelRelease && env.SignerID() != signer {
		return nil, fmt.Errorf("dht: record is signed by another signer than the key's")
	}
	return env, nil
}

// Client-side decoders of the answers of dht/2 (services.md §2). Each checks the frame type and
// that the answer echoes the request ID (K4: an answer is bound to its request); an ERROR frame
// becomes a RemoteError. Nothing here trusts the content: cards and records are verified by the
// caller.

func decodeErr(req, typ uint64, payload []byte) error {
	if typ == TypeError {
		m, err := UnmarshalError(payload)
		if err != nil {
			return err
		}
		if m.Req != req {
			return fmt.Errorf("dht: ERROR answers request %d, asked %d", m.Req, req)
		}
		return &RemoteError{Code: m.Code}
	}
	return fmt.Errorf("dht: unexpected frame type 0x%x", typ)
}

// DecodeNodes decodes the answer to FIND_NODE.
func DecodeNodes(req, typ uint64, payload []byte) ([][]byte, error) {
	if typ != TypeNodes {
		return nil, decodeErr(req, typ, payload)
	}
	m, err := UnmarshalNodes(payload)
	if err != nil {
		return nil, err
	}
	if m.Req != req {
		return nil, fmt.Errorf("dht: NODES answers request %d, asked %d", m.Req, req)
	}
	return m.Cards, nil
}

// DecodeValue decodes the answer to FIND_VALUE: the record, or the closer cards.
func DecodeValue(req, typ uint64, payload []byte) ([]byte, [][]byte, error) {
	if typ != TypeValue {
		return nil, nil, decodeErr(req, typ, payload)
	}
	m, err := UnmarshalValue(payload)
	if err != nil {
		return nil, nil, err
	}
	if m.Req != req {
		return nil, nil, fmt.Errorf("dht: VALUE answers request %d, asked %d", m.Req, req)
	}
	return m.Record, m.Cards, nil
}

// DecodeStored decodes the answer to STORE: the STORED status.
func DecodeStored(req, typ uint64, payload []byte) (uint32, error) {
	if typ != TypeStored {
		return 0, decodeErr(req, typ, payload)
	}
	m, err := UnmarshalStored(payload)
	if err != nil {
		return 0, err
	}
	if m.Req != req {
		return 0, fmt.Errorf("dht: STORED answers request %d, asked %d", m.Req, req)
	}
	return m.Status, nil
}
