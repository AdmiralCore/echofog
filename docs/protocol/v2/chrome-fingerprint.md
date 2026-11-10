# Keeping the ClientHello profile in step with Chrome (T2.6d, H6)

The `reality2-quic` and `reality2-tcp` clients imitate the current stable Chrome (wire.md §3.1, §4.1).
A profile one stable version behind is a signature, so it is checked against a live Chrome.

## What is checked

`internal/v2/carrier/reality2/chromefp` parses a ClientHello into a secret-free `Sample` (JA4, ciphers,
extension set, GREASE places, groups, signature algorithms, key shares with sizes — the post-quantum
share included —, ALPN, ALPS, certificate compression, ECH shape, QUIC transport parameter ids and
small values) and `Compare` lists the differences. Chrome permutes the extension order per connection,
so the set is compared. The ECH payload length is random and ignored.

* `TestProfileMatchesStoredChromeSample` — CI: our profile against `testdata/chrome-<version>-<tcp|quic>.json`.
  No browser needed.
* `TestLiveChrome` — runs the installed stable Chrome headless (fresh profile, `--host-resolver-rules`
  mapping only a `.test` name to loopback, everything else unresolvable) against a loopback TCP/UDP
  server that records the ClientHello and the QUIC Initial. It takes three captures per transport,
  requires them to agree, compares our profile with the live Chrome and, when the stored sample is of
  the same version, the live Chrome with that sample. A stored sample of another version (a CI runner
  lags or leads the machine that refreshed `testdata`) is logged as drift and skips only the
  stored-sample comparison: our profile is still checked against the Chrome that is installed.
  It skips with a reason when no Chrome is found (`CHROME_BIN` overrides the search). Nothing leaves
  the machine.
* `TestStaleProfileIsRed` — a Chrome 120 profile must not match the current sample.

## When a new stable Chrome comes out (about every 4 weeks) and before each EchoFog release

1. `cd echofog-core-oss && GOWORK=off CHROMEFP_UPDATE=1 go test ./internal/v2/carrier/reality2/chromefp -run TestLiveChrome -v`
   writes `testdata/chrome-<version>-{tcp,quic}.json` and lists every divergence of our profile.
2. A divergence is a security-review item, not a quiet fix: record it in the task report, decide the
   profile change (uTLS preset in `utls.HelloChrome_Auto`, fork `chrome_parrot.go` / `ECHOFOG-PATCH.md`),
   make it, and re-run the test.
3. Divergences that are seen but not yet decided stay in `testdata/known-divergences.json`
   (`CHROMEFP_ACCEPT=1` rewrites it from the live result; the git diff is the review). A divergence
   outside the list fails; a listed one that disappears fails too.
4. Delete stored samples older than the previous stable version.

A profile more than one stable version behind blocks the release (checked in T15.3).
