#!/usr/bin/env sh
# zstd-dict sweep. Ratio rows are host-independent; encode/decode MB/s
# rows bind on server3 (E-box). Prep once, sweep from the record files:
#
#   go run . prep -wet a.wet.gz,b.wet.gz -out data/text.rec
#   go run . prep -wat a.wat.gz,b.wat.gz -out data/links.rec
#   run.sh <label> <datadir>
set -eu

label="${1:?usage: run.sh <label> <datadir>}"
data="${2:?usage: run.sh <label> <datadir>}"
dir="$(dirname "$0")"

go run "$dir" sweep -text "$data/text.rec" -links "$data/links.rec" -label "$label" -reps 3
