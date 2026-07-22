#!/usr/bin/env sh
# Read-planner sweep. Run on the gate box (server3), never a laptop,
# and only when the box is otherwise quiet: this lab measures tail
# latency and co-tenant load pollutes p99.9 (the 10M fixture build
# showed a 6x throughput swing from load alone).
# Needs the cross-compiled readplanner binary next to this script,
# root for /proc/sys/vm/drop_caches, and ~50 GB free on the disk.
# Usage: run.sh <label>   e.g. run.sh server3
set -eu

label="${1:?usage: run.sh <label>}"
dir="$(dirname "$0")"

echo "# host: $label  $(uname -sm)  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2)"
echo "# load: $(cat /proc/loadavg 2>/dev/null)"

"$dir/readplanner" -label "$label" -file "$dir/readplanner.dat" -size 48 -sec 2
