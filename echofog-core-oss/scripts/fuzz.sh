#!/usr/bin/env bash
# Runs every fuzz target for a while: scripts/fuzz.sh [seconds per target, default 30].
# New failing inputs land in testdata/fuzz/ next to the test; commit them, they become regression tests.
set -u
secs="${1:-${FUZZTIME:-30}}"
fail=0
for pkg in $(go list ./...); do
  for target in $(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz'); do
    echo "=== $pkg $target (${secs}s)"
    if ! go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "${secs}s" -timeout 0; then
      fail=1
    fi
  done
done
exit $fail
