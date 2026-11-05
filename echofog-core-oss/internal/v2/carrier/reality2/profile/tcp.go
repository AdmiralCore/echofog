package profile

import (
	"fmt"
)

const (
	recChangeCipherSpec = 20
	recAlert            = 21
	recHandshake        = 22
	recApplicationData  = 23
	maxRecordPayload    = 16384 + 256
)

type tlsRecord struct {
	typ     byte
	payload []byte
	at      int // index of the event that completed the record
	server  bool
}

// splitRecords cuts one direction of a TCP capture into TLS records. A trailing partial record
// is ignored: a capture may stop in the middle of one.
func splitRecords(events []Event, fromServer bool) ([]tlsRecord, error) {
	var (
		buf  []byte
		ends []int // ends[i] = event index of buf[i]
		out  []tlsRecord
	)
	for i, e := range events {
		if e.FromServer != fromServer {
			continue
		}
		buf = append(buf, e.Data...)
		for range e.Data {
			ends = append(ends, i)
		}
	}
	for len(buf) >= 5 {
		typ := buf[0]
		if typ < recChangeCipherSpec || typ > 24 || buf[1] != 3 {
			return nil, fmt.Errorf("%w: TLS record header % x", ErrMalformed, buf[:5])
		}
		l := int(buf[3])<<8 | int(buf[4])
		if l > maxRecordPayload {
			return nil, fmt.Errorf("%w: TLS record of %d bytes", ErrMalformed, l)
		}
		if len(buf) < 5+l {
			break
		}
		out = append(out, tlsRecord{typ: typ, payload: buf[5 : 5+l], at: ends[5+l-1], server: fromServer})
		buf, ends = buf[5+l:], ends[5+l:]
		if len(out) > maxItems {
			return nil, ErrTooBig
		}
	}
	return out, nil
}

// ObserveTCP turns the bytes of one TCP connection, in the order they crossed the wire, into the
// Vector an observer would get. The capture must end after the server's post-handshake records
// and before the client's first request.
func ObserveTCP(events []Event) (Vector, error) {
	v, _, err := observeTCP(events)
	return v, err
}

// observeTCP also returns the server records of the flight and the post-handshake window, which
// the keyed observation decrypts.
func observeTCP(events []Event) (Vector, []tlsRecord, error) {
	v := Vector{Transport: TCP}
	total := 0
	for _, e := range events {
		total += len(e.Data)
	}
	if total > maxCaptureBytes {
		return v, nil, ErrTooBig
	}
	srv, err := splitRecords(events, true)
	if err != nil {
		return v, nil, err
	}
	cli, err := splitRecords(events, false)
	if err != nil {
		return v, nil, err
	}
	var kept []tlsRecord
	all := make([]tlsRecord, 0, len(srv)+len(cli))
	all = append(all, srv...)
	all = append(all, cli...)
	sortRecords(all)

	const (
		inFlight = iota
		inPost
		done
	)
	state := inFlight
	var hs []byte // server handshake bytes sent in the clear
	for _, r := range all {
		switch {
		case !r.server:
			switch state {
			case inFlight:
				if r.typ == recApplicationData {
					state = inPost
				}
			case inPost:
				state = done
			}
		case state == inFlight:
			kept = append(kept, r)
			v.Flight = append(v.Flight, Item{Kind: fmt.Sprintf("rec/%d", r.typ), Len: len(r.payload)})
			if r.typ == recChangeCipherSpec {
				v.CCS = true
			}
			if r.typ == recHandshake && len(hs) < maxCaptureBytes {
				hs = append(hs, r.payload...)
			}
		case state == inPost:
			kept = append(kept, r)
			v.Post = append(v.Post, Item{Kind: fmt.Sprintf("rec/%d", r.typ), Len: len(r.payload)})
		}
		if state == done {
			break
		}
	}
	v, err = finishHello(v, hs)
	return v, kept, err
}

func sortRecords(rs []tlsRecord) {
	// insertion sort by completion event; the lists are short and nearly sorted
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].at < rs[j-1].at; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// finishHello fills the open ServerHello fields of v from the clear handshake bytes hs.
func finishHello(v Vector, hs []byte) (Vector, error) {
	var last *serverHello
	for len(hs) >= 4 {
		n := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
		if len(hs) < 4+n {
			break
		}
		if hs[0] == 2 {
			sh, err := parseServerHello(hs[:4+n])
			if err != nil {
				return v, err
			}
			if sh.HRR {
				v.HRR = true
			}
			last = &sh
		}
		hs = hs[4+n:]
	}
	if last == nil {
		return v, ErrNoServerHello
	}
	v.Version, v.Cipher, v.Group = last.Version, last.Cipher, last.Group
	v.ExtOrder, v.SessionIDLen = last.ExtOrder, last.SessionIDLen
	return v, nil
}
