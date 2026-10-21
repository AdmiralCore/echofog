package dht

import (
	"log/slog"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
	"google.golang.org/protobuf/proto"
)

// Records other peers store here cost this node disk. Each peer has its hourly quota, but peer
// IDs cost nothing, so the node also caps the records it holds and the stores it takes in an hour,
// all peers together. Records go once their own TTL is over (see SweepRecords).
const (
	// MaxStoredRecords is the most records held; past it only records already held are updated.
	MaxStoredRecords = 10000
	// MaxStoresPerHour is how many stores of new records the node takes in an hour.
	MaxStoresPerHour = 5000

	sweepPage = 500
)

// recordExpired reports whether a record's own TTL is over.
func recordExpired(rec *internal_pb.DhtRecord, now time.Time) bool {
	return int64(rec.Timestamp)+int64(rec.Ttl) < now.Unix()
}

// admitNewRecord reserves room for one record this node does not hold yet; false when the node
// is full or took in its hourly share of new records.
func (d *EchoFogDHT) admitNewRecord() bool {
	d.recMu.Lock()
	counted := d.recCounted
	d.recMu.Unlock()
	if !counted {
		d.SweepRecords(time.Now())
	}
	d.recMu.Lock()
	defer d.recMu.Unlock()
	if d.records >= MaxStoredRecords || d.storesHour >= MaxStoresPerHour {
		return false
	}
	d.records++
	d.storesHour++
	return true
}

// SweepRecords deletes the records whose TTL is over (and any that cannot be read), counts what
// is left and returns how many it deleted. The node runs it with PruneExpired.
func (d *EchoFogDHT) SweepRecords(now time.Time) int {
	if d.swarm == nil {
		return 0
	}
	st := d.swarm.GetStorage()
	if st == nil {
		return 0
	}
	d.sweepMu.Lock()
	defer d.sweepMu.Unlock()

	prefix := []byte(RecordPrefix)
	start := prefix
	kept, deleted := 0, 0
	for {
		page, err := st.FindValuesSeek(prefix, start, sweepPage)
		if err != nil {
			return deleted
		}
		for _, e := range page {
			var rec internal_pb.DhtRecord
			if proto.Unmarshal(e.Value, &rec) != nil || recordExpired(&rec, now) {
				if st.Delete(e.Key) == nil {
					deleted++
					continue
				}
			}
			kept++
		}
		// a page may come back short when entries do not open: stop only on an empty one
		if len(page) == 0 {
			break
		}
		start = append(append([]byte(nil), page[len(page)-1].Key...), 0)
	}

	d.recMu.Lock()
	d.records, d.recCounted = kept, true
	d.recMu.Unlock()
	if deleted > 0 {
		slog.Debug("DHT: swept expired records", "deleted", deleted, "kept", kept)
	}
	return deleted
}
