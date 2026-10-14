package dht

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	internal_pb "github.com/echofog/echofog-core/internal/proto"
)

const MaxRecordSize = 50 * 1024
const MaxTTL = 7 * 24 * time.Hour

func ValidateRecord(rec *internal_pb.DhtRecord) error {
	if rec == nil {
		return errors.New("nil record")
	}

	if len(rec.Signature) < ed25519.SignatureSize {
		return errors.New("invalid signature size")
	}

	if len(rec.Value) > MaxRecordSize {
		return errors.New("record exceeds maximum size")
	}

	if len(rec.PublisherPubKey) != ed25519.PublicKeySize {
		return errors.New("invalid publisher public key size")
	}

	slog.Debug("DHT: Validating record signature",
		"sig_hex", hex.EncodeToString(rec.Signature[:16]),
		"pub_hex", hex.EncodeToString(rec.PublisherPubKey[:8]))

	now := time.Now().Unix()
	if int64(rec.Timestamp) > now+60 {
		return errors.New("record timestamp is in the future")
	}

	expirationTime := int64(rec.Timestamp) + int64(rec.Ttl)
	if expirationTime < now {
		return errors.New("record has already expired")
	}

	if time.Duration(rec.Ttl)*time.Second > MaxTTL {
		return errors.New("record TTL exceeds maximum allowed duration")
	}

	signData := PackRecordForSigning(rec.Key, rec.Value, rec.Timestamp, rec.Ttl)

	slog.Debug("Validation details",
		"key_len", len(rec.Key),
		"val_len", len(rec.Value),
		"sig_hex", hex.EncodeToString(rec.Signature[:4]))

	if !ed25519.Verify(rec.PublisherPubKey, signData, rec.Signature) {
		return errors.New("cryptographic signature verification failed: fake data detected")
	}

	return nil
}
