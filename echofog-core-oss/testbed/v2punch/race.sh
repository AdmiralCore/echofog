#!/bin/bash
# -race over internal/v2 in WSL Ubuntu as root (README section 4); run from anywhere.
export PATH=/opt/gosdk/go/bin:$PATH
cd /mnt/d/projects/echofog-wt/wt-v2-6.7-birthday/echofog-core-oss || exit 2
export GOWORK=off GOFLAGS="-mod=mod -buildvcs=false"
pkgs=$(go list ./internal/v2/...)
echo "packages: $(echo "$pkgs" | wc -l)"
go test -race -count=2 $pkgs 2>&1 | grep -E "^(ok|FAIL|--- FAIL|panic|WARNING: DATA RACE)" | sort | uniq -c | sort -rn | head -60
