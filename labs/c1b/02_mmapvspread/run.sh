#!/usr/bin/env sh
# mmap-vs-pread sweep. Run on the gate box (server3), never a laptop,
# and only when the box is otherwise quiet: the antagonists ARE the
# load, and uncontrolled co-tenant load on top of them muddies the
# comparison this lab exists to make.
# Needs the cross-compiled mmapvspread binary next to this script,
# root for /proc/sys/vm/drop_caches, and ~50 GB free on the disk.
# Reuses lab 01's backing file when present so the disk pays once.
# Usage: run.sh <label>   e.g. run.sh server3
set -eu

label="${1:?usage: run.sh <label>}"
dir="$(dirname "$0")"

file="$dir/mmapvspread.dat"
if [ -f "$dir/../01_readplanner/readplanner.dat" ]; then
    file="$dir/../01_readplanner/readplanner.dat"
fi

echo "# host: $label  $(uname -sm)  $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2)"
echo "# load: $(cat /proc/loadavg 2>/dev/null)"

"$dir/mmapvspread" -label "$label" -file "$file" -size 48 -sec 2 -hog 12
