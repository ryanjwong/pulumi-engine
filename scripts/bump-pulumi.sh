#!/usr/bin/env bash
# Bump the pinned pulumi/pulumi modules (pkg/v3 and sdk/v3 together), tidy,
# build, run the unit tests and the upstream drift check, and print the
# commands for the parts that need plugins, network and the pulumi CLI.
#
#   scripts/bump-pulumi.sh v3.240.0            # bump + unit + drift check
#   scripts/bump-pulumi.sh v3.240.0 --check    # do not modify go.mod/go.sum on failure (CI probe)
#
# Exit codes: 0 compatible; 1 something failed (the output says what); 2 usage.
set -uo pipefail

version="${1:-}"
mode="${2:-}"
if [ -z "$version" ]; then
  echo "usage: $0 vX.Y.Z [--check]" >&2
  exit 2
fi
case "$version" in v*) ;; *) version="v$version" ;; esac

cd "$(dirname "${BASH_SOURCE[0]}")/.."
current=$(go list -m -f '{{.Version}}' github.com/pulumi/pulumi/pkg/v3)
echo "==> pulumi pkg/v3: $current -> $version"

status=0
step() {
  echo "==> $*"
  if ! "$@"; then
    echo "FAILED: $*" >&2
    status=1
    return 1
  fi
}

step go get "github.com/pulumi/pulumi/pkg/v3@$version" "github.com/pulumi/pulumi/sdk/v3@$version" || exit 1
# pkg/v3's tests import packages some sdk versions lack; -e keeps tidy going.
step go mod tidy -e
step go build ./...
step go vet ./engine/... ./capi/... ./internal/...
step go test ./... -count=1
step go run ./internal/upstream/cmd/upstreamcheck

if [ "$mode" != "--check" ]; then
  # Keep the CI's parity CLI version and the README table in step with the pin.
  bare="${version#v}"
  sed -i.bak -E "s/^(  PULUMI_CLI_VERSION: \")[0-9.]+(\")/\1${bare}\2/" .github/workflows/ci.yml && rm -f .github/workflows/ci.yml.bak
  sed -i.bak -E "s/(\| \`github.com\/pulumi\/pulumi\/pkg\/v3\`, \`sdk\/v3\` \| \*\*)v[0-9.]+(\*\*)/\1${version}\2/" README.md && rm -f README.md.bak
fi

cat <<MSG

Bump to $version: $([ $status -eq 0 ] && echo OK || echo FAILED)

Next (need plugins, network, and the pulumi CLI at $version on PATH):
  make integration                  # real providers, YAML host
  make parity                       # CLI parity subset (PULUMI_ENGINE_CLI=/path/to/pulumi to pick a CLI)
  PULUMI_ENGINE_RECORD_DIR=\$PWD/engine/testdata/events make integration   # re-record event fixtures if events changed
  make lib abitest node             # shared library, C ABI test, Node binding
If upstream-check reported drift: review the diff, port what matters into
internal/upstream, then 'make upstream-update' and re-run 'make upstream-check'.
Then update the compatibility table in README.md and docs/cli-parity.md.
MSG
exit $status
