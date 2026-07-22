#!/bin/sh
# Sweep for lab 04 traversal. Deterministic counting rows, host
# independent, runnable anywhere (see PREDICTION.md); the label names
# the host anyway for honesty in the results file.
set -eu

cd "$(dirname "$0")"
label="${1:-$(hostname -s)}"
out="results"
mkdir -p "$out"

go build -o traversal .

./traversal -label "$label" -docs 1000000 -queries 200 -k 1000 >"$out/sweep.tsv"
./traversal -label "$label" -docs 10000000 -queries 200 -k 1000 | tail -n +2 >>"$out/sweep.tsv"

echo "rows written to $out/sweep.tsv"
