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
- Events carry the library's own `sequence` (from 0, per operation) and
  `timestamp` (epoch seconds), and every stream ends with a
  `{ type: "cancel", cancelEvent: {} }` terminator after the summary — also
  when the operation fails before the engine starts. The DIY backend's
  `Updating (dev):` banner arrives as a `stdoutEvent`, not on this process's
  stdout.
- `StackSpec.env` (and `Options.env` for one operation) is the environment
  every provider plugin and language host the stack launches is started with.
  It never touches this process's environment, so `PULUMI_HOME`, `NODE_PATH`,
  `PATH` and program-facing variables can be set per stack:

  ```ts
  const stack = await openStack({ /* … */, env: { NODE_PATH: "/app/node_modules" } });
  await stack.up({ mode: "local", dir, languageVersion: "1.38.5" }, { env: { LOG: "debug" } });
  ```
- `stack.outputs(showSecrets?)`, `export()`, `import()`, `setConfig()`,
  `getConfig()`, `getTags()`, `setTags()`, `history(opts?)`, `cancel()`,
  `remove()`, `close()`; module-level `listStacks(backend, filter?)`.
- `stack.history({ limit?, page?, showSecrets? })` returns `UpdateInfo[]`
  newest first (`kind`, `result`, `message`, `startTime`/`endTime` as epoch
  seconds, `version`, `environment`, `config`, `resourceChanges`); previews
  are not recorded and the DIY backend reports version 0.
- `listStacks({ url, token? }, { project?, organization?, tagName?, tagValue? })`
  returns `StackSummary[]` (`name`, `fullName`, `project?`, `lastUpdate?`,
  `resourceCount?`).
- Update plans, with the CLI's semantics: `preview(program, { savePlan:
  "/path" })` writes the plan file `pulumi preview --save-plan` would
  (`generatePlan: true` returns it as `result.plan` instead, or as well);
  `up(program, { plan: "/path" })` or `{ planJson: plan }` constrains the
  update to it and `result()` rejects with `PlanViolationError` (`resources:
  [{ urn, message }]`) when the program exceeds the plan; nothing beyond the
  plan is applied. Plans written by the library are honoured by `pulumi up
  --plan` and vice versa.
- The event loop is never blocked: the blocking ABI calls run on koffi's
  async thread pool.

Library lookup (`libraryPath()` tells which): `$PULUMI_ENGINE_LIB`; the
per-platform package `@ryanjwong/pulumi-engine-node-<os>-<arch>` (an
optional dependency of the published package, see
[docs/releasing.md](../../docs/releasing.md)); `lib/native/libpulumi-<os>-<arch>.<ext>`
in the source tree; `../../build/`. The published package is named
`@ryanjwong/pulumi-engine-node` (GitHub Packages requires the owner's
scope); install it under this name with
`npm install @pulumi-engine/node@npm:@ryanjwong/pulumi-engine-node`.

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
  the backend from `Pulumi.yaml` when the env names none. The whole `envVars`
  map (plus `pulumiHome` as `PULUMI_HOME`) is handed to the library as
  `StackSpec.env`, so it reaches plugins and language hosts as it does with
  the CLI. An empty `PULUMI_CONFIG_PASSPHRASE` is a passphrase, as on the CLI.
- `Stack.up/preview/refresh/destroy/previewDestroy` with `onEvent`,
  `onOutput`, `onError`, `signal`, `parallel`, `message`, `target`,
  `targetDependents`, `exclude`, `replace`, `refresh`, `continueOnError`,
  `expectNoChanges`, `runProgram`, `program`; results shaped as
  `UpResult`/`PreviewResult`/`RefreshResult`/`DestroyResult` (`stdout`,
  `stderr`, `summary` as an `UpdateSummary`, `changeSummary`, `outputs` as
  `{value, secret}`).
- `EngineEvent` objects with the SDK's field names (`diagnosticEvent`,
  `resourcePreEvent`, `resOutputsEvent`, `resOpFailedEvent`, `summaryEvent`,
  `preludeEvent`, …), with the library's `sequence` (from 0) and epoch-second
  `timestamp`, ending with the `cancelEvent` marker the CLI's stream ends with.
- `cancel`, `outputs`, `exportStack`, `importStack`,
  `getConfig/setConfig/getAllConfig/setAllConfig/removeConfig/refreshConfig`;
  `workspace.stack/createStack/selectStack/removeStack/listStacks/exportStack/importStack/stackOutputs/projectSettings/stackSettings`.
- `info`/`history(pageSize?, page?, showSecrets?)` read the backend's update
  history as `UpdateSummary` objects (`kind`, `result`, `message`,
  `startTime`/`endTime` as `Date`, `config`, `environment`, `version`,
  `resourceChanges`), so updates another process made are visible too.
  Previews are never recorded and the DIY backend reports version 0.
- `listTags/getTag/setTag/removeTag`, over the backend's stack tags.
- `workspace.listStacks()` on every backend (not just `file://`), with the
  elided names `pulumi stack ls` prints and `current` for the selected stack.
- `PulumiCommand.get()` returns `{ command, version }` where `version` is the
  embedded Pulumi version (the SDK's minimum-version check passes) and
  `command` names the shared library; `run` rejects.
- Failures are `CommandError` subclasses built from the SDK's stderr
  conventions (`no stack named … found`, `stack … already exists`,
  `[409] Conflict`, `update canceled`), with the library's typed
  `PulumiError` on `cause`.

Gaps — what differs from the SDK over a `pulumi` binary:

- `preview({ plan })` saves a plan and `up({ plan })` is constrained by it
  as with the CLI (a violation is a `CommandError` whose `cause` is
  `PlanViolationError`); **remote workspaces**, policy packs, `importFile`,
  `attachDebugger`, `color`, log options are ignored.
- `onOutput` receives the facade's own rendering (headers, per-step lines,
  diagnostics, a resource summary), not the CLI's progress display.
- `createStack` probes then creates (two backend opens); engine handles
  stay open until `workspace.removeStack`, `stack.close()` or
  `workspace.close()`.
- Inline programs resolve `@pulumi/pulumi` from this package's location
  (see "inline" above), not from the consumer's.
