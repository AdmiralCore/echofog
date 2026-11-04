package objects

import (
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/echofog/echofog-core/internal/v2/keys"
	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// Release manifests (objects.md §9, versioning.md §5, Decision D16).

const (
	// CosignLabel prefixes the co-signature input.
	CosignLabel = "echofog/release-cosign/2"

	minReleaseKeys      = 2
	maxReleaseKeys      = 5
	maxNextKeys         = 3
	releaseFutureWindow = day      // released_at ≤ now + 86400
	minDeprecation      = 90 * day // deprecated_after < released_at + 90 days is ignored
	protoMajorStep      = 100      // proto_ver = major × 100 + minor
)

// CosignInput returns "echofog/release-cosign/2" ‖ SHA-256(manifest).
func CosignInput(manifest []byte) []byte {
	h := sha256.Sum256(manifest)
	return append([]byte(CosignLabel), h[:]...)
}

// Cosign returns the co-signature of manifest by a release key.
func Cosign(s *keys.Signer, manifest []byte) *pb.CoSig {
	return &pb.CoSig{Key: s.Public(), Sig: s.Sign(CosignInput(manifest))}
}

// SealRelease builds a release object: the manifest signed in the envelope by signer (seq =
// released_at) and co-signed by cosigners.
func SealRelease(signer *keys.Signer, notAfter uint64, rel *pb.Release, cosigners ...*keys.Signer) ([]byte, error) {
	manifest, err := Marshal(rel)
	if err != nil {
		return nil, err
	}
	obj := &pb.ReleaseObject{Manifest: manifest}
	for _, c := range cosigners {
		obj.Cosigs = append(obj.Cosigs, Cosign(c, manifest))
	}
	body, err := Marshal(obj)
	if err != nil {
		return nil, err
	}
	return Seal(LabelRelease, signer, rel.GetReleasedAt(), notAfter, body)
}

// Manifest is a release object valid under a key set.
type Manifest struct {
	Env     *Envelope
	Body    *pb.ReleaseObject
	Release *pb.Release
	// Signers are the two or more distinct release keys whose signatures verified (envelope first).
	Signers []keys.AKey
	// Keys is the release key set after applying this manifest.
	Keys []keys.AKey
}

// ReleaseState holds the current release key set and the last applied manifest.
type ReleaseState struct {
	mu       sync.Mutex
	keys     []keys.AKey
	last     *Manifest
	policies map[string]*pb.Policy // effective policies of the last applied manifest
}

// NewReleaseState starts from the release keys compiled into the build.
func NewReleaseState(compiled []keys.AKey) (*ReleaseState, error) {
	set, err := uniqueKeys(compiled)
	if err != nil {
		return nil, err
	}
	if len(set) < minReleaseKeys || len(set) > maxReleaseKeys {
		return nil, invalid("release key set of %d keys", len(set))
	}
	return &ReleaseState{keys: set}, nil
}

func uniqueKeys(in []keys.AKey) ([]keys.AKey, error) {
	var out []keys.AKey
	for _, k := range in {
		if _, err := keys.ParseKind(k, keys.Ed25519); err != nil {
			return nil, invalid("release key: %v", err)
		}
		if !containsKey(out, k) {
			out = append(out, append(keys.AKey(nil), k...))
		}
	}
	return out, nil
}

func containsKey(set []keys.AKey, k keys.AKey) bool {
	for _, x := range set {
		if x.Equal(k) {
			return true
		}
	}
	return false
}

// Keys returns the current release key set.
func (s *ReleaseState) Keys() []keys.AKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]keys.AKey(nil), s.keys...)
}

// Last returns the last applied manifest, or nil.
func (s *ReleaseState) Last() *Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Check verifies a release object against the current state without applying it.
func (s *ReleaseState) Check(b []byte, now time.Time) (*Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.check(b, now)
}

// Apply verifies a release object and makes it the current manifest: the key set changes by its
// next_keys and revoked_keys, and its policies become the effective ones.
func (s *ReleaseState) Apply(b []byte, now time.Time) (*Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.check(b, now)
	if err != nil {
		return nil, err
	}
	s.policies = s.effectivePolicies(m.Release)
	s.keys = m.Keys
	s.last = m
	return m, nil
}

func (s *ReleaseState) check(b []byte, now time.Time) (*Manifest, error) {
	e, err := Verify(b, now)
	if err != nil {
		return nil, err
	}
	if err := expectLabel(e, LabelRelease); err != nil {
		return nil, err
	}
	if !containsKey(s.keys, e.Signer) {
		return nil, invalid("release: envelope signer is not a current release key")
	}
	body := &pb.ReleaseObject{}
	if err := unmarshalCanonical(e.Body, body); err != nil {
		return nil, err
	}
	manifest := body.GetManifest()
	rel := &pb.Release{}
	if err := unmarshalCanonical(manifest, rel); err != nil {
		return nil, invalid("release manifest: %v", err)
	}

	// seq is bound to time: seq = released_at ≤ now + 1 day, and later than the last applied one.
	if e.Seq != rel.GetReleasedAt() {
		return nil, invalid("release: seq %d ≠ released_at %d", e.Seq, rel.GetReleasedAt())
	}
	if rel.GetReleasedAt() > unix(now)+releaseFutureWindow {
		return nil, invalid("release: released_at more than one day in the future")
	}
	if s.last != nil && rel.GetReleasedAt() <= s.last.Release.GetReleasedAt() {
		return nil, invalid("release: not newer than the last applied manifest")
	}

	// Threshold: the envelope signer plus at least one co-signature by another current key.
	signers := []keys.AKey{e.Signer}
	in := CosignInput(manifest)
	for _, cs := range body.GetCosigs() {
		k, err := keys.ParseKind(cs.GetKey(), keys.Ed25519)
		if err != nil || !containsKey(s.keys, k) || containsKey(signers, k) {
			continue
		}
		if keys.Verify(k, in, cs.GetSig()) == nil {
			signers = append(signers, k)
		}
	}
	if len(signers) < 2 {
		return nil, invalid("release: fewer than 2 distinct release-key signatures")
	}

	if err := checkReleaseFields(rel); err != nil {
		return nil, err
	}

	// Key set change: valid under the set before the change; signers not among the revoked keys.
	next := rel.GetNextKeys()
	if len(next) > maxNextKeys {
		next = next[:maxNextKeys]
	}
	var revoked []keys.AKey
	for _, r := range rel.GetRevokedKeys() {
		k, err := keys.Parse(r)
		if errors.Is(err, keys.ErrUnknownAlg) {
			continue
		}
		if err != nil {
			return nil, invalid("release: revoked key: %v", err)
		}
		revoked = append(revoked, k)
	}
	for _, sg := range signers {
		if containsKey(revoked, sg) {
			return nil, invalid("release: a signer revokes its own key")
		}
	}
	set := append([]keys.AKey(nil), s.keys...)
	for _, n := range next {
		k, err := keys.ParseKind(n, keys.Ed25519)
		if err != nil {
			return nil, invalid("release: next key: %v", err)
		}
		if err := keys.CheckEd25519Public(k.Key()); err != nil {
			return nil, invalid("release: next key: %v", err)
		}
		if !containsKey(set, k) {
			set = append(set, append(keys.AKey(nil), k...))
		}
	}
	kept := set[:0:0]
	for _, k := range set {
		if !containsKey(revoked, k) {
			kept = append(kept, k)
		}
	}
	if len(kept) < minReleaseKeys || len(kept) > maxReleaseKeys {
		return nil, invalid("release: key set would have %d keys", len(kept))
	}
	return &Manifest{Env: e, Body: body, Release: rel, Signers: signers, Keys: kept}, nil
}

func checkReleaseFields(rel *pb.Release) error {
	if u := rel.GetNotesUrl(); u != "" {
		if err := validateHTTPS(u, "notes_url"); err != nil {
			return err
		}
	}
	for i, a := range rel.GetArtifacts() {
		if u := a.GetUrl(); u != "" {
			if err := validateHTTPS(u, "artifact url"); err != nil {
				return invalid("artifact %d: %v", i, err)
			}
		}
		if h := a.GetSha256(); len(h) != 0 && len(h) != 32 {
			return invalid("artifact %d: sha256 of %d bytes", i, len(h))
		}
	}
	for i, p := range rel.GetPolicies() {
		if p.GetComponent() == "" {
			return invalid("policy %d: empty component", i)
		}
	}
	return nil
}

func majorStep(component string) uint32 {
	if component == "proto" {
		return protoMajorStep
	}
	return 1
}

// effectivePolicies applies the "at most one major version per manifest" rule: a min_supported
// raise beyond one major version over the previous manifest is clipped (versioning.md §5).
func (s *ReleaseState) effectivePolicies(rel *pb.Release) map[string]*pb.Policy {
	out := make(map[string]*pb.Policy)
	for _, p := range rel.GetPolicies() {
		c := p.GetComponent()
		if _, dup := out[c]; dup {
			continue // first entry of a component wins
		}
		q := &pb.Policy{Component: c, MinSupported: p.GetMinSupported(), Recommended: p.GetRecommended(),
			DeprecatedAfter: p.GetDeprecatedAfter()}
		prev, ok := uint32(0), false
		if old := s.policies[c]; old != nil {
			prev, ok = old.GetMinSupported(), true
		} else if c == "proto" {
			prev, ok = MinProtocolVersion, true
		}
		if ok && q.MinSupported > prev+majorStep(c) {
			q.MinSupported = prev + majorStep(c)
		}
		// No instant switch-off: a deprecation sooner than 90 days after release is ignored.
		if q.DeprecatedAfter != 0 && q.DeprecatedAfter < rel.GetReleasedAt()+minDeprecation {
			q.DeprecatedAfter = 0
		}
		out[c] = q
	}
	return out
}

// Policy returns the effective policy of a component in the last applied manifest, or nil.
func (s *ReleaseState) Policy(component string) *pb.Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policies[component]
}

// minAt is the effective minimum of a policy at time now: min_supported, raised to recommended once
// deprecated_after has passed.
func minAt(p *pb.Policy, now time.Time) uint32 {
	m := p.GetMinSupported()
	if d := p.GetDeprecatedAfter(); d != 0 && unix(now) >= d && p.GetRecommended() > m {
		m = p.GetRecommended()
	}
	return m
}

// PeerRefused reports whether a node running version own of component must refuse a peer running
// version peer. Never lock out: refusals apply only to peer versions strictly below the node's own,
// and an entry that would cover the node's own version is ignored for refusals.
func (s *ReleaseState) PeerRefused(component string, own, peer uint32, now time.Time) bool {
	p := s.Policy(component)
	if p == nil {
		return false
	}
	m := minAt(p, now)
	if m > own {
		return false
	}
	return peer < m
}

// UpdateStatus is what a client shows for its own version of a component.
type UpdateStatus int

const (
	UpToDate UpdateStatus = iota
	UpdateRecommended
	UpdateRequired
)

// Status returns the update status of the node's own version of a component.
func (s *ReleaseState) Status(component string, own uint32, now time.Time) UpdateStatus {
	p := s.Policy(component)
	switch {
	case p == nil:
		return UpToDate
	case own < minAt(p, now):
		return UpdateRequired
	case own < p.GetRecommended():
		return UpdateRecommended
	}
	return UpToDate
}
