#!/usr/bin/env sh
# Chain-rate sweep. Needs CHIZU_S3_ENDPOINT (and credentials) in the
# environment and the chainrate binary next to this script.
# Usage: run.sh <label>   e.g. run.sh server2-to-server3
set -eu

label="${1:?usage: run.sh <label>}"
bin="$(dirname "$0")/chainrate"

# Design-load arm: the PRED-CHIZU-C0A-CHAIN headline. Three contenders at
# 1 append/s each, 8 records per append = 24 records/s across 3 appends/s.
"$bin" -label "$label-design" -contenders 3 -records 8 -pace 1 -duration 60s

# Saturation sweep: contention and batch-size behavior.
for c in 1 2 4 8; do
	for r in 1 8 32; do
		"$bin" -label "$label-sat" -contenders "$c" -records "$r" -duration 30s
	done
done
