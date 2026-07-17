#!/usr/bin/env sh
# Decode-rate sweep. Run on the gate box (server3), never a laptop.
# Needs the cross-compiled decoderate binary and ref.c next to this
# script, and a C compiler on the box for the K2 reference arm.
# Usage: run.sh <label>   e.g. run.sh server3
set -eu

label="${1:?usage: run.sh <label>}"
dir="$(dirname "$0")"

echo "# host: $label  $(uname -sm)  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2)"
echo "# columns: label arm block width_or_tier postings M/s GBin/s GBout/s"

"$dir/decoderate" -label "$label" -sec 0.7

cc -O3 -march=native -o "$dir/cref/cref" "$dir/cref/ref.c"
"$dir/cref/cref" "$label" 0.7
