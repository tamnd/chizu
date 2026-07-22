#!/usr/bin/env sh
# Skip-depth sweep. Unlike the device labs this is a deterministic
# counting simulation: rows are a pure function of the seed and carry
# no perf claim, so it can run anywhere, including a laptop.
# Usage: run.sh <label>   e.g. run.sh sim
set -eu

label="${1:?usage: run.sh <label>}"
dir="$(dirname "$0")"

go run "$dir" -label "$label" -docs 10000000 -queries 400
go run "$dir" -label "$label" -docs 500000000 -queries 400 | tail -n +2
