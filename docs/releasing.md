# Releasing

A release is a git tag `vX.Y.Z` (or `vX.Y.Z-rc.1` for a pre-release) on
`master`. Pushing the tag runs [`release.yml`](../.github/workflows/release.yml),
which builds, verifies, attaches and publishes everything below. Nothing in
the workflow runs on a branch push or a pull request, and nothing creates a
tag: the tag is the human's decision.

```
git checkout master && git pull
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

## What a release contains

| artifact | where | built by |
|-|-|-|
| `libpulumi-<os>-<arch>.tar.gz` (+ `.sha256`, and one `SHA256SUMS` for the set) for darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Each tarball holds `libpulumi.{dylib,so}`, `libpulumi.h` and a `SHA256SUMS` of both. | the GitHub release for the tag, with generated notes | `make dist-lib` on each platform's runner (`scripts/dist-lib.sh`) |
| `@ryanjwong/pulumi-engine-node` (the JavaScript) and `@ryanjwong/pulumi-engine-node-<os>-<arch>` (the shared library only), one per platform above | GitHub Packages (npm registry `https://npm.pkg.github.com`); the same `.tgz` files are also attached to the workflow run as the `npm-packages` artifact | `scripts/package-node.mjs` |
| the Python binding | not published by the workflow; `pip install` from the repository (see the README) with the library from a tarball above | `make dist-python` builds a wheel and sdist locally |

The version stamped into everything is the tag without its `v`:
`engine.LibraryVersion` (through `-ldflags -X`, so `pulumi_version()`,
`version()` in Node and `pulumi_engine.version()` in Python report it) and the
`version` of every npm package. Development builds report `0.1.0-dev` from
the library and `0.0.0-dev` from `make release-dry-run`.

## How each platform is built and verified

Every platform builds on its own **native** GitHub-hosted runner; there is
no cross compilation in the release path:

| platform | runner | verification on that runner |
|-|-|-|
| darwin-arm64 | `macos-15` (Apple silicon) | `make abitest` (a cgo test binary links the built library through its header and drives it) and a Node load (`make node-build` + `version()` through koffi must report the tag's version) |
| darwin-amd64 | `macos-15-intel` | same |
| linux-amd64 | `ubuntu-24.04` | same |
| linux-arm64 | `ubuntu-24.04-arm` | same |

The job first checks `go env GOOS/GOARCH` against the platform it is meant
to build, so a runner label that silently resolves to another architecture
fails the job instead of producing a mislabeled tarball. The verification is
what "loads on the target" means here: the library is `dlopen`ed and called
on the machine type that will run it, from both a C (cgo) and a Node (koffi)
caller.

If a native runner ever goes away, the fallback is a cgo cross build (Apple's
clang cross-compiles darwin/amd64 from arm64 with `GOARCH=amd64 CGO_ENABLED=1`
and nothing else; for Linux `CC="zig cc -target aarch64-linux-gnu"` works)
with the verification step run under Rosetta (`arch -x86_64`) or QEMU user
mode (`docker/setup-qemu-action` + an arm64 container that runs the ABI
test). None of that is wired up because all four targets have native
runners, on private repositories too.

## The Node packages

The layout is esbuild's: the main package has no native code and declares
one `optionalDependencies` entry per platform package; npm (and pnpm, yarn)
installs only the one whose `os`/`cpu` fields match the host. At load time
`bindings/nodejs/src/native.ts` looks, in order, at `$PULUMI_ENGINE_LIB`,
the platform package named in the package's own `pulumiEngine.platformPackages`
map for `<os>-<arch>` (resolved from the package's location, so hoisted and
nested installs both work), the in-tree `lib/native/` copy `make node`
makes, and the repository's `build/`. A missing platform package is a clear
error naming the package to install, not a segfault or an ENOENT.

**Names.** GitHub Packages requires an npm scope equal to the repository
owner, so the published names are `@ryanjwong/pulumi-engine-node` and
`@ryanjwong/pulumi-engine-node-<os>-<arch>`. The source package keeps
`@pulumi-engine/node` as its `name` (imports, tests and the pnpm workspace
use it); `package-node.mjs` rewrites the name at packaging time and records
the source name in `pulumiEngine.sourceName`. Consumers who want the import
name stable install an alias:

```
npm install @pulumi-engine/node@npm:@ryanjwong/pulumi-engine-node@0.2.0
```

with `.npmrc` pointing the scope at GitHub Packages:

```
@ryanjwong:registry=https://npm.pkg.github.com
//npm.pkg.github.com/:_authToken=${GITHUB_TOKEN}
```

(GitHub Packages needs a token with `read:packages` even for public
packages.) `import { openStack } from "@pulumi-engine/node"` then resolves
to the published package.

**Publishing safety.** The `npm` job publishes only on a tag, and skips
cleanly (a workflow warning, exit 0) when there is no token, when `npm
whoami` against the registry fails, when the version already exists (so a
re-run of the workflow is idempotent) or when `npm publish` is refused. The
token is `NPM_PUBLISH_TOKEN` when that repository secret exists (a PAT with
`write:packages`), otherwise the workflow's own `GITHUB_TOKEN`, which can
publish packages linked to this repository because the workflow requests
`packages: write`. Platform packages publish before the main package so the
main package's optional dependencies resolve as soon as it appears.

## Local dry run

```
make release-dry-run VERSION=0.2.0
```

builds, for the host platform only, the same things the workflow builds:
`dist/libpulumi-<os>-<arch>.tar.gz` (+ `.sha256`), runs the ABI test
against it, packs `dist/npm/*.tgz` (the main package listing all four
platforms as optional dependencies, plus the host's platform package) and
`dist/python/` (wheel + sdist). Nothing is published. CI runs the same
target on every pull request (`release-dry-run` job) and then installs the
packed Node tarballs into a scratch project to check that the library is
resolved from the platform package.

To try the packed Node package locally:

```
cd $(mktemp -d) && npm init -y
npm install --registry https://registry.npmjs.org \
  <repo>/dist/npm/ryanjwong-pulumi-engine-node-0.2.0.tgz \
  <repo>/dist/npm/ryanjwong-pulumi-engine-node-darwin-arm64-0.2.0.tgz
node -e 'const b = require("@ryanjwong/pulumi-engine-node"); console.log(b.libraryPath(), b.version())'
```

(The other platforms' optional dependencies are not on the public registry
and are skipped, as they would be for a non-matching platform.)

## Checklist

1. `master` is green (unit, integration, parity, abitest, node, python,
   release-dry-run).
2. The compatibility table in the README names the Pulumi versions this
   release was tested with.
3. Tag and push. Watch the `release` workflow; the GitHub release appears
   when all four platform builds have passed.
4. Check the release page lists five files (four tarballs and `SHA256SUMS`,
   plus the `.sha256` sidecars) and that GitHub Packages shows the five npm
   packages at the new version.
