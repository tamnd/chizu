#!/bin/sh
# Importer lab: fetch + transform on a real box. Needs a wet.paths file
# (gunzip of crawl-data/CC-MAIN-*/wet.paths.gz) in $data.
set -eu
dir=$(cd "$(dirname "$0")" && pwd)
data=${1:?usage: run.sh <datadir> [label]}
label=${2:-$(hostname -s)}

go run "$dir" fetch -paths "$data/wet.paths" -n 8 -conc 4 -label "$label" -keep "$data/wet"
go run "$dir" transform -wet "$(ls "$data"/wet/*.wet.gz | head -2 | paste -sd, -)" -reps 3 -label "$label"
