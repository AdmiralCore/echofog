package app

// SnapshotDoc is the content of a group snapshot, for the tests that seal and open one themselves.
type SnapshotDoc = snapshotDoc

// OpenSnapshotForTest opens a sealed snapshot.
func OpenSnapshotForTest(raw []byte, password string) (*SnapshotDoc, error) {
	return openSnapshot(raw, password)
}

// SealSnapshotForTest seals a snapshot.
func SealSnapshotForTest(d *SnapshotDoc, password string) ([]byte, error) {
	return sealSnapshot(d, password)
}
