#!/bin/sh
# Dict-size sweep. Run on the box named in -label; results are TSV on
# stdout, save under results/<date>-<label>.txt.
set -eu

dir="$(cd "$(dirname "$0")" && pwd)"
label="${1:-local}"

go build -o "$dir/dictsize" "$dir"
"$dir/dictsize" -label "$label" -terms 4000000 -sec 2
