package profile

import (
	"fmt"
	"slices"
)

// SchemaVersion is the version of the stored CoverProfile layout.
const SchemaVersion = 1

// Slot is one position of a server flight: the same position in every sample has the same kind
// and the length lies in [Min, Max] over the samples.
type Slot struct {
	Kind string `json:"kind"`
	Unit int    `json:"unit,omitempty"`
	Min  int    `json:"min"`
	Max  int    `json:"max"`
}

// CoverProfile is what a cover (or our own server) showed over Samples connections.
type CoverProfile struct {
	Schema    int       `json:"schema"`
	Transport Transport `json:"transport"`
	Host      string    `json:"host,omitempty"`
	SNI       string    `json:"sni,omitempty"`
	Addr      string    `json:"addr,omitempty"`
	// MeasuredAt is unix seconds.
	MeasuredAt int64 `json:"measured_at,omitempty"`
	Samples    int   `json:"samples"`

	Version      uint16   `json:"version"`
	Cipher       uint16   `json:"cipher"`
	Group        uint16   `json:"group"`
	HRR          bool     `json:"hrr,omitempty"`
	ALPN         string   `json:"alpn,omitempty"`
	ExtOrder     []uint16 `json:"ext_order,omitempty"`
	SessionIDLen int      `json:"session_id_len,omitempty"`
	SCIDLen      int      `json:"scid_len,omitempty"`
	CCS          bool     `json:"ccs,omitempty"`
	Flight       []Slot   `json:"flight,omitempty"`
	// Post are the post-handshake records or packets; on TLS 1.3 over TCP, the tickets.
	Post []Slot `json:"post,omitempty"`
	// Decrypted reports that Inner and Tickets come from decrypted handshakes (TCP).
	Decrypted bool `json:"decrypted,omitempty"`
	// Inner are the encrypted handshake messages up to Finished, Tickets the NewSessionTickets.
	Inner   []Slot `json:"inner,omitempty"`
	Tickets []Slot `json:"tickets,omitempty"`

	// Unstable lists the items that differed between samples and are not lengths: nobody can
	// mirror them.
	Unstable []string `json:"unstable,omitempty"`

	// Informational, not compared.
	LeafKey      string `json:"leaf_key,omitempty"`
	CertVerified bool   `json:"cert_verified,omitempty"`
	RequestOK    bool   `json:"request_ok,omitempty"`
	RequestInfo  string `json:"request_info,omitempty"`
}

// Aggregate folds the Vectors of one cover into a profile. vs must not be empty.
func Aggregate(vs []Vector) *CoverProfile {
	p := &CoverProfile{Schema: SchemaVersion, Samples: len(vs)}
	if len(vs) == 0 {
		return p
	}
	f := vs[0]
	p.Transport = f.Transport
	p.Version, p.Cipher, p.Group, p.HRR = f.Version, f.Cipher, f.Group, f.HRR
	p.ExtOrder, p.SessionIDLen, p.SCIDLen, p.CCS = f.ExtOrder, f.SessionIDLen, f.SCIDLen, f.CCS

	unstable := map[string]bool{}
	for _, v := range vs[1:] {
		if v.Version != f.Version {
			unstable[ItemVersion] = true
		}
		if v.Cipher != f.Cipher {
			unstable[ItemCipher] = true
		}
		if v.Group != f.Group {
			unstable[ItemGroup] = true
		}
		if v.HRR != f.HRR {
			unstable[ItemHRR] = true
		}
		if !slices.Equal(v.ExtOrder, f.ExtOrder) {
			unstable[ItemExtOrder] = true
		}
		if v.SessionIDLen != f.SessionIDLen {
			unstable[ItemSessionID] = true
		}
		if v.SCIDLen != f.SCIDLen {
			unstable[ItemSCID] = true
		}
		if v.CCS != f.CCS {
			unstable[ItemCCS] = true
		}
		if !sameShape(v.Flight, f.Flight) {
			unstable[ItemPacking] = true
		}
		if len(v.Post) != len(f.Post) {
			unstable[postCountItem(f.Decrypted)] = true
		} else if !sameShape(v.Post, f.Post) {
			unstable[ItemPacking] = true
		}
		if f.Decrypted {
			if !sameShape(v.Inner, f.Inner) {
				unstable[ItemMsgs] = true
			}
			if len(v.Tickets) != len(f.Tickets) {
				unstable[ItemTicketCount] = true
			}
		}
	}
	p.Flight = slots(f.Flight, vs, func(v Vector) []Item { return v.Flight })
	p.Post = slots(f.Post, vs, func(v Vector) []Item { return v.Post })
	if f.Decrypted {
		p.Decrypted = true
		p.Inner = slots(f.Inner, vs, func(v Vector) []Item { return v.Inner })
		p.Tickets = slots(f.Tickets, vs, func(v Vector) []Item { return v.Tickets })
	}
	for _, it := range []string{ItemVersion, ItemCipher, ItemGroup, ItemHRR, ItemExtOrder, ItemSessionID,
		ItemSCID, ItemCCS, ItemPacking, ItemMsgs, ItemTicketCount} {
		if unstable[it] {
			p.Unstable = append(p.Unstable, it)
		}
	}
	return p
}

func sameShape(a, b []Item) bool {
	return slices.EqualFunc(a, b, func(x, y Item) bool { return x.Kind == y.Kind && x.Unit == y.Unit })
}

// slots builds the slots of ref with the length range over every sample of the same shape.
func slots(ref []Item, vs []Vector, pick func(Vector) []Item) []Slot {
	out := make([]Slot, len(ref))
	for i, it := range ref {
		out[i] = Slot{Kind: it.Kind, Unit: it.Unit, Min: it.Len, Max: it.Len}
	}
	for _, v := range vs {
		items := pick(v)
		if !sameShape(items, ref) {
			continue
		}
		for i, it := range items {
			out[i].Min, out[i].Max = min(out[i].Min, it.Len), max(out[i].Max, it.Len)
		}
	}
	return out
}

// Diff is one item our server does not reproduce.
type Diff struct {
	Item   string
	Cover  string
	Ours   string
	Detail string
	// PadFixable is set for length differences where every shorter-than-cover position could be
	// raised to the cover's length by padding (an X.509 extension for the certificate); a longer
	// position cannot be fixed.
	PadFixable bool
}

func (d Diff) String() string {
	return fmt.Sprintf("%s: cover %s, ours %s %s", d.Item, d.Cover, d.Ours, d.Detail)
}

// Compare lists what ours does not reproduce of cover, in a fixed order. An empty list means
// every observable item matches. An item the cover itself varies is reported as "unstable" and
// not compared further.
func Compare(cover, ours *CoverProfile) []Diff {
	var out []Diff
	skip := map[string]bool{}
	for _, it := range cover.Unstable {
		skip[it] = true
		out = append(out, Diff{Item: it, Cover: "unstable", Ours: "-"})
	}
	for _, it := range ours.Unstable {
		if !skip[it] {
			skip[it] = true
			out = append(out, Diff{Item: it, Cover: "-", Ours: "unstable"})
		}
	}
	disc := func(item string, c, o any) {
		if skip[item] {
			return
		}
		if fmt.Sprint(c) != fmt.Sprint(o) {
			out = append(out, Diff{Item: item, Cover: fmt.Sprint(c), Ours: fmt.Sprint(o)})
		}
	}
	disc(ItemVersion, hex16(cover.Version), hex16(ours.Version))
	disc(ItemCipher, hex16(cover.Cipher), hex16(ours.Cipher))
	disc(ItemGroup, hex16(cover.Group), hex16(ours.Group))
	disc(ItemHRR, cover.HRR, ours.HRR)
	disc(ItemALPN, cover.ALPN, ours.ALPN)
	if !skip[ItemExtOrder] && !slices.Equal(cover.ExtOrder, ours.ExtOrder) {
		out = append(out, Diff{Item: ItemExtOrder, Cover: fmt.Sprint(cover.ExtOrder), Ours: fmt.Sprint(ours.ExtOrder)})
	}
	disc(ItemSessionID, cover.SessionIDLen, ours.SessionIDLen)
	if cover.Transport == QUIC {
		disc(ItemSCID, cover.SCIDLen, ours.SCIDLen)
	} else {
		disc(ItemCCS, cover.CCS, ours.CCS)
	}

	decrypted := cover.Decrypted && ours.Decrypted
	flightShape := sameSlots(cover.Flight, ours.Flight)
	postShape := len(cover.Post) != len(ours.Post) || sameSlots(cover.Post, ours.Post)
	if decrypted && len(cover.Post) != len(ours.Post) && !skip[ItemPacking] {
		// tickets sent after the client's Finished versus inside the server flight
		out = append(out, Diff{Item: ItemPacking, Cover: fmt.Sprintf("%d post-handshake records", len(cover.Post)),
			Ours: fmt.Sprintf("%d post-handshake records", len(ours.Post))})
	}
	if !skip[ItemPacking] {
		if !flightShape || !postShape {
			out = append(out, Diff{Item: ItemPacking, Cover: shape(cover.Flight) + " | " + shape(cover.Post),
				Ours: shape(ours.Flight) + " | " + shape(ours.Post)})
		}
		if flightShape {
			if d, ok := lengthDiff(ItemFlightLen, cover.Flight, ours.Flight); ok {
				out = append(out, d)
			}
		}
	}
	cTick, oTick := cover.Post, ours.Post
	if decrypted {
		cTick, oTick = cover.Tickets, ours.Tickets
		if !skip[ItemMsgs] {
			if !sameSlots(cover.Inner, ours.Inner) {
				out = append(out, Diff{Item: ItemMsgs, Cover: shape(cover.Inner), Ours: shape(ours.Inner)})
			} else if d, ok := lengthDiff(ItemMsgLen, cover.Inner, ours.Inner); ok {
				out = append(out, d)
			}
		}
	}
	if !skip[ItemTicketCount] {
		if len(cTick) != len(oTick) {
			out = append(out, Diff{Item: ItemTicketCount, Cover: fmt.Sprint(len(cTick)), Ours: fmt.Sprint(len(oTick))})
		} else if (decrypted || postShape) && !skip[ItemPacking] {
			if d, ok := lengthDiff(ItemTicketLen, cTick, oTick); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func hex16(v uint16) string { return fmt.Sprintf("0x%04x", v) }

func sameSlots(a, b []Slot) bool {
	return slices.EqualFunc(a, b, func(x, y Slot) bool { return x.Kind == y.Kind && x.Unit == y.Unit })
}

func shape(s []Slot) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x.Kind
		if x.Unit != 0 {
			out += fmt.Sprintf("@%d", x.Unit)
		}
	}
	return out
}

// lengthDiff compares the length ranges of two flights of the same shape: ours must lie inside
// the range the cover showed.
func lengthDiff(item string, cover, ours []Slot) (Diff, bool) {
	var bad []string
	fixable := true
	for i := range cover {
		c, o := cover[i], ours[i]
		if o.Min >= c.Min && o.Max <= c.Max {
			continue
		}
		bad = append(bad, fmt.Sprintf("#%d cover %d-%d ours %d-%d", i, c.Min, c.Max, o.Min, o.Max))
		if o.Max >= c.Min {
			fixable = false
		}
	}
	if len(bad) == 0 {
		return Diff{}, false
	}
	return Diff{Item: item, Cover: "see detail", Ours: "see detail", Detail: fmt.Sprint(bad), PadFixable: fixable}, true
}

// postCountItem names the item a difference in the number of post-handshake records belongs to:
// without decryption they are taken to be tickets.
func postCountItem(decrypted bool) string {
	if decrypted {
		return ItemPacking
	}
	return ItemTicketCount
}
