#!/usr/bin/env bash
# Pins the import edges from spec 2107 doc 02 section 2:
#   - no internal/ directories anywhere
#   - coldfmt, hotfmt, and wire import stdlib only
#   - s3c imports stdlib only; chain imports only s3c and wire
#   - nothing imports a plane package (crawl, build, serve, rootsrv) except cmd/chizu
#   - no AWS SDK anywhere; s3c is the only S3 client
# Packages that do not exist yet are skipped, so the script is green from the
# first commit and tightens as packages land.
set -euo pipefail
cd "$(dirname "$0")/.."
module=$(head -1 go.mod | awk '{print $2}')
fail=0

if find . -type d -name internal -not -path './.git/*' | grep -q .; then
  echo "internal/ directories are banned:"
  find . -type d -name internal -not -path './.git/*'
  fail=1
fi

if grep -rln --include='*.go' 'aws-sdk-go' . >/dev/null 2>&1; then
  echo "AWS SDK import found; s3c is the only S3 client:"
  grep -rln --include='*.go' 'aws-sdk-go' .
  fail=1
fi

# check <pkg> <comma-separated allowed module-internal imports>
check() {
  pkg=$1
  allowed=$2
  [ -d "$pkg" ] || return 0
  imports=$(go list -f '{{join .Imports "\n"}}' "./$pkg" | grep "^$module/" | sed "s|^$module/||" || true)
  for imp in $imports; do
    case ",$allowed," in
      *",$imp,"*) ;;
      *)
        echo "$pkg imports $imp (allowed: ${allowed:-nothing})"
        fail=1
        ;;
    esac
  done
}

check coldfmt ""
check hotfmt ""
check wire ""
check s3c ""
check chain "s3c,wire"

for plane in crawl build serve rootsrv; do
  [ -d "$plane" ] || continue
  offenders=$(go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./... |
    grep -v "^$module/cmd/" | grep -v "^$module/$plane:" |
    grep -E " $module/$plane( |$)" || true)
  if [ -n "$offenders" ]; then
    echo "plane $plane is imported outside cmd/chizu:"
    echo "$offenders"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "import boundary: FAIL"
  exit 1
fi
echo "import boundary: ok"
