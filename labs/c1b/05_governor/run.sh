#!/bin/sh
# Sweep for lab 05 governor. Timing lab: the deciding rows run on
# server3 (quiet, after the gate run exits); see PREDICTION.md.
set -eu

cd "$(dirname "$0")"
label="${1:-$(hostname -s)}"
file="${2:-../01_readplanner/readplanner.dat}"
out="results"
mkdir -p "$out"

go build -o governor .

first=1
for budget in 6 8 10 12; do
    sync
    if [ -w /proc/sys/vm/drop_caches ]; then
        echo 3 >/proc/sys/vm/drop_caches
    fi
    if [ "$first" = 1 ]; then
        ./governor -label "$label" -file "$file" -budget "$budget" -workers 8 -queries 2000 >"$out/sweep.tsv"
        first=0
    else
        ./governor -label "$label" -file "$file" -budget "$budget" -workers 8 -queries 2000 | tail -n +2 >>"$out/sweep.tsv"
    fi
done

echo "rows written to $out/sweep.tsv"
