package handshake

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	utls "github.com/refraction-networking/utls"
)

// chromeQUICClientHelloSpec returns the TLS ClientHello Chrome sends over QUIC.
//
// This is deliberately NOT one of uTLS's HelloChrome_* presets. Those are
// TLS-over-TCP (h2) fingerprints, and the QUIC ClientHello is a materially
// different message; using a TCP preset over QUIC produces a combination
// matching no real client, which is worse than not trying at all.
//
// The extension order is permuted per connection, hence the shuffle.
//
// alpn comes from the caller's tls.Config rather than being hardcoded:
// advertising a protocol the peer doesn't speak breaks the connection outright,
// which is worse than an imperfect fingerprint. The match is exact only for the
// protocol Chrome would negotiate.
func chromeQUICClientHelloSpec(alpn []string) *utls.ClientHelloSpec {
	if len(alpn) == 0 {
		alpn = []string{"h3"}
	}
	return &utls.ClientHelloSpec{
		// A GREASE cipher suite first, as real Chrome sends. uTLS rewrites the
		// GREASE_PLACEHOLDER into a per-connection 0x?a?a value from its GREASE seed.
		// A censor reading JA3/JA4 counts this as a browser tell; omitting it (as this
		// spec once did) left the fingerprint of a library, not of Chrome.
		CipherSuites: []uint16{
			utls.GREASE_PLACEHOLDER,
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
		},
		CompressionMethods: []byte{0x00},
		Extensions: utls.ShuffleChromeTLSExtensions([]utls.TLSExtension{
			// The first of Chrome's two GREASE extensions. ShuffleChromeTLSExtensions
			// treats GREASE as positionally invariant, so this one stays first and its
			// sibling below stays last, bracketing the list as Chrome does.
			&utls.UtlsGREASEExtension{},
			&utls.SNIExtension{},
			// A GREASE curve first, as real Chrome sends.
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.CurveID(utls.GREASE_PLACEHOLDER),
				utls.X25519MLKEM768,
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
			}},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.ALPNExtension{AlpnProtocols: alpn},
			&utls.UtlsCompressCertExtension{Algorithms: []utls.CertCompressionAlgo{
				utls.CertCompressionBrotli,
			}},
			// TLS 1.3, preceded by a GREASE version, as real Chrome sends.
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.GREASE_PLACEHOLDER,
				utls.VersionTLS13,
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			// A GREASE key share first (one zero byte of key data, as Chrome sends),
			// then the real ones. The ML-KEM share is what pushes the ClientHello past
			// a single packet, giving the characteristic two Initial datagrams.
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.CurveID(utls.GREASE_PLACEHOLDER), Data: []byte{0}},
				{Group: utls.X25519MLKEM768},
				{Group: utls.X25519},
			}},
			// Populated from quic-go's own marshalled parameters; see
			// utlsQUICConn.SetTransportParameters.
			&utls.QUICTransportParametersExtension{},
			// ALPS at the newer codepoint; uTLS's presets use the older one.
			&utls.ApplicationSettingsExtensionNew{SupportedProtocols: alpn},
			// An ECH extension is always present: real when a config is available
			// from DNS, GREASE otherwise. GREASE is right here, and is structurally
			// indistinguishable from the real thing without decrypting it. Note its
			// type (0xfe0d) is not a GREASE type, so it does not stand in for the
			// GREASE cipher/extension/group above that a JA3/JA4 censor looks for.
			utls.BoringGREASEECH(),
			// uTLS has no native support for trust_anchors, so it goes out raw.
			&utls.GenericExtension{Id: extensionTrustAnchors, Data: chromeTrustAnchorsData},
			// The second of Chrome's two GREASE extensions, kept last by the shuffle.
			&utls.UtlsGREASEExtension{},
		}),
	}
}

// ChromeParrotKeyShare builds the Chrome QUIC ClientHello with rand as the TLS randomness source and
// returns the raw key_share extension body — the same bytes a server reads off the wire
// (client_shares length, then each share's group, length and key).
//
// It builds only as far as ApplyPreset, which is where uTLS generates the ephemeral key shares from
// config.rand and writes them back into the spec's KeyShareExtension. A QUIC ClientHello reads the
// client random first, then (no legacy_session_id, per RFC 9001) the GREASE seed, then the shares, all
// from config.rand; for a HelloCustom preset uTLS never regenerates them, so the shares read here are
// exactly what the live handshake sends. The hello is discarded.
//
// EchoFog uses this to bind its stealth token to the key_share. The token rides in the ClientHello
// random, which is drawn before the shares, so it cannot be sealed to the shares in one build. Instead
// the dial predicts the shares here with a deterministic rand, seals the token to them, and dials for
// real with the same deterministic rand so the live handshake reproduces the identical shares.
func ChromeParrotKeyShare(rand io.Reader, alpn []string) ([]byte, error) {
	spec := chromeQUICClientHelloSpec(alpn)
	uConf := &utls.Config{
		Rand:                   rand,
		MinVersion:             utls.VersionTLS13,
		MaxVersion:             utls.VersionTLS13,
		InsecureSkipVerify:     true,
		SessionTicketsDisabled: true,
		NextProtos:             alpn,
	}
	conn := utls.UQUICClient(&utls.QUICConfig{TLSConfig: uConf}, utls.HelloCustom)
	if err := conn.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("building Chrome ClientHello to read its key_share: %w", err)
	}
	for _, ext := range spec.Extensions {
		ks, ok := ext.(*utls.KeyShareExtension)
		if !ok {
			continue
		}
		return marshalKeyShareBody(ks.KeyShares), nil
	}
	return nil, errors.New("Chrome ClientHello spec has no key_share extension")
}

// marshalKeyShareBody encodes resolved key shares as the key_share extension body appears on the wire:
// a 16-bit client_shares length, then each share's 16-bit group, 16-bit key length and key. This
// matches uTLS's own marshalling (KeyShareExtension.Read) and what a server extracts from a ClientHello.
func marshalKeyShareBody(shares []utls.KeyShare) []byte {
	total := 0
	for _, ks := range shares {
		total += 4 + len(ks.Data)
	}
	body := make([]byte, 2, 2+total)
	body[0] = byte(total >> 8)
	body[1] = byte(total)
	for _, ks := range shares {
		g := uint16(ks.Group)
		body = append(body, byte(g>>8), byte(g), byte(len(ks.Data)>>8), byte(len(ks.Data)))
		body = append(body, ks.Data...)
	}
	return body
}

// extensionTrustAnchors is the TLS trust_anchors extension
// (draft-ietf-tls-trust-anchor-ids).
const extensionTrustAnchors uint16 = 0xca34

// chromeTrustAnchorIDs are the trust anchor IDs Chrome requests, in binary
// relative OID form. The set follows Chrome's root store and changes across
// releases, so it is pinned like the transport parameter values.
var chromeTrustAnchorIDs = [][]byte{
	{0x82, 0xdf, 0x13, 0x02, 0x01},
	{0x82, 0xdf, 0x13, 0x02, 0x06},
	{0x82, 0xdf, 0x13, 0x02, 0x0d},
	{0x82, 0xdf, 0x13, 0x02, 0x0e},
	{0x82, 0xdf, 0x13, 0x02, 0x0f},
	{0x82, 0xdf, 0x13, 0x02, 0x12},
	{0x82, 0xdf, 0x13, 0x02, 0x13},
	{0x82, 0xdf, 0x13, 0x02, 0x14},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x07},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x08},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x09},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x0a},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x0b},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x0c},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x0d},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x12},
	{0x83, 0x9a, 0x64, 0x8c, 0x9b, 0x2d, 0x01, 0x13},
	{0xd6, 0x79, 0x09, 0x01},
	{0xd6, 0x79, 0x09, 0x04},
	{0xd6, 0x79, 0x09, 0x05},
	{0xd6, 0x79, 0x09, 0x06},
	{0xd6, 0x79, 0x09, 0x07},
	{0xd6, 0x79, 0x09, 0x08},
	{0xd6, 0x79, 0x09, 0x0a},
	{0xd6, 0x79, 0x09, 0x0b},
	{0xd6, 0x79, 0x09, 0x0c},
	{0xd6, 0x79, 0x09, 0x0d},
	{0xd6, 0x79, 0x09, 0x0f},
}

var chromeTrustAnchorsData = encodeTrustAnchorIDs(chromeTrustAnchorIDs)

// encodeTrustAnchorIDs encodes the extension body: a 16-bit length, then each ID
// 8-bit length-prefixed. The IDs are sorted first, as Chrome encodes them, so the
// order is the same on every connection.
func encodeTrustAnchorIDs(ids [][]byte) []byte {
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, bytes.Compare)

	var list []byte
	for _, id := range sorted {
		list = append(list, byte(len(id)))
		list = append(list, id...)
	}
	return append([]byte{byte(len(list) >> 8), byte(len(list))}, list...)
}
