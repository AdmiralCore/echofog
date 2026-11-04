package objects

import (
	"errors"
	"fmt"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// DevicePrimary is Device.flags bit 0.
const DevicePrimary = 1

// Processing limits (objects.md §2.3: readers use the first N elements and ignore the rest).
const (
	maxDevices  = 16
	maxDevLabel = 32
)

// DeviceList is a verified echofog/devices/2 object (objects.md §6), signed by the account key.
type DeviceList struct {
	Env     *Envelope
	Body    *pb.DeviceList
	Account keys.AKey
	NodeID  [32]byte
	usable  []keys.AKey // device keys of the processed entries with a known tag
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// invalidDevices reports a card whose embedded device list is missing, invalid or does not list
// the card's signer; the error matches both ErrInvalid and ErrDevices.
func invalidDevices(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrInvalid, ErrDevices, fmt.Sprintf(format, args...))
}

func expectLabel(e *Envelope, label string) error {
	if e.Label != label {
		return fmt.Errorf("%w: %q, want %q", ErrWrongLabel, e.Label, label)
	}
	return nil
}

// VerifyDeviceList verifies a device list.
func VerifyDeviceList(b []byte, now time.Time) (*DeviceList, error) {
	return verifyDeviceListAt(b, &now)
}

func verifyDeviceListAt(b []byte, now *time.Time) (*DeviceList, error) {
	e, err := verifyAt(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelDevices); err != nil {
		return nil, err
	}
	body := &pb.DeviceList{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	dl := &DeviceList{Env: e, Body: body, Account: e.Signer, NodeID: e.Signer.ID()}
	for i, d := range body.GetDevices() {
		k, err := keys.Parse(d.GetKey())
		switch {
		case errors.Is(err, keys.ErrUnknownAlg):
			continue // unusable entry, not an error (registries.md §8)
		case err != nil:
			return nil, invalid("device %d: %v", i, err)
		case k.Alg() != keys.Ed25519:
			return nil, invalid("device %d: %s key", i, k.Alg())
		}
		if len(d.GetLabel()) > maxDevLabel {
			return nil, invalid("device %d: label longer than %d bytes", i, maxDevLabel)
		}
		if i < maxDevices {
			dl.usable = append(dl.usable, k)
		}
	}
	return dl, nil
}

// Devices returns the processed device entries (at most 16).
func (dl *DeviceList) Devices() []*pb.Device {
	d := dl.Body.GetDevices()
	if len(d) > maxDevices {
		d = d[:maxDevices]
	}
	return d
}

// Contains reports whether the processed list contains the device key k.
func (dl *DeviceList) Contains(k keys.AKey) bool {
	for _, d := range dl.usable {
		if d.Equal(k) {
			return true
		}
	}
	return false
}

// SealDeviceList encodes and signs a device list with the account key.
func SealDeviceList(account *keys.Signer, seq, notAfter uint64, body *pb.DeviceList) ([]byte, error) {
	b, err := Marshal(body)
	if err != nil {
		return nil, err
	}
	env, err := Seal(LabelDevices, account, seq, notAfter, b)
	if err != nil {
		return nil, err
	}
	if _, err := verifyDeviceListAt(env, nil); err != nil {
		return nil, err
	}
	return env, nil
}
