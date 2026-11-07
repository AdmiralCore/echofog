package objects

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	pb "github.com/echofog/echofog-core/internal/v2/objects/pb"
)

// T12.4e: min_version is the oldest version that may be installed or kept. It is part of the signed
// manifest; a malformed bound, or one above the version of the manifest itself, is not a manifest.
func TestReleaseMinVersion(t *testing.T) {
	r := newReleaseKeys(t)
	check := func(version, min string) error {
		st, err := NewReleaseState(r.compiled())
		if err != nil {
			t.Fatal(err)
		}
		m := rel(fixedTime, func(m *pb.Release) { m.Version = version; m.MinVersion = min })
		_, err = st.Check(sealRel(t, r.k[0], m, r.k[1]), vectorNow)
		return err
	}
	for _, ok := range [][2]string{{"2.0.1", ""}, {"2.0.1", "2.0.0"}, {"2.0.1", "2.0.1"}, {"2.1.0", "1.9.9"}, {"v2.0.1", "2.0.0"}} {
		if err := check(ok[0], ok[1]); err != nil {
			t.Errorf("version %s min %q refused: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{{"2.0.1", "2.0.2"}, {"2.0.1", "3.0.0"}, {"2.0.1", "x"}, {"2.0.1", "2.0"}, {"2.0.1", "02.0.0"}, {"2.0.1", "2.0.0\n"},
		{"2.0.1", "-1.0.0"}, {"2.0.1", "2.0.0 "}} {
		if err := check(bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("version %s min %q: %v, want ErrInvalid", bad[0], bad[1], err)
		}
	}
}

// A manifest with min_version is still read by a node that does not know the field (it is kept as an
// unknown field and the signature covers it), so the field is additive.
func TestReleaseMinVersionIsSigned(t *testing.T) {
	r := newReleaseKeys(t)
	st, _ := NewReleaseState(r.compiled())
	m := rel(fixedTime, func(m *pb.Release) { m.Version = "2.0.1"; m.MinVersion = "2.0.0" })
	mf, err := st.Check(sealRel(t, r.k[0], m, r.k[1]), vectorNow)
	if err != nil || mf.Release.GetMinVersion() != "2.0.0" {
		t.Fatalf("manifest: %v %v", mf, err)
	}
}

// ParseSemver accepts exactly the canonical form: what it accepts, it prints back unchanged (after
// the optional "v"), so a bound that passed the check means one precise version.
func FuzzParseSemver(f *testing.F) {
	for _, s := range []string{"", "1.2.3", "v1.2.3", "01.2.3", "1.2", "1.2.3.4", "999999.0.0", "1000000.0.0", "-1.0.0", "1.2.3\n", "1..3", "٣.٣.٣"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := ParseSemver(s)
		if !ok {
			return
		}
		got := fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
		if got != strings.TrimPrefix(s, "v") {
			t.Fatalf("%q parsed as %v but prints as %q", s, v, got)
		}
		if CompareSemver(v, v) != 0 {
			t.Fatal("a version differs from itself")
		}
	})
}
