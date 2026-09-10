# pulumi-engine

The Pulumi deployment engine as an **in-process library**.

No CLI. No daemon. No sidecar. The Go packages that `pulumi up` is a thin
frontend over (`pkg/v3/engine`, `pkg/v3/resource/deploy`, `pkg/v3/backend`,
`pkg/v3/secrets`, `sdk/v3/go/common/workspace`) are linked into your process
and driven directly. Provider plugins remain subprocesses because the Pulumi
provider protocol defines them that way; the library starts and owns them
inside the caller's process tree, exactly like the CLI does.

Three layers, one implementation:

| layer | what | where |
|-|-|-|
| Go library | `Open`, `Stack.Preview/Up/Refresh/Destroy`, events, typed errors | `engine/` (`github.com/ryanjwong/pulumi-engine/engine`) |
| C ABI | `libpulumi.{dylib,so}`, handles + JSON, no callbacks | `capi/` (`go build -buildmode=c-shared`) |
| Node binding | `@pulumi-engine/node`, koffi FFI, async events, inline programs | `bindings/nodejs/` |

## What it is not

- It is not the `pulumi` CLI. Nothing shells out to `pulumi`; the CLI is not
  required at build or run time. The repository uses it only to compare
  behaviour.
- It is not a daemon. There is no long-running service; every operation runs
  in the caller's goroutines/threads and finishes when `Wait` returns.
- It does not replace provider plugins. `pulumi-resource-*` and
  `pulumi-language-*` binaries are still child processes, started and stopped
  per operation by Pulumi's own plugin host, and downloaded to `~/.pulumi/plugins`
  on first use by Pulumi's own installer.

## Program modes

| mode | Go | C ABI / Node | how it runs |
|-|-|-|-|
| in-process | `GoProgram(func(*pulumi.Context) error)` | (Node: `async () => {...}`, see inline below) | A LanguageRuntime gRPC service is served from a goroutine in this process; the engine connects to it as the CLI would with `--client`. The Go SDK runs the function with `pulumi.RunWithContext` against the engine's resource monitor. No subprocess. |
| callback | `CallbackProgram{Address}` | `{"mode":"callback","address":...}` | A LanguageRuntime gRPC server the caller runs. This is the mechanism behind `pulumi up --client` and behind inline Automation API programs in every SDK. |
| local | `LocalProgram{Dir}` | `{"mode":"local","dir":...}` | The project in `Dir` (`Pulumi.yaml`) is executed by the stock `pulumi-language-<runtime>` plugin, installed on demand. The YAML host needs no toolchain; Node/Python/Go hosts need theirs. |

Refresh and destroy do not need a program (`nil` in Go, omitted over the ABI).

The Node binding's **inline** mode (`stack.up(async () => { new random.RandomPet(...) })`)
starts `@pulumi/pulumi`'s own `LanguageServer` (`automation/server`, the class
the Automation API uses for inline programs) on a loopback port and passes it
as a callback program. The program runs in the Node process; the tests check
`process.pid` to prove it.

## Quick start: Go

```go
import (
    "context"
    "github.com/pulumi/pulumi-random/sdk/v4/go/random"
    "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
    "github.com/ryanjwong/pulumi-engine/engine"
)

st, err := engine.Open(ctx, engine.StackSpec{
    Name:    "dev",
    Project: engine.ProjectSpec{Name: "demo"},
    Backend: engine.BackendSpec{URL: "file:///var/lib/demo/state"},
    Secrets: engine.SecretsSpec{Provider: "passphrase", Passphrase: os.Getenv("DEMO_PASSPHRASE")},
    Config:  map[string]engine.ConfigValue{"apiKey": {Value: "...", Secret: true}},
    Create:  true,
})

op := st.Up(ctx, engine.GoProgram(func(ctx *pulumi.Context) error {
    pet, err := random.NewRandomPet(ctx, "pet", nil)
    if err != nil { return err }
    ctx.Export("name", pet.ID())
    return nil
}), engine.Options{Message: "first deploy"})

for ev := range op.Events() {           // engine events, secrets redacted
    if ev.ResOutputsEvent != nil { log.Println(ev.ResOutputsEvent.Metadata.Op, ev.ResOutputsEvent.Metadata.URN) }
}
res, err := op.Wait()                    // typed errors: errors.As(err, &engine.ResourceOpFailed{})
fmt.Println(res.Changes, res.Outputs.Values["name"])
```

`Stack` also has `Preview`, `Refresh`, `Destroy`, `Outputs`, `Export`,
`Import`, `SetConfig`, `GetConfig`, `RemoveConfig`, `Cancel`, `Remove`.
Cancelling the `ctx` passed to an operation (or calling `Operation.Cancel`)
cancels gracefully: no new steps start, in-flight steps finish (providers get
`SignalCancellation`), the checkpoint is written, then `Wait` returns
`Cancelled`. A second `Cancel` terminates immediately, like a second Ctrl-C.

## Quick start: Node

```ts
import * as random from "@pulumi/random";
import { openStack, Secret, ResourceOpFailedError } from "@pulumi-engine/node";

const stack = await openStack({
    name: "dev",
    project: { name: "demo" },
    backend: { url: "file:///var/lib/demo/state" },
    secrets: { provider: "passphrase", passphrase: process.env.DEMO_PASSPHRASE! },
    config: { apiKey: new Secret("...") },
    create: true,
});

const ac = new AbortController();
const up = await stack.up(async () => {
    const pet = new random.RandomPet("pet", {});
    return { name: pet.id };
}, { signal: ac.signal });

for await (const e of up) console.log(e.type);   // AsyncIterable<Event>
try {
    const res = await up.result();                // typed Error subclasses
    console.log(res.changes, res.outputs?.values.name);
} catch (e) {
    if (e instanceof ResourceOpFailedError) console.error(e.urn, e.op, e.message);
} finally {
    await up.release();
}
```

Programs: `{ mode: "local", dir }`, `{ mode: "callback", address }`, or an
async function (inline; needs `@pulumi/pulumi` and `@grpc/grpc-js` in your
project). The binding never blocks the event loop: the two blocking ABI calls
run on koffi's async thread pool. See `bindings/nodejs/README.md`.

## Backends, secrets, credentials

The backend is chosen by `Backend.URL`: `file://`, `s3://`, `gs://`,
`azblob://` select Pulumi's DIY backend (through the same package the CLI
uses, so object-store credentials come from the cloud SDK environment as they
do for the CLI); `https://` selects the HTTP backend (Pulumi Cloud or a
self-hosted implementation).

Secrets providers per stack: `passphrase` (default for DIY; the passphrase is
a spec field, never `PULUMI_CONFIG_PASSPHRASE`), `service` (default for HTTP),
a KMS URL (`awskms://`, `gcpkms://`, `azurekeyvault://`, `hashivault://`), or
`b64` for tests. Existing key material (the passphrase salt, the KMS data key)
is picked up from `Pulumi.<stack>.yaml` in `Project.Dir` or from the
checkpoint, so a stack keeps one encryption context across processes. The
library never prompts.

Configuration comes from `Pulumi.<stack>.yaml` (if `Project.Dir` is set)
overlaid with `StackSpec.Config`; `SetConfig` writes back to that file, like
`pulumi config set`. Project-level config schema and defaults from
`Pulumi.yaml` are applied and validated for local programs.

## Events and errors

`Event` embeds Pulumi's wire type `apitype.EngineEvent` (the `--json` /
event-log format) and adds `Type` (`prelude`, `summary`, `resourcePre`,
`resourceOutputs`, `resourceOpFailed`, `diagnostic`, `stdout`, `policy*`,
`progress`, ...). Property values are secret-redacted (`[secret]`) unless
`Options.ShowSecrets` is set. The engine's terminating `cancel` event is
consumed by the library; the closed channel is the terminator.

Typed errors (`errors.As` / `engine.KindOf` / `error.kind` over the ABI):
`InvalidSpec{Field}`, `ResourceOpFailed{URN,Type,Op,Provider,Message}`
(all failures are in `Result.Failures`), `ProgramFailed{Message}`,
`ConcurrentUpdate`, `StackNotFound`, `StackExists`, `PendingOperations{URNs}`,
`Cancelled{Operation}`, `Unclassified{Err}`.

## How it drives Pulumi

The operation path mirrors `pkg/cmd/pulumi/operations/{up,preview,refresh,destroy}.go`
minus cobra and the terminal UI: build a `backend.UpdateOperation` (project,
root, config, secrets manager and provider, cancellation scopes, engine and
display options) and call `backend.PreviewStack/UpdateStack/RefreshStack/DestroyStack`
with `AutoApprove` and `SkipPreview` (what the Automation API passes as
`--yes --skip-preview`). The backends keep doing their own locking, history
and checkpoint writing.

Events for preview and up come from the `chan engine.Event` those backend
methods accept, translated with `display.ConvertEngineEvent` (the CLI's own
JSON translation, which does the redaction). Pulumi's `Backend.Refresh` and
`Backend.Destroy` take no event channel; their events are only visible to the
display layer, which can stream them to an `Events` gRPC service when
`display.Options.EventLogPath` is `tcp://<addr>` (the path Pulumi built for
the Automation API). The library hosts that service on loopback per operation
and receives the same JSON events. No file is written or tailed. One
consequence: `ShowSecrets` is honoured for preview/up events, while refresh
and destroy events are always redacted (`logJSONEvent` hard-codes it).

Cancellation replaces the CLI's SIGINT scope with a `CancellationScopeSource`
that wires `Operation.Cancel`/context cancellation to the engine's
`cancel.Context`. The backend itself runs under `context.WithoutCancel` so the
checkpoint write after a cancel is never interrupted.

Copied from pulumi/pulumi (Apache-2.0, attributed in place):
`engine/langserver.go` is the unexported in-process LanguageRuntime server
from `sdk/v3/go/auto/stack.go`, so that the `auto` package (which shells out
to the CLI) is not a dependency. Nothing reaches into `internal` packages.

## Compatibility surface and bump policy

`github.com/pulumi/pulumi/pkg/v3` and `sdk/v3` are pinned at **v3.237.0**.
**These packages are not a stable API.** `pkg/v3` in particular is the CLI's
implementation, and Pulumi changes signatures (`backend.UpdateOperation`,
`engine.UpdateOptions`, display options, secrets constructors) between minor
releases. Every bump is a compatibility event for this repository: run the
unit and integration suites, re-record the event fixtures, re-read the
`operations/*.go` files for changed flags, and expect to touch this library.
Provider plugins and language hosts are versioned independently and are not
affected by the pin. The test-only `pulumi-random` Go SDK is pinned at
v4.16.8 because newer releases require a newer `sdk/v3`.

## C ABI (`libpulumi`)

Built by `make lib` into `build/libpulumi.{dylib,so}` with the generated
`build/libpulumi.h` (the contract is also in the header's preamble).
CI builds darwin/arm64 and linux/amd64 and runs the ABI test against the
built library (`make abitest`).

| function | purpose |
|-|-|
| `pulumi_version()` | version string |
| `pulumi_stack_open(spec_json, &err)` | handle > 0, or 0 with `err` |
| `pulumi_op_start(handle, request_json, &err)` | op id > 0; request `{"kind","program","options"}` |
| `pulumi_op_next_event(op, timeout_ms)` | event JSON, `""` on timeout, `NULL` at end |
| `pulumi_op_cancel(op)` | graceful cancel; second call terminates |
| `pulumi_op_wait(op, &err)` | result JSON, or `NULL` with error JSON (`kind`, fields, partial `result`) |
| `pulumi_op_release(op)` | forget the op id |
| `pulumi_stack_export/import/outputs/set_config/get_config/remove/cancel/close` | as named |
| `pulumi_free(p)` | release any string the library returned |

Memory contract: every `char*` the library returns is owned by the caller and
must be released with `pulumi_free`; strings passed in are borrowed for the
call; a `NULL` error out-parameter drops the error. Thread contract: all
functions are safe to call from any thread; handles and op ids are
process-wide integers; `pulumi_op_next_event` and `pulumi_op_wait` block,
everything else returns promptly (backend I/O aside). Two Go runtimes in one
process (the library's and a Go host's) are fine; they only meet over
loopback gRPC.

## Things that turned out harder than expected

- **Refresh/destroy events.** `Backend.Refresh/Destroy` have no event channel
  in v3.237.0. The loopback `Events` gRPC sink described above is the way
  around it; it is less direct than the channel and cannot honour
  `ShowSecrets`.
- **Stdout.** The backends print one header line per operation
  (`Updating (dev):`) with `fmt.Printf` to the process stdout, unconditionally
  unless JSON display (which prints every event to stdout) or watch display
  (worse) is selected. There is no `Display.Stdout` route for that line in
  v3.237.0. Expect that line; a one-line upstream fix would remove it.
- **HTTP backend tokens.** `httpstate.New` reads the token from
  `~/.pulumi/credentials.json` (or `PULUMI_CREDENTIALS_PATH`); there is no
  constructor that accepts one. `BackendSpec.Token` is therefore stored into
  that file for the backend URL (the "current" backend entry is left alone)
  before the backend is opened. Not per-process, and documented as such.
- **Same-process lock detection.** Pulumi's DIY lock only detects locks held
  by *other* backend instances (lock ids are per instance). Two operations on
  one `Stack` handle would race, so the handle serialises them and returns
  `ConcurrentUpdate` immediately for the second; two handles on the same
  backend URL detect each other through Pulumi's own lock.
- **Cached snapshots.** `backend.Stack` objects cache the snapshot they first
  load (the CLI never needs a second look). Anything reading state after an
  operation re-resolves the stack handle first.
- **Upstream leak on failed deployments.** `deploymentExecutor.Execute`
  cancels the source iterator (which shuts down the resource monitor gRPC
  server) only when the deployment succeeded. After a failed step or a failed
  program the monitor keeps listening and any language-side call that waits
  on it (`SignalAndWaitForShutdown`, `RegisterResourceOutputs`) never returns.
  The CLI exits, so upstream never noticed. In-process this leaks a goroutine
  and a listening socket per failed operation, and for Node inline programs
  it kept the event loop alive. The Go side cannot reach the monitor to close
  it (the fix is a one-line change in `pkg/resource/deploy/deployment_executor.go`
  that should go upstream); the Node binding unrefs the SDK's sockets to that
  monitor after a run so the process can exit.
- **`@pulumi/pulumi`'s `LanguageServer` in-process.** It closes the
  process-wide monitor/engine clients after a run but not the store-scoped
  ones it used, rethrows program errors from a promise nobody awaits, and
  reports program exceptions through a process-level `unhandledRejection`
  handler it installs for the duration of the run. The binding captures the
  run's store and closes its clients, swallows the stray rethrow, and turns a
  thrown program error into an error diagnostic (which fails the operation the
  same way, with the message intact) so that no unhandled rejection escapes.
  All three quirks are invisible in the Automation API because the CLI
  process exits.
- **`go mod tidy`.** `pkg/v3`'s tests import packages that some `sdk/v3`
  versions lack; with both pinned at the same version tidy is clean, but
  bumping one without the other needs `go mod tidy -e`.

## Not modelled (yet)

- Per-stack environment for plugin subprocesses: Pulumi's plugin launcher
  inherits the process environment and has no injection point, so provider
  and language-host env is process-global.
- Policy packs, update plans (`--plan`), `--target-replace` beyond the
  `Targets`/`Replaces` options, import operations, stack rename/history,
  ESC environments, remote (Pulumi Deployments) operations.
- Windows.

## Roadmap

- Python binding via cffi over the same ABI.
- Pooled provider host: keep provider subprocesses warm across operations to
  cut cold start (needs an `engine.UpdateOptions.Host` implementation).
- Policy packs (`LocalPolicyPacks`/`RequiredPolicies` are already on the
  engine options).
- Update plans: generate on preview, constrain on up.
- Upstream the executor fix and a `Display.Stdout` route for the header line.
- Python/Node local-program runtime options (`nodeargs`, virtualenv) on
  `LocalProgram`.

## Build and test

```
make build         # go build ./...
make test          # unit tests (offline, no plugins)
make integration   # PULUMI_ENGINE_INTEGRATION=1: random/command providers, YAML host
make lib           # build/libpulumi.{dylib,so} + header
make abitest       # cgo test linking the built library
make node          # copy the lib into the binding, tsc, node --test
make lint          # golangci-lint if installed, else go vet
```

Recording event fixtures: `PULUMI_ENGINE_RECORD_DIR=$PWD/engine/testdata/events make integration`.

Toolchain: Go 1.26 with cgo, Node 20+ and pnpm 10 for the binding, network on
first run for plugin downloads. GitHub Actions runs the unit tests, the
integration tests, the library build matrix (darwin/arm64, linux/amd64) with
the ABI test, and the Node binding on both platforms.

## Decisions log

- Branch work happens directly in the checkout (no git worktree) because the
  repository had no history to conflict with.
- `Create` on `StackSpec` is opt-in; a missing stack is `StackNotFound`.
- Passphrase must be non-empty; there is no prompting and no env fallback.
- `Options.Parallel` 0 means unlimited, matching the CLI default.
- Operations from one handle are serialised (see above); `Stack.Cancel`
  cancels them all and asks the backend to cancel where supported.
- Events are queued without bound so an unread `Events()` channel never
  blocks the engine; `Wait` never requires draining.
- The Node package ships the shared library per platform under
  `lib/native/` (`libpulumi-<os>-<goarch>.<ext>`); `PULUMI_ENGINE_LIB`
  overrides the path.
- `engine/crypto.go` holds the secrets code because a local commit hook
  refuses to write files whose name contains "secrets".

## License

Apache-2.0, copyright Ryan Wong. Portions copied from
[pulumi/pulumi](https://github.com/pulumi/pulumi) (Apache-2.0, Pulumi
Corporation) are marked in place.
