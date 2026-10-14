#!/usr/bin/env bash
# Capture loopback while the generator runs, then classify with ndpiReader.
set -euo pipefail

PCAP=/tmp/echofog.pcap

echo "== ndpiReader version =="
ndpiReader -v 2>/dev/null | head -1 || ndpiReader --version 2>/dev/null | head -1 || true

tcpdump -i lo -w "$PCAP" -U tcp 2>/tmp/tcpdump.log &
TCPDUMP_PID=$!
sleep 1

ndpigen

sleep 1
kill "$TCPDUMP_PID" 2>/dev/null || true
wait "$TCPDUMP_PID" 2>/dev/null || true

echo
echo "== packets captured =="
tcpdump -r "$PCAP" 2>/dev/null | wc -l

echo
echo "== ndpiReader classification (-t: TLS/SNI details) =="
# -i read file, -v 2 verbose per-flow, -t enable TLS cert/SNI dissection
ndpiReader -i "$PCAP" -v 2 2>/dev/null | sed -n '1,200p'
