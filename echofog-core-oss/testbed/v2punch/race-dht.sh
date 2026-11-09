#!/bin/bash
export PATH=/opt/gosdk/go/bin:/usr/bin:/bin
cd /mnt/d/projects/echofog-wt/wt-v2-6.7-birthday/echofog-core-oss || exit 2
export GOWORK=off GOFLAGS="-mod=mod -buildvcs=false"
go test -race -count=1 -timeout 20m ./internal/v2/dht 2>&1 | tail -5
