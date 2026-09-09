# pulumi-engine

The Pulumi deployment engine as an **in-process library**.

No CLI. No daemon. No sidecar. The Go packages that `pulumi up` is a thin
frontend over (`pkg/v3/engine`, `pkg/v3/resource/deploy`, `pkg/v3/backend`,
`pkg/v3/secrets`, `sdk/v3/go/common/workspace`) are linked into your process
and driven directly. Provider plugins remain subprocesses because the Pulumi
provider protocol defines them that way; the library starts and owns them
inside the caller's process tree.

Three layers, one implementation:

| layer | package | status |
|-|-|-|
| Go library | `github.com/ryanjwong/pulumi-engine/engine` | bootstrapping |
| C ABI (`libpulumi`) | `capi` (`go build -buildmode=c-shared`) | bootstrapping |
| Node binding | `bindings/nodejs` (`@pulumi-engine/node`, via koffi FFI) | bootstrapping |

This is the repository skeleton. Phase one (the working engine library, C ABI
and Node binding) lands on a pull request; see that PR and the README on the
phase-one branch for design decisions, quick starts and the compatibility
policy.

## Build

```
make build        # compile
make test         # unit tests
make integration  # real providers, needs network (PULUMI_ENGINE_INTEGRATION=1)
make lib          # build/libpulumi.{dylib,so} + header for this host
make node         # build and test the Node binding against build/libpulumi.*
```

Toolchain: Go 1.26 with cgo, Node 22+ and pnpm for the binding.

## License

Apache-2.0, copyright Ryan Wong. Portions copied from
[pulumi/pulumi](https://github.com/pulumi/pulumi) (Apache-2.0, Pulumi
Corporation) are marked in place.
