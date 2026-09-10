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
| Python binding | `pulumi_engine`, cffi (ABI mode, no compile step), iterator events, inline programs | `bindings/python/` |

## What it is not

- It is not the `pulumi` CLI. Nothing shells out to `pulumi`; the CLI is not
  required at build or run time. The repository uses it only to compare
  behaviour: [docs/cli-parity.md](docs/cli-parity.md) records how the two
  agree on state, secrets, config, events, locks, cancellation and plugins.
- It is not a daemon. There is no long-running service; every operation runs
  in the caller's goroutines/threads and finishes when `Wait` returns.
- It does not replace provider plugins. `pulumi-resource-*` and
  `pulumi-language-*` binaries are still child processes, started and stopped
  per operation by Pulumi's own plugin host, and downloaded to `~/.pulumi/plugins`
  on first use by Pulumi's own installer.

## Program modes

| mode | Go | C ABI / Node / Python | how it runs |
|-|-|-|-|
| in-process | `GoProgram(func(*pulumi.Context) error)` | (Node: `async () => {...}`, Python: `def program(): ...`, see inline below) | A LanguageRuntime gRPC service is served from a goroutine in this process; the engine connects to it as the CLI would with `--client`. The Go SDK runs the function with `pulumi.RunWithContext` against the engine's resource monitor. No subprocess. |
| callback | `CallbackProgram{Address}` | `{"mode":"callback","address":...}` | A LanguageRuntime gRPC server the caller runs. This is the mechanism behind `pulumi up --client` and behind inline Automation API programs in every SDK. |
| local | `LocalProgram{Dir}` | `{"mode":"local","dir":...}` | The project in `Dir` (`Pulumi.yaml`) is executed by the stock `pulumi-language-<runtime>` plugin, installed on demand. The YAML host needs no toolchain; Node/Python/Go hosts need theirs. |

Refresh and destroy do not need a program (`nil` in Go, omitted over the ABI).

The Node binding's **inline** mode (`stack.up(async () => { new random.RandomPet(...) })`)
starts `@pulumi/pulumi`'s own `LanguageServer` (`automation/server`, the class
the Automation API uses for inline programs) on a loopback port and passes it
as a callback program. The program runs in the Node process; the tests check
`process.pid` to prove it. The Python binding does the same with
`pulumi.automation._server.LanguageServer` (the class the Python Automation
API uses for inline programs) on a `grpc` server; see
`bindings/python/README.md` for the one workaround it needs.

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

## Quick start: Python

```python
import pulumi_engine as pe

stack = pe.open_stack({
    "name": "dev",
    "project": {"name": "demo"},
    "backend": {"url": "file:///var/lib/demo/state"},
    "secrets": {"provider": "passphrase", "passphrase": os.environ["DEMO_PASSPHRASE"]},
    "config": {"apiKey": pe.Secret("...")},
    "create": True,
})

def program():                       # inline: runs in this process (needs pulumi + grpcio)
    import pulumi, pulumi_random as random
    pet = random.RandomPet("pet")
    pulumi.export("name", pet.id)

with stack.up(program, {"message": "first deploy"}) as op:   # leaving the block cancels an unfinished op
    for event in op:                                          # iterator of event dicts, secrets redacted
        print(event["type"])
    try:
        result = op.result()                                  # typed exceptions
        print(result["changes"], result["outputs"]["values"]["name"])
    except pe.ResourceOpFailedError as e:
        print(e.urn, e.op, e)
```

Programs: `{"mode": "local", "dir": ...}` (`pe.LocalProgram`), `{"mode":
"callback", "address": ...}` (`pe.CallbackProgram`), or a callable (inline).
The blocking ABI calls release the GIL (cffi does that for every C call), so
other threads run while `result()` or the event iterator waits. See
`bindings/python/README.md`.

## Backends, secrets, credentials

The backend is chosen by `Backend.URL`: `file://`, `s3://`, `gs://`,
`azblob://` select Pulumi's DIY backend (through the same package the CLI
uses, so object-store credentials come from the cloud SDK environment as they
do for the CLI); `https://` selects the HTTP backend (Pulumi Cloud or a
self-hosted implementation).

Secrets providers per stack: `passphrase` (default for DIY; the passphrase is
a spec field, never `PULUMI_CONFIG_PASSPHRASE`; the empty passphrase is valid
when confirmed with `PassphraseSet`, or by the presence of the `passphrase`
key in JSON, as `PULUMI_CONFIG_PASSPHRASE=""` is for the CLI), `service` (default for HTTP),
a KMS URL (`awskms://`, `gcpkms://`, `azurekeyvault://`, `hashivault://`), or
`b64` for tests. Existing key material (the passphrase salt, the KMS data key)
is picked up from `Pulumi.<stack>.yaml` in `Project.Dir` or from the
checkpoint, so a stack keeps one encryption context across processes. The
library never prompts.

Configuration comes from `Pulumi.<stack>.yaml` (if `Project.Dir` is set)
overlaid with `StackSpec.Config`; `SetConfig` writes back to that file, like
`pulumi config set`. Project-level config schema and defaults from
`Pulumi.yaml` are applied and validated for local programs.

**HTTP backend credentials** come from `Backend.Token` only: never from
`PULUMI_ACCESS_TOKEN`, never from a `pulumi login`. Each open backend keeps
its own token, so operations with different tokens (even for one URL) run
concurrently without interfering. The one thing Pulumi leaves process-global
is the hand-off (`httpstate.New` has no constructor that takes a token; see
[docs/upstream.md](docs/upstream.md) #3): under a lock the token is written
to Pulumi's credentials file for the URL, the backend is constructed, and the
file is restored byte for byte, or deleted if it did not exist.

**Plugin environment.** `StackSpec.Env` (plus `Options.Env` per operation)
is the environment every provider plugin and language host launched by the
operation gets, overlaid on the process environment. Per-run credentials,
`PULUMI_HOME` for the plugins' own use, `NODE_PATH`, program variables: all
go here, and never into this process, so concurrent operations with
different environments do not interfere. The library's own plugin cache and
downloads still use the process's `PULUMI_HOME`/`GITHUB_TOKEN` (upstream.md
#9).

**Tags, listing, history.** `Stack.GetTags/SetTags` (both backends; the DIY
backend keeps a `<stack>.pulumi-tags` file beside the checkpoint since
Pulumi 3.2xx, with one limit documented on `SetTags`), `engine.ListStacks`
(project/organization/tag filters; `Organization` is `Unsupported` on DIY),
`Stack.History` (newest first, paged; the DIY backend reads its
`.pulumi/history` files and has no version numbers). Anything a backend
cannot do is a typed `Unsupported` error, never a panic.

## Events and errors

`Event` embeds Pulumi's wire type `apitype.EngineEvent` (the `--json` /
event-log format) and adds `Type` (`prelude`, `summary`, `resourcePre`,
`resourceOutputs`, `resourceOpFailed`, `diagnostic`, `stdout`, `policy*`,
`progress`, ...). Property values are secret-redacted (`[secret]`) unless
`Options.ShowSecrets` is set. Every event carries `sequence` (from 0 per
operation) and `timestamp` (Unix seconds), and every stream ends with the
engine's `cancel` event after the summary, exactly as `pulumi --event-log`
and the Automation API deliver it; it is the terminator, not a sign that
anything was cancelled. The backend's `Updating (dev):` banner arrives as a
`stdout` event (see "Stdout" below).

Typed errors (`errors.As` / `engine.KindOf` / `error.kind` over the ABI):
`InvalidSpec{Field}`, `ResourceOpFailed{URN,Type,Op,Provider,Message}`
(all failures are in `Result.Failures`), `ProgramFailed{Message}`,
`ConcurrentUpdate`, `StackNotFound`, `StackExists`, `PendingOperations{URNs}`,
`Cancelled{Operation}`, `PlanViolation{Resources}`, `Unsupported{Feature,Backend}`,
`Unclassified{Err}`.

## Update plans

`Preview` with `Options.SavePlan` (a path) writes the plan file
`pulumi preview --save-plan` writes; `Options.GeneratePlan` returns it in
`Result.Plan` instead (or as well). `Up` with `Options.Plan` (a path) or
`Options.PlanJSON` (the document) is constrained to that plan the way
`pulumi up --plan` is: the engine refuses any operation the plan did not
propose (a replace where an update was planned, an update where a same was
planned, a resource the plan does not know) and the operation fails with
`PlanViolation{Resources: [{URN, Message}]}` before that operation is
applied. The plan is `apitype.DeploymentPlanV1`, serialised with the same
encoder the CLI uses, with secret values encrypted by the stack's secrets
provider unless `ShowSecrets` was set for the preview, so a plan written by
the library is honoured by the CLI for the same stack and vice versa
(`TestIntegrationParityPlans`; [docs/cli-parity.md](docs/cli-parity.md) row
10). One difference: the plan manifest's `version` (the CLI binary's
version string) is empty in a library-written plan; neither side checks it.
Over the ABI the options are `savePlan`, `generatePlan`, `plan`, `planJson`
and the result field `plan`; the Node binding types them on `Options` and
`Result` (`PlanViolationError`), the automation-compat facade maps the
SDK's `preview({ plan })` / `up({ plan })`, and Python mirrors Node.

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
JSON translation, which does the redaction). Engine-internal step events
(default provider steps, the refresh steps of an `up --refresh`) are passed
through: that is what `pulumi up --event-log` and therefore the Automation
API deliver; only the CLI's terminal display and `--json` output drop them
(see [docs/cli-parity.md](docs/cli-parity.md)). Pulumi's `Backend.Refresh` and
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

Each operation runs with its own `plugin.Host` (`engine/pluginhost.go`),
passed through `engine.UpdateOptions.Host`: Pulumi's default host on a
`plugin.Context` of the operation's own, with provider launches given
`StackSpec.Env` through the `env.Env` parameter `Host.Provider` already has,
and language hosts launched by the library (Pulumi launches those with a nil
env). The host's diag sinks feed the operation's event stream, so plugin
output and `pulumi.log` calls arrive as diagnostics as they do from the CLI.

Update plans use Pulumi's own machinery unchanged: `engine.UpdateOptions.GeneratePlan`
makes the preview's step generator record a `deploy.Plan`, which
`backend.PreviewStack` returns; `stack.SerializePlan/DeserializePlan` are
the CLI's plan file codec; `engine.UpdateOptions.Plan` on an up makes the
step generator check every step and goal against it. Because the library
runs `UpdateStack` with `SkipPreview`, the plan is checked once, during the
real update, which is also where the CLI's `--plan` enforcement happens
(its preview phase only clones the plan).

Copied from pulumi/pulumi (Apache-2.0): everything lives under
[`internal/upstream/`](internal/upstream), one file per copied unit with a
header naming the upstream file, version and sha256, and begin/end markers
around the copied region. `make upstream-check` fails with a diff when any of
those upstream files changed at the pinned version. Nothing else reaches into
behaviour a public Pulumi API could provide; the hooks we wish existed are in
[docs/upstream.md](docs/upstream.md).

## Compatibility surface and bump policy

| what | version |
|-|-|
| `github.com/pulumi/pulumi/pkg/v3`, `sdk/v3` | **v3.237.0** (go.mod) |
| `pulumi` CLI versions the parity suite was verified against | 3.218.0, 3.237.0 (CI, `PULUMI_CLI_VERSION`), 3.250.0 |
| `@pulumi/pulumi` Node SDK the binding is tested with | 3.261.0 (`bindings/nodejs/pnpm-lock.yaml`; peer range `>=3.150.0`) |
| `pulumi` Python SDK the binding is tested with | 3.262.0 (`pulumi_engine[inline]` requires `>=3.150.0`, `grpcio>=1.60`; Python 3.12 in CI, 3.14 locally) |
| language hosts / providers in tests | `pulumi-language-yaml` 1.38.5 (pinned when installed by the tests), `pulumi-random` 4.16.8, `pulumi-command` latest |
| event schema | `apitype.EngineEvent` of the pinned sdk (the `--event-log` / Automation API JSON); deployment schema v3 (`apitype.DeploymentSchemaVersionCurrent`); service API `application/vnd.pulumi+9` |

**These packages are not a stable API.** `pkg/v3` is the CLI's
implementation, and Pulumi changes signatures (`backend.UpdateOperation`,
`engine.UpdateOptions`, display options, secrets constructors) between minor
releases. Every bump is a compatibility event, made cheap by three things:

- **One fork boundary.** All copied Pulumi code is under `internal/upstream/`
  with a header per file (upstream path, pinned version, sha256, why it is
  copied, what would let us delete it).
- **Drift check.** `make upstream-check` (also run by `make test` and CI)
  recomputes the sha256 of every upstream file a copy came from at the
  version pinned in go.mod and fails with a unified diff between the recorded
  and the pinned version when it changed; after review and porting,
  `make upstream-update` accepts the new version.
- **Bump workflow.** `make bump PULUMI=vX.Y.Z` (`scripts/bump-pulumi.sh`)
  bumps pkg+sdk together, tidies, builds, runs the unit tests and the drift
  check, keeps CI's parity CLI version and this table in step, and prints the
  integration/parity/Node commands to run next. A weekly GitHub Actions job
  (`upstream-weekly.yml`) does the same against the latest `pkg/v3` release on
  a throwaway branch and opens or updates an issue with the result; nothing is
  merged automatically.

Provider plugins and language hosts are versioned independently and are not
affected by the pin. The test-only `pulumi-random` Go SDK is pinned at
v4.16.8 because newer releases require a newer `sdk/v3`.

## C ABI (`libpulumi`)

Built by `make lib` into `build/libpulumi.{dylib,so}` with the generated
`build/libpulumi.h` (the contract is also in the header's preamble).
CI builds darwin/arm64 and linux/amd64 and runs the ABI test against the
built library (`make abitest`); releases build all four platforms (see
"Install" below).

| function | purpose |
|-|-|
| `pulumi_version()` | version string |
| `pulumi_stack_open(spec_json, &err)` | handle > 0, or 0 with `err` |
| `pulumi_op_start(handle, request_json, &err)` | op id > 0; request `{"kind","program","options"}`; options include the plan fields `savePlan`, `generatePlan` (preview) and `plan`, `planJson` (up) |
| `pulumi_op_next_event(op, timeout_ms)` | event JSON, `""` on timeout, `NULL` at end |
| `pulumi_op_cancel(op)` | graceful cancel; second call terminates |
| `pulumi_op_wait(op, &err)` | result JSON (with `plan` for a plan-generating preview), or `NULL` with error JSON (`kind`, fields, partial `result`; `resources` for `planViolation`) |
| `pulumi_op_release(op)` | forget the op id |
| `pulumi_stack_export/import/outputs/set_config/get_config/remove/cancel/close` | as named |
| `pulumi_stack_get_tags/set_tags(handle, tags_json)` | tags as a JSON object; set replaces all |
| `pulumi_stack_history(handle, options_json)` | `[{kind, result, message, startTime, endTime, version, environment, config, resourceChanges}]`, newest first; options `{limit, page, showSecrets}` |
| `pulumi_list_stacks(request_json)` | `{"backend": {...}, "filter": {project, organization, tagName, tagValue}}` -> `[{name, fullName, project, lastUpdate, resourceCount}]` |
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
  (worse) is selected; there is no `Display.Stdout` route in v3.237.0. The
  first operation therefore replaces the Go runtime's `os.Stdout` with a pipe
  (`engine/stdout.go`): banner lines of running operations become their
  `stdout` events, everything else is forwarded to the original stdout. For
  the shared library that is all of the library's output and the host
  process's file descriptor 1 is untouched; a Go program embedding the
  package keeps its own prints (through the forwarding hop) and can opt out
  with `engine.SetStdoutCapture(false)` before the first operation.
- **HTTP backend tokens.** `httpstate.New` reads the token from Pulumi's
  credentials file and nothing else; the backend type is unexported. See
  "Backends, secrets, credentials" for the per-spec hand-off.
- **Plugin environment.** `Host.Provider` takes an env, `NewLanguageRuntime`
  does not, and a caller-supplied host cannot sit on the engine's plugin
  context (whose diag sink is unexported). Hence the per-operation host on
  its own context with a copied event sink, and a copied plugin launcher for
  language hosts (`internal/upstream/{eventsink,launch}.go`).
- **Passphrase cache.** `passphrase.GetPassphraseSecretsManager` caches
  managers process-wide by salt and ignores the passphrase on a hit; the
  library verifies the passphrase first (`internal/upstream/passphrase.go`),
  otherwise a wrong passphrase would be accepted after a right one in the
  same process.
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

- Per-stack `PULUMI_HOME` for the library's own plugin resolution and
  downloads (and `GITHUB_TOKEN` for them): `workspace.GetPluginPath` reads
  the process environment; the spec's `PULUMI_HOME` reaches plugin
  subprocesses only ([docs/upstream.md](docs/upstream.md) #9).
- Policy packs, `--target-replace` beyond the `Targets`/`Replaces` options,
  import operations, stack rename, ESC environments, remote (Pulumi
  Deployments) operations, the CLI's `--strict` (generate a plan during an
  up's own preview and constrain the update to it; the library's up has no
  preview phase, so generate with `Preview` and pass the plan to `Up`).
- Windows.

## Install

Releases are git tags `vX.Y.Z` built by `release.yml`
([docs/releasing.md](docs/releasing.md)): `libpulumi` for darwin-arm64,
darwin-amd64, linux-amd64 and linux-arm64, each built and load-tested on a
native runner, attached to the GitHub release, and the Node packages on
GitHub Packages.

| language | install |
|-|-|
| Go | `go get github.com/ryanjwong/pulumi-engine/engine@vX.Y.Z` (cgo not needed; the engine is plain Go) |
| C / anything with `dlopen` | download `libpulumi-<os>-<arch>.tar.gz` from the release (`libpulumi.{dylib,so}`, `libpulumi.h`, `SHA256SUMS`), check it against the release's `SHA256SUMS` |
| Node | `npm install @pulumi-engine/node@npm:@ryanjwong/pulumi-engine-node@X.Y.Z` with `@ryanjwong:registry=https://npm.pkg.github.com` (and a token with `read:packages`) in `.npmrc`. The main package pulls the one `@ryanjwong/pulumi-engine-node-<os>-<arch>` optional dependency that matches the host; `libraryPath()` shows which library loaded. |
| Python | `pip install "pulumi_engine[inline] @ git+https://github.com/ryanjwong/pulumi-engine@vX.Y.Z#subdirectory=bindings/python"` plus the release tarball's library: set `PULUMI_ENGINE_LIB=/path/to/libpulumi.<ext>` or copy it to `pulumi_engine/native/libpulumi-<os>-<arch>.<ext>`. The wheel is pure Python (cffi in ABI mode). |

`make release-dry-run VERSION=X.Y.Z` builds all of it for the host platform
into `dist/` without publishing.

## Roadmap

- Pooled provider host: keep provider subprocesses warm across operations to
  cut cold start (needs an `engine.UpdateOptions.Host` implementation).
- Policy packs (`LocalPolicyPacks`/`RequiredPolicies` are already on the
  engine options).
- Publish the Python binding as platform wheels carrying the library, the
  way the Node platform packages do.
- Upstream the asks in [docs/upstream.md](docs/upstream.md) (language-host
  env, a host factory on the engine's context, `httpstate.NewWithAccount`, a
  `Display.Stdout` route for the banner, the executor fix) and delete the
  copies they replace.
- Per-spec `PULUMI_HOME` for plugin resolution (blocked on upstream.md #9).
- Python/Node local-program runtime options (`nodeargs`, virtualenv) on
  `LocalProgram`.

## Build and test

```
make build         # go build ./...
make test          # unit tests (offline, no plugins)
make integration   # PULUMI_ENGINE_INTEGRATION=1: random/command providers, YAML host
make parity        # the CLI parity subset of the above (needs the pulumi CLI; docs/cli-parity.md)
make lib           # build/libpulumi.{dylib,so} + header
make abitest       # cgo test linking the built library
make node          # copy the lib into the binding, tsc, node --test
make python        # venv under bindings/python/.venv, pip install -e, pytest against build/libpulumi.*
make release-dry-run VERSION=X.Y.Z   # dist/: lib tarball + abitest, npm packages, Python wheel (docs/releasing.md)
make lint          # golangci-lint if installed, else go vet
make upstream-check   # drift check of internal/upstream against the pinned Pulumi modules
make bump PULUMI=vX.Y.Z   # bump the Pulumi pin (scripts/bump-pulumi.sh)
```

Recording event fixtures: `PULUMI_ENGINE_RECORD_DIR=$PWD/engine/testdata/events make integration`.

Toolchain: Go 1.26 with cgo, Node 20+ and pnpm 10 for the Node binding,
Python 3.10+ for the Python binding, network on first run for plugin
downloads. GitHub Actions runs the unit tests, the integration tests
(including the CLI parity tests against a pinned `pulumi` installed with
`pulumi/actions`), the library build matrix (darwin/arm64, linux/amd64)
with the ABI test, the Node and Python bindings on both platforms, and the
release dry run (which also installs the packed Node package into a scratch
project).

## Decisions log

- Branch work happens directly in the checkout (no git worktree) because the
  repository had no history to conflict with.
- `Create` on `StackSpec` is opt-in; a missing stack is `StackNotFound`.
- The passphrase is a spec field; there is no prompting and no env fallback.
  The empty passphrase is accepted when confirmed (`PassphraseSet`, or the
  JSON key being present), as the CLI accepts `PULUMI_CONFIG_PASSPHRASE=""`.
- `StackSpec.Env` is for subprocesses only; the library never calls
  `os.Setenv`.
- `Options.Parallel` 0 means the CLI's default, 4 x GOMAXPROCS (phase one
  used "unlimited", which the engine passes to language hosts as MaxInt32
  and the Python SDK's language server overflows on).
- Operations from one handle are serialised (see above); `Stack.Cancel`
  cancels them all and asks the backend to cancel where supported.
- Events are queued without bound so an unread `Events()` channel never
  blocks the engine; `Wait` never requires draining.
- The Node package is split esbuild-style: the published main package holds
  the JavaScript and one `optionalDependencies` entry per platform package
  that holds only the shared library; in the source tree `make node` puts
  the host's library under `lib/native/`. `PULUMI_ENGINE_LIB` overrides the
  path everywhere (Node and Python). Published names carry the owner's
  scope (`@ryanjwong/...`) because GitHub Packages requires it; the source
  package keeps `@pulumi-engine/node` and consumers alias.
- Releases are tags only; the workflow never creates one, and publishing
  steps skip rather than fail when the token cannot publish.
- `engine/crypto.go` holds the secrets code because a local commit hook
  refuses to write files whose name contains "secrets".

## License

Apache-2.0, copyright Ryan Wong. Portions copied from
[pulumi/pulumi](https://github.com/pulumi/pulumi) (Apache-2.0, Pulumi
Corporation) are marked in place.
