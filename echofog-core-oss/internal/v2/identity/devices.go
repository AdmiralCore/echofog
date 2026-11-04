package identity

import (
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	"github.com/echofog/echofog-core/internal/v2/objects"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// DeviceListLifetime is the lifetime of a device list (objects.md §6: at most 30 days).
const DeviceListLifetime = 30 * 24 * time.Hour

// DeviceEntry is one device in a signed device list.
type DeviceEntry struct {
	// Key is the algorithm-tagged device signing key.
	Key keys.AKey
	// Added is when the account list first included the device.
	Added time.Time
	// Primary marks the device the account was created on (bit 0 of flags).
	Primary bool
	// Label is the device's name, at most 32 bytes.
	Label string
}

// SealDeviceList builds and signs echofog/devices/2 with the account key (objects.md §6). The
// object is verified before it is returned, so a bad entry (for example a non-Ed25519 key) is an
// error here rather than at the peer.
func (a *Account) SealDeviceList(seq, notAfter uint64, entries []DeviceEntry) ([]byte, error) {
	body := &pb.DeviceList{Devices: make([]*pb.Device, 0, len(entries))}
	for _, e := range entries {
		var flags uint32
		if e.Primary {
			flags |= objects.DevicePrimary
		}
		body.Devices = append(body.Devices, &pb.Device{
			Key:   append([]byte(nil), e.Key...),
			Added: unix(e.Added),
			Flags: flags,
			Label: e.Label,
		})
	}

	var out []byte
	err := a.WithSigner(func(signer *keys.Signer) error {
		var err error
		out, err = objects.SealDeviceList(signer, seq, notAfter, body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SealSingleDeviceList signs the one-device list that v2.0 accounts have (objects.md §6), valid
// for DeviceListLifetime from now.
func (a *Account) SealSingleDeviceList(dev *Device, seq uint64, now time.Time) ([]byte, error) {
	return a.SealDeviceList(seq, uint64(now.Add(DeviceListLifetime).Unix()), []DeviceEntry{
		{Key: dev.Public(), Added: now, Primary: true},
	})
}

// unix converts a time to the unix seconds used on the wire; times before 1970 become 0.
func unix(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}
