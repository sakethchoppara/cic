#!/bin/sh
# Cold vs warm latency table, then the 50-worker, 5-minute load test.
# Results go to bench/results.md. Takes ~35 minutes; needs scripts/images.sh first.
set -e
cd "$(dirname "$0")/.."
out=bench/results.md
{
	echo "# Benchmark results"
	echo
	echo "$(date -u +%Y-%m-%d), $(nproc) CPUs, Docker $(docker version --format '{{.Server.Version}}'), $(go version | cut -d' ' -f3)."
	echo
	echo "## Cold vs warm latency"
	echo
	go run ./bench -mode latency
	echo
	echo "## Load test"
	echo
	echo '```'
	go run ./bench -mode load 2>&1
	echo '```'
} > "$out.tmp"
mv "$out.tmp" "$out"
cat "$out"
