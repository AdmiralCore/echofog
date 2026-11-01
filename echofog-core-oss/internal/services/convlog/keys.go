package convlog

import (
	"encoding/binary"

	"github.com/echofog/echofog-core/internal/storage"
)

// Keys of the conversation log (all under the sealed prefix "cl/"). bc is the blinded
// conversation id, ba a blinded author; see docs/design/conversation-log.md section 3.
const (
	pfxEvent  = "cl/e/" // bc/ba/seq       -> event bytes (source of truth, sync ranges)
	pfxFork   = "cl/f/" // bc/id           -> event bytes of an equivocating author
	pfxIndex  = "cl/i/" // bc/id           -> {author, seq}: the event was applied
	pfxOrder  = "cl/o/" // bc/^lamport/id  -> id: history, newest first
	pfxView   = "cl/s/" // bc/id           -> View
	pfxThread = "cl/t/" // bc/root/lamport/id -> id: a thread, oldest first
	pfxPin    = "cl/p/" // bc/id           -> id: pinned messages
	pfxMeta   = "cl/v/" // bc              -> Meta
	pfxMember = "cl/m/" // bc/lamport/id   -> event bytes: a group's membership history
	pfxChan   = "cl/c/" // bc/channel      -> Channel
	pfxChanO  = "cl/q/" // bc/channel/^lamport/id -> id: a channel's history, newest first
	pfxProf   = "cl/g/" // bc              -> Profile: a group's picture
)

func (l *Log) bc(c ConvID) string { return storage.BlindID(l.st, "conv:"+c.String()) }

func (l *Log) ba(author string) string { return storage.BlindID(l.st, "author:"+author) }

func be64(x uint64) []byte { return binary.BigEndian.AppendUint64(nil, x) }

func key(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func bs(x string) []byte { return []byte(x) }

func eventPrefix(bc, ba string) []byte { return key(bs(pfxEvent + bc + "/" + ba + "/")) }

func eventKey(bc, ba string, seq uint64) []byte { return key(eventPrefix(bc, ba), be64(seq)) }

func forkKey(bc, id string) []byte { return bs(pfxFork + bc + "/" + id) }

func forkPrefix(bc string) []byte { return bs(pfxFork + bc + "/") }

func indexKey(bc, id string) []byte { return bs(pfxIndex + bc + "/" + id) }

func orderPrefix(bc string) []byte { return bs(pfxOrder + bc + "/") }

// orderKey sorts by Lamport clock, newest first, and breaks ties by event id: the same order on
// every device (the author would be blinded per device, the id is not and names no one).
func orderKey(bc string, lamport uint64, id string) []byte {
	return key(orderPrefix(bc), be64(^lamport), bs("/"+id))
}

func viewKey(bc, id string) []byte { return bs(pfxView + bc + "/" + id) }

func threadPrefix(bc, root string) []byte { return bs(pfxThread + bc + "/" + root + "/") }

func threadKey(bc, root string, lamport uint64, id string) []byte {
	return key(threadPrefix(bc, root), be64(lamport), bs("/"+id))
}

func pinPrefix(bc string) []byte { return bs(pfxPin + bc + "/") }

func pinKey(bc, id string) []byte { return bs(pfxPin + bc + "/" + id) }

func metaKey(bc string) []byte { return bs(pfxMeta + bc) }

func memberPrefix(bc string) []byte { return bs(pfxMember + bc + "/") }

func channelPrefix(bc string) []byte { return bs(pfxChan + bc + "/") }

func channelKey(bc, id string) []byte { return bs(pfxChan + bc + "/" + id) }

func profileKey(bc string) []byte { return bs(pfxProf + bc) }

func chanOrderPrefix(bc, channel string) []byte { return bs(pfxChanO + bc + "/" + channel + "/") }

func chanOrderKey(bc, channel string, lamport uint64, id string) []byte {
	return key(chanOrderPrefix(bc, channel), be64(^lamport), bs("/"+id))
}

func memberKey(bc string, lamport uint64, id string) []byte {
	return key(memberPrefix(bc), be64(lamport), bs("/"+id))
}
