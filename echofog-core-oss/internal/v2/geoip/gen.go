//go:build ignore

// Command gen turns a DB-IP Lite Country CSV into the compact database embedded by package
// geoip. go:generate runs it from the package directory:
//
//	go generate ./internal/v2/geoip
//
// The source file is not committed. Download it from https://download.db-ip.com/free/ (for
// example dbip-country-lite-2026-11.csv.gz), put it in data/ under the package directory and
// keep the name data/dbip-country-lite.csv.gz. The database and its license are described in
// the package doc and in THIRD_PARTY_NOTICES.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/echofog/echofog-core/internal/v2/geoip/internal/dbgen"
)

func main() {
	in := flag.String("in", "", "DB-IP Lite Country CSV or CSV.gz (required)")
	out := flag.String("out", "country.bin", "output file")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "geoip gen: -in is required")
		os.Exit(2)
	}
	st, err := dbgen.Run(*in, *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "geoip gen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "geoip: %d ranges (%d IPv4, %d IPv6), %d countries, %d bytes, %d unknown rows skipped\n",
		st.Ranges, st.IPv4, st.IPv6, st.Countries, st.Bytes, st.Skipped)
}
