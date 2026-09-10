# Running exa-deploy stages on the in-process engine

How the deploy worker's native path would switch from the Pulumi Automation
API (a `pulumi` child process per operation) to this library, what stays as
it is, what the library must add first, and what a spike measured. The spike
lives in the exa monorepo as `[exa-deploy]: spike — run stages on the
in-process pulumi-engine library` (branch `ryan/25c7d0-exa-deploy-libpulumi-spike`);
the facade it runs on is `@pulumi-engine/node/automation-compat` (PR #3).

## The path today

```
worker activity ── native-engine.ts ── engine.ts (initDeployEngine, runDeployStage)
                                            │
                                            ▼
                     automation.ts / automation-workspace.ts / automation-operations.ts
                                            │  importAutomationRuntime()
                                            ▼
                                @pulumi/pulumi/automation (LocalWorkspace, Stack)
                                            │  spawn
                                            ▼
                    pulumi CLI ── pulumi-language-nodejs ── node (tsx) deploy.ts
                               └─ pulumi-resource-* (providers, exa-deploy shim)
```

`runDeployStage` never touches the SDK directly: every stack operation goes
through `importAutomationRuntime()`, which resolves `@pulumi/pulumi/automation`
unless a loader was installed with `installAutomationRuntimeLoader`. That seam
(added for the replay fake) is the whole integration point.

## The switch

1. **`native-engine.ts` installs the library runtime once per process**, next
   to `deployEngine()`:

   ```ts
   import { libpulumiAutomationRuntime } from "exa-deploy-cli/src/cli/testing/libpulumi-runtime.ts";
   const runtime = await libpulumiAutomationRuntime(); // createAutomationModule({ sdk })
   runtime.install();                                   // installAutomationRuntimeLoader
   ```

   In production the module would be built the same way but not from
   `testing/`: `createAutomationModule({ sdk })` from
   `@pulumi-engine/node/automation-compat`, given the worker's own
   `@pulumi/pulumi/automation` so the CLI's `instanceof StackNotFoundError` /
   `ConcurrentUpdateError` / `CommandError` checks keep binding to one class
   set.

2. **`engine.ts` and everything below it stay unchanged.** The spike ran
   `runDeployStage` for `preview` and `up` end to end on the library —
   registry probe, temp workspace, dependency-output probe of the infra
   stack, lifecycle, stage-result recording, event log, outputs snapshot —
   with the facade answering `LocalWorkspace.createOrSelectStack/selectStack`,
   `Stack.preview/up`, `outputs`, `exportStack`, `importStack`, `cancel`.

3. **Per-run state moves from env vars to the operation.** Today a run's
   `PULUMI_HOME`, `PULUMI_ACCESS_TOKEN`, `EXA_DEPLOY_RUN_ID` travel in the
   request's `env` overlay and become the child's environment. With one
   engine in the process there is no child to give an environment to; the
   library needs to take them per stack/operation (below).

## What stays

- **The Nix wrapper path.** Projects with their own `.#deploy` wrapper keep
  running the shipped CLI as a subprocess (`runProjectDeployWrapper`); the
  library only replaces the engine of the *native* path. The wrapper is how
  those projects inject program environment, which is exactly what the
  library cannot do yet.
- **The provider shim.** `pulumi-resource-exa-deploy` is a launcher on PATH.
  The library resolves ambient plugins through the same
  `workspace.GetPluginPath` as the CLI: the spike's event stream carries
  `warning: using pulumi-language-nodejs from $PATH at …` (verified in
  `tests/engine-libpulumi.test.ts`), so a `pulumi-resource-exa-deploy` on the
  worker's PATH is found the same way. Two differences: the "bundled plugin"
  check compares against `os.Executable()`, which is `node` here, so every
  PATH plugin is "ambient" and warns on every operation; and the shim's
  environment comes from the process snapshot (below), not from the run.
- **Dynamic providers.** The program's `pulumi.dynamic.Resource` is served
  by `pulumi-resource-pulumi-nodejs`, found beside the language host; the
  spike's fixture creates one and reads its id back through the stack
  outputs (verified).
- **exa-pulumi-cloud as the httpstate backend.** The library opens
  `https://` backends with `httpstate.New` and takes the token from the spec
  (`backend.token`, which the facade reads from `PULUMI_ACCESS_TOKEN`). The
  passphrase pin (`pinPassphraseSecretsProvider` writing `encryptionsalt` into
  `Pulumi.<stack>.yaml`) works unchanged: the engine loads that file from the
  workspace directory.
- **Program loading.** The temp workspace, `descriptor-program.ts`, the tsx
  loader in `nodeargs`, `NODE_PATH`, the repo-root package link: all as
  today, run by `pulumi-language-nodejs` as a local program.
- **Lock polling, state mirrors, stack export streaming.** These spawn `aws`
  or `pulumi` themselves (see the assumptions list) and are untouched by the
  runtime swap; the ones that spawn `pulumi` need a library-backed
  replacement before the binary can leave the image.

## What the library must add

Found by the spike, in order of blocking-ness. Status after phase two (PR
"phase 2: per-operation plugin environment, credentials, event metadata, tags,
listing; upstream compatibility tooling"):

1. **A per-operation plugin environment. Closed.** `StackSpec.Env` (and
   `Options.Env` per operation) is the environment of every provider plugin
   and language host the operation launches, overlaid on the process
   environment and never applied to the process; concurrent operations with
   different `Env` do not interfere (`TestIntegrationPluginEnvConcurrent`,
   `TestIntegrationLanguageHostEnv`, the binding's `local.test.ts`). The
   facade passes the SDK's `envVars` (and `pulumiHome`) through, so
   `EXA_DEPLOY_PROGRAM`, `PULUMI_BUILD_TAG`, `NODE_PATH`, per-run
   credentials reach the program with no preload stub. Still process-global:
   the `PULUMI_HOME` the *library* resolves and downloads plugins from
   (`workspace.GetPluginPath` reads the process environment; the spec's
   `PULUMI_HOME` reaches the plugin subprocesses). One `PULUMI_HOME` per
   worker process is what the pod has today, so this is not blocking;
   [upstream.md](upstream.md) #9 has the ask.
2. **httpstate credential injection. Closed for the worker, with a documented
   hand-off.** The token comes from `backend.token` only (`PULUMI_ACCESS_TOKEN`
   is never read), each open backend keeps its own, and concurrent opens with
   different tokens for one URL each get theirs (`TestHTTPBackendCredentialsPerSpec`,
   httptest, asserts the `Authorization` header per stack). Pulumi's backend
   has no in-memory constructor, so the token is handed over through the
   credentials file under a lock for the duration of the constructor and the
   file is restored byte for byte; the file lives at the process's
   `PULUMI_HOME` ([upstream.md](upstream.md) #3).
3. **Accept an empty passphrase. Closed.** `PULUMI_CONFIG_PASSPHRASE=""` is
   passed through as `passphrase: ""`; the presence of the JSON key is the
   confirmation (`PassphraseSet` in Go).
4. **Event sequence and timestamps. Closed.** Every event carries `sequence`
   (from 0) and `timestamp`; the stream ends with the engine's `cancel`
   event after the summary, as `pulumi --event-log` does. The facade forwards
   the library's events unchanged; the event-log contract and the replay
   recordings hold with no facade numbering.
5. **Silence the DIY backend banner. Closed.** The banner is captured from
   the Go runtime's stdout and delivered as a `stdout` event of the operation
   (the facade prints its own header and skips the duplicate); nothing from
   the library reaches the pod's stdout.
6. **Stack history, tags, listing. Closed.** `Stack.GetTags/SetTags`,
   `Stack.History`, `engine.ListStacks` over both backends (DIY tags are a
   `<stack>.pulumi-tags` file that the CLI reads; DIY history has no version
   numbers; `Organization` filters are `Unsupported` on DIY). The facade's
   `listTags/setTag/getTag/removeTag`, `history/info` and `listStacks` use
   them, so `ensureStackTags` sets `exa:journal`/`exa:team` for real.
7. **Update plans** (`preview --save-plan` / `up --plan`, used by the
   plan-bound `up`) and `PulumiCommand.run` replacements for the paths that
   still spawn `pulumi` (`stack export --show-secrets` streaming in
   `stack-export.ts`, state mirrors). **Open.**

## exa-deploy assumptions that presume a `pulumi` binary

Everything `runDeployStage` reached in the spike that would still need the
CLI (none of these blocked a file-backend preview/up):

| where | assumption | spike handling |
|-|-|-|
| `automation-runtime.ts` `automationCommand` → `mkPulumiCommand` → `PulumiCommand.get()` | probes `pulumi version` and keeps `command` to spawn later | facade's `PulumiCommand.get()` returns the embedded version and the library path; `run` rejects |
| `automation-runtime.ts` `isolatedPulumiCommand` (`inheritEnvironment: false`) | resolves a `pulumi` executable on a bootstrap PATH | not reached by `runDeployStage` |
| `stack-export.ts` `exportStackWithoutBuffer` | spawns `pulumi stack export --show-secrets` (bootstrap-from-backend and mirrors) | not reached on a file backend; would fail with ENOENT |
| `state-mirror.ts` | `automationCommand` + `pulumiCommand` for mirror imports | not reached (no mirror target for `file://`) |
| `lock-poll.ts` | spawns `aws s3api` (not pulumi) | skipped for non-S3 backends |
| `automation-workspace.ts` `workspaceEnvVars` | `PULUMI_CONFIG_PASSPHRASE: ""`, `NODE_PATH`, `EXA_DEPLOY_PROGRAM` reach the engine child | phase two: `envVars` become `StackSpec.env` for the language host and providers; the empty passphrase is accepted |
| `temp-workspace.ts` `generatePulumiYaml` | `runtime.options.nodeargs` honoured by the language host | honoured |
| `automation.ts` `ensureStackTags` | `listTags/setTag` on httpstate stacks | phase two: served by `Stack.GetTags/SetTags` on both backends |
| worker `deploy-stage.ts` | `SHIP_PULUMI_HOME` per pod via env | phase two: `envVars.PULUMI_HOME` reaches the plugins; the library's own plugin cache is the process's `PULUMI_HOME` (one per pod today) |

## Measured: one stage, Automation API versus the library

`scripts/bench-libpulumi-stage.ts` in the spike runs the same fixture
(`tests/fixtures/libpulumi-engine`: a stack with a dynamic resource and a
`command.local.Command`) through `runDeployStage` on a `file://` backend,
cold stack each iteration, on this machine (Apple Silicon, pulumi CLI
v3.218.0 from Homebrew, libpulumi embedding v3.237.0, `pulumi-language-nodejs`
and providers shared by both).

| runtime | preview (cold stack) | up | preview (no changes) |
|-|-|-|-|
| Automation API (pulumi CLI v3.218.0) | 9.61s (min 9.56s) | 10.56s (min 10.53s) | 9.84s (min 9.82s) |
| libpulumi, in-process (embeds v3.237.0) | 8.86s (min 8.67s) | 9.04s (min 9.02s) | 8.71s (min 8.53s) |

Medians of 3 iterations; each cell is one `runDeployStage` call, which also
includes the registry probe (a Node driver evaluating `deploy.ts`) and the
dependency-output probe of the infra stack. Per operation the library saves
0.75–1.5s (8–15%).

What the difference is: the CLI path pays a `pulumi` process start plus its
own language-host and plugin launches per operation (two `pulumi` runs for a
stage: the operation and the `stack output` that follows an `up`, plus
`pulumi version` once); the library path pays only the language host and
providers. Program evaluation (node + tsx + the framework) dominates both,
so the saving is a fixed slice per operation rather than a multiple.
