# @pulumi-engine/node

The Pulumi engine as an in-process library for Node.js. Loads `libpulumi`
(the C ABI of [pulumi-engine](../../README.md)) through
[koffi](https://koffi.dev), so there is no native compile step: the shared
library ships with the package per platform under `lib/native/`.

```ts
import { openStack, Secret } from "@pulumi-engine/node";

const stack = await openStack({
    name: "dev",
    project: { name: "demo" },
    backend: { url: "file:///var/lib/demo/state" },
    secrets: { provider: "passphrase", passphrase: process.env.PASSPHRASE! },
    config: { token: new Secret("...") },
    create: true,
});

// local: a project directory run by its language host
const up = await stack.up({ mode: "local", dir: "/path/to/project" });
for await (const e of up) console.log(e.type);
const result = await up.result();
await up.release();

// inline: a function in this process (needs @pulumi/pulumi + @grpc/grpc-js)
const inline = await stack.up(async () => {
    const pet = new random.RandomPet("pet", {});
    return { name: pet.id };
});
```

- `stack.preview/up/refresh/destroy(program?, options?)` return an
  `Operation`: an `AsyncIterable<Event>` with `result()` (a promise that
  rejects with a typed `PulumiError` subclass: `ResourceOpFailedError`,
  `ProgramFailedError`, `CancelledError`, `ConcurrentUpdateError`,
  `StackNotFoundError`, `StackExistsError`, `PendingOperationsError`,
  `InvalidSpecError`), `cancel()` and `release()`.
- `options.signal` (an `AbortSignal`) cancels gracefully.
- `stack.outputs(showSecrets?)`, `export()`, `import()`, `setConfig()`,
  `getConfig()`, `cancel()`, `remove()`, `close()`.
- The event loop is never blocked: the blocking ABI calls run on koffi's
  async thread pool.

Library lookup: `$PULUMI_ENGINE_LIB`, else
`lib/native/libpulumi-<platform>-<goarch>.<ext>`, else `../../build/`.

Build from the repository root: `make node` (builds the library, copies it
in, compiles TypeScript and runs the tests). Tests need network on first run
to download the `random`/`command` providers and the YAML language host.

## Automation API compatibility (`@pulumi-engine/node/automation-compat`)

For code written against `@pulumi/pulumi/automation`, the package ships a
facade with the SDK's shapes served by the in-process engine: swap the module
and the consumer runs without a `pulumi` binary.

```ts
import { createAutomationModule } from "@pulumi-engine/node/automation-compat";
import * as sdk from "@pulumi/pulumi/automation";

// With `sdk`, errors are that SDK's classes (instanceof StackNotFoundError,
// ConcurrentUpdateError, CommandError holds) and its other exports pass through.
const automation = createAutomationModule({ sdk });

const stack = await automation.LocalWorkspace.createOrSelectStack(
    { stackName: "dev", workDir: "/path/to/project" }, // Pulumi.yaml + a local program
    { envVars: { PULUMI_BACKEND_URL: "file:///var/lib/state", PULUMI_CONFIG_PASSPHRASE: "pw" } },
);
const result = await stack.up({ onEvent: (e) => log(e), onOutput: (s) => process.stdout.write(s) });
result.summary.resourceChanges; // { create: 3 }
result.outputs;                 // { url: { value: "…", secret: false } }
```

Implemented, with the SDK's names and shapes:

- `LocalWorkspace.create/createStack/selectStack/createOrSelectStack` with
  `LocalProgramArgs` (`workDir`) and `InlineProgramArgs` (`program`), and the
  options `workDir`, `envVars`, `secretsProvider`, `projectSettings`,
  `stackSettings` (config), `pulumiHome`, `pulumiCommand`, `program`.
  The engine reads `PULUMI_BACKEND_URL`, `PULUMI_ACCESS_TOKEN`,
  `PULUMI_CONFIG_PASSPHRASE[_FILE]` from `envVars` (then `process.env`), and
  the backend from `Pulumi.yaml` when the env names none.
- `Stack.up/preview/refresh/destroy/previewDestroy` with `onEvent`,
  `onOutput`, `onError`, `signal`, `parallel`, `message`, `target`,
  `targetDependents`, `exclude`, `replace`, `refresh`, `continueOnError`,
  `expectNoChanges`, `runProgram`, `program`; results shaped as
  `UpResult`/`PreviewResult`/`RefreshResult`/`DestroyResult` (`stdout`,
  `stderr`, `summary` as an `UpdateSummary`, `changeSummary`, `outputs` as
  `{value, secret}`).
- `EngineEvent` objects with the SDK's field names (`diagnosticEvent`,
  `resourcePreEvent`, `resOutputsEvent`, `resOpFailedEvent`, `summaryEvent`,
  `preludeEvent`, …), numbered from 0 with epoch-second timestamps, ending
  with the `cancelEvent` marker the CLI's stream ends with.
- `cancel`, `outputs`, `exportStack`, `importStack`, `info`, `history`,
  `getConfig/setConfig/getAllConfig/setAllConfig/removeConfig/refreshConfig`;
  `workspace.stack/createStack/selectStack/removeStack/listStacks/exportStack/importStack/stackOutputs/projectSettings/stackSettings`.
- `PulumiCommand.get()` returns `{ command, version }` where `version` is the
  embedded Pulumi version (the SDK's minimum-version check passes) and
  `command` names the shared library; `run` rejects.
- Failures are `CommandError` subclasses built from the SDK's stderr
  conventions (`no stack named … found`, `stack … already exists`,
  `[409] Conflict`, `update canceled`), with the library's typed
  `PulumiError` on `cause`.

Gaps — what differs from the SDK over a `pulumi` binary:

- **`envVars` do not reach plugins.** The Go runtime snapshots the process
  environment when the library loads (on macOS, dyld hands it the
  environment the process started with — even `process.env` writes made
  before loading are invisible), and Pulumi launches language hosts and
  providers with that snapshot. `PULUMI_HOME`, `NODE_PATH`, `PATH` and any
  program-facing variable must be set before the Node process starts until
  the library grows a per-operation plugin environment.
- **An empty passphrase is rejected** (`PULUMI_CONFIG_PASSPHRASE=""` is a
  valid CLI passphrase); the library's spec treats `""` as unset.
- **Update plans** (`plan`/`--save-plan`) reject; **stack tags** reject;
  **remote workspaces**, policy packs, `importFile`, `attachDebugger`,
  `color`, log options are ignored.
- **`history`/`info`** are kept in memory per workspace (the library has no
  history API): a stack updated by another process reports none.
- **`listStacks`** reads the state directory and only works on `file://`.
- The Pulumi DIY backend prints its `Updating (stack):` banner straight to
  the process's stdout (`backend/diy/backend.go`); `onOutput` receives the
  facade's own rendering (headers, per-step lines, diagnostics, a resource
  summary), not the CLI's progress display.
- `createStack` probes then creates (two backend opens); engine handles
  stay open until `workspace.removeStack`, `stack.close()` or
  `workspace.close()`.
- Inline programs resolve `@pulumi/pulumi` from this package's location
  (see "inline" above), not from the consumer's.
