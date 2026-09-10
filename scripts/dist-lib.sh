#!/usr/bin/env bash
# Copyright 2026 Ryan Wong
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Package the built shared library for one platform as a release tarball:
#
#   dist/libpulumi-<os>-<arch>.tar.gz     libpulumi.{dylib,so} + libpulumi.h + SHA256SUMS
#   dist/libpulumi-<os>-<arch>.tar.gz.sha256
#
#   scripts/dist-lib.sh [build-dir] [dist-dir] [os-arch]
#
# Defaults: build, dist, and the Go host platform (`go env GOOS/GOARCH`).
set -euo pipefail

build=${1:-build}
dist=${2:-dist}
key=${3:-"$(go env GOOS)-$(go env GOARCH)"}

case "$key" in
darwin-*) ext=dylib ;;
*) ext=so ;;
esac
lib="$build/libpulumi.$ext"
header="$build/libpulumi.h"
[ -f "$lib" ] || { echo "dist-lib: $lib not found (run 'make lib' first)" >&2; exit 2; }
[ -f "$header" ] || { echo "dist-lib: $header not found" >&2; exit 2; }

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

mkdir -p "$dist"
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cp "$lib" "$header" "$stage/"
(cd "$stage" && sha256 "libpulumi.$ext" libpulumi.h > SHA256SUMS)
name="libpulumi-$key.tar.gz"
# Deterministic member order and no owner names, so two builds of the same
# input differ only where the binary does.
tar -C "$stage" --no-xattrs -czf "$dist/$name" "libpulumi.$ext" libpulumi.h SHA256SUMS 2>/dev/null ||
    tar -C "$stage" -czf "$dist/$name" "libpulumi.$ext" libpulumi.h SHA256SUMS
(cd "$dist" && sha256 "$name" > "$name.sha256")
echo "built $dist/$name ($(cat "$dist/$name.sha256" | cut -d' ' -f1))"
