// Command capture records the browser-profile database used by the M1 detectors and by T2.6d:
// the uTLS Chrome preset, quic-go with ChromeParrot, crypto/tls, curl, and a real Chrome when it
// is installed. Run it on a machine in the country/network you want to profile:
//
//	go run ./testbed/browserprofiles/cmd/capture -country lab -network lab
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/echofog/echofog-core/testbed/browserprofiles"
)

func main() {
	country := flag.String("country", "lab", "country of the capture (lab, ru, cn, ir, ...)")
	network := flag.String("network", "lab", "network of the capture (lab, ru-mobile, ...)")
	out := flag.String("out", filepath.Join("testbed", "browserprofiles", "profiles.json"), "where the database goes")
	flag.Parse()

	db := browserprofiles.CaptureLocal(context.Background(), *country, *network)
	if len(db.Entries) == 0 {
		fmt.Fprintln(os.Stderr, "no source could be captured")
		os.Exit(1)
	}
	if err := db.Save(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s: %d profiles (version %s)\n", *out, len(db.Entries), db.Version)
	for _, p := range db.Entries {
		switch {
		case p.TLS != nil:
			fmt.Printf("  %-32s tls  %s  grease=%v alpn=%s\n", p.Source, p.TLS.JA4, p.TLS.Grease, p.TLS.ALPN)
		case p.QUIC != nil:
			fmt.Printf("  %-32s quic %s  first=%d snI=%s\n", p.Source, p.QUIC.JA4, p.QUIC.FirstSize, p.QUIC.SNI)
		}
	}
}
