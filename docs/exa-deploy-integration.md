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

Found by the spike, in order of blocking-ness.

1. **A per-operation plugin environment.** The Go runtime snapshots the
   process environment when the library loads — on macOS, dyld hands it the
   environment the process *started* with, so even `process.env` writes made
   before `koffi.load` are invisible (verified with a probe library) — and
   Pulumi launches language hosts and providers with `os.Environ()`
   (`sdk/go/common/resource/plugin/plugin.go`). The CLI's workspace `envVars`
   (`EXA_DEPLOY_PROGRAM`, `PULUMI_BUILD_TAG`, `NODE_PATH`,
   `PULUMI_NODEJS_TRANSPILE_ONLY`, per-run credentials) therefore never reach
   the program; `mkProject` fails on the missing `PULUMI_BUILD_TAG` before
   registering a resource. The spike bridges this with a preload stub
   (`--require` added to the manifest's `nodeargs` that loads the env from a
   file) — the library needs `Options.env` / `StackSpec.env` applied to every
   plugin it launches, and `PULUMI_HOME` per stack for the plugin cache.
2. **httpstate credential injection.** `openBackend` stores the token in
   `~/.pulumi/credentials.json` (`workspace.StoreAccount`) because Pulumi's
   HTTP backend has no constructor that takes one. Two runs with different
   tokens for the same backend URL in one process would overwrite each other;
   the worker vends a token per run. The backend needs a per-stack
   credential source (a `PULUMI_CREDENTIALS_PATH`-like override per spec, or
   an `httpstate.New` variant taking an `Account`).
3. **Accept an empty passphrase.** exa-deploy runs every workspace with
   `PULUMI_CONFIG_PASSPHRASE=""`; the spec's `validate` treats `""` as unset.
   Make the field a pointer / `passphraseSet` flag.
4. **Event sequence and timestamps.** Every event arrives with `sequence:
   0, timestamp: 0`; the terminal `CancelEvent` is swallowed. The facade
   numbers events and appends the marker, but the library should emit the
   CLI's stream (exa-deploy's event log contract and the replay recordings
   assume it).
5. **Silence the DIY backend banner.** `pkg/backend/diy/backend.go` prints
   `Updating (stack):` with `fmt.Printf` to the process's stdout on every
   operation; in the worker that lands in the pod log, not in the run's
   stream. Needs a display option or `os.Stdout` redirection in the library.
6. **Stack history, tags, listing.** `Stack.info/history` (in-memory in the
   facade), `listTags/setTag` (exa-deploy sets `exa:journal` and `exa:team`
   on httpstate stacks; best-effort, so it degrades to a warning), and
   `listStacks` (file:// only in the facade).
7. **Update plans** (`preview --save-plan` / `up --plan`, used by the
   plan-bound `up`) and `PulumiCommand.run` replacements for the paths that
   still spawn `pulumi` (`stack export --show-secrets` streaming in
   `stack-export.ts`, state mirrors).

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
| `automation-workspace.ts` `workspaceEnvVars` | `PULUMI_CONFIG_PASSPHRASE: ""`, `NODE_PATH`, `EXA_DEPLOY_PROGRAM` reach the engine child | passphrase substituted; env delivered by the preload stub |
| `temp-workspace.ts` `generatePulumiYaml` | `runtime.options.nodeargs` honoured by the language host | honoured (the stub prepends to it) |
| `automation.ts` `ensureStackTags` | `listTags/setTag` on httpstate stacks | facade rejects; the CLI catches and warns |
| worker `deploy-stage.ts` | `SHIP_PULUMI_HOME` per pod via env | must become a library option (item 1) |

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
