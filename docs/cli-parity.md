# CLI parity

The library is not the `pulumi` CLI, but it drives the same `pkg/v3` engine
and backends, so a stack it writes must be indistinguishable from one the CLI
writes and vice versa. `engine/parity_test.go` (Go) and
`bindings/nodejs/test/parity.test.ts` (Node) prove that by running the same
program under the library and under the real CLI against one
`file://<tmp>` backend with one passphrase, in both directions.

Run them with `make parity` (or `make integration`, which includes them). They
need `PULUMI_ENGINE_INTEGRATION=1` and a `pulumi` binary on `PATH`
(`PULUMI_ENGINE_CLI=/path/to/pulumi` overrides). CI installs the CLI with
`pulumi/actions` pinned to `PULUMI_CLI_VERSION` in `.github/workflows/ci.yml`,
which is kept equal to the `pkg/v3` pin in `go.mod`.

Programs: `engine/testdata/yaml-parity` (YAML: `random:RandomPet`,
`random:RandomPassword` as a secret output, a config value declared
`secret: true` echoed as an output) and, for locks and cancellation,
`engine/testdata/yaml-slow` (`command:local:Command` with `sleep 6`). The Node
test writes a Node project whose `index.js` exports the program function, and
uses that same function as the binding's inline program.

## Results

Verified against pulumi v3.218.0 (Homebrew), v3.237.0 (the pinned `pkg/v3`
version, what CI uses) and v3.250.0 (nix) on darwin/arm64, with
`pulumi-random` and `pulumi-command` providers and the YAML and Node language
hosts. Every row passes on all three CLI versions unless noted.

| case | library -> CLI | CLI -> library | notes |
|-|-|-|-|
| 1. state | library `up`; `pulumi stack export` decodes as `apitype.DeploymentV3`, `secrets_providers.type == passphrase`, 4 resources; `stack export` and `Stack.Export` are the same JSON document; `pulumi preview --expect-no-changes`, `refresh --expect-no-changes`, `destroy`, `stack rm` all succeed; the library then reports `StackNotFound` | `pulumi stack init/config set/up`; library `Open` (no create), `GetConfig` decrypts, `Preview` is 3x `same`, `Refresh` no-op, `Destroy` 3x `delete`; `pulumi stack ls --json` shows `resourceCount: 0`; `Stack.Remove` makes `pulumi stack ls` forget it and `pulumi stack rm` fail | Also passes with the Node binding's inline program in both directions (`parity.test.ts`); the CLI runs the identical `index.js` through `pulumi-language-nodejs`. |
| 1/2. secrets | `RandomPassword.result` and the secret config output are `{sig, ciphertext}` objects in the checkpoint; the plaintext never appears in the export; `pulumi stack output --json` gives `[secret]`, `--show-secrets` gives the same map `Stack.Outputs(true)` returns | `Stack.Outputs(false)` equals `pulumi stack output --json`; `Outputs(true)` equals `--show-secrets`; `SecretKeys` is `[apiKey token]` | Same salt, same passphrase, same `Pulumi.<stack>.yaml`, so ciphertexts written by one side decrypt on the other. |
| 3. events | `pulumi up/preview/refresh/destroy --event-log` vs library events: identical multiset of step events (kind, op, type, name), identical prelude config keys, identical summary counts; secret outputs redacted identically (`{sig, "ciphertext": "[secret]"}`); both streams start with the prelude (after diagnostics) and end with the `cancel` terminator; library events are numbered from 0 with non-zero timestamps like the CLI's | n/a | Two differences, both explained below: the CLI emits one extra `warning` diagnostic about where it expected its language host, and the library carries the backend's `Updating (dev):` banner as a `stdout` event where the CLI prints it outside the log. Before phase one the library dropped default-provider steps for preview/up (mismatch, fixed; see findings). |
| 4. config | `SetConfig` plain, secret and object values are read by `pulumi config get`, `config --json` (`{"secret": true}`, no value) and `config --json --show-secrets`; the CLI validates and accepts the file after the library rewrites it | `pulumi config set`, `--secret` and `--path` values are read by a fresh `Open` + `GetConfig` with the right `Secret`/`Object` flags | One `Pulumi.<stack>.yaml` written by both: one `encryptionsalt`, project-scoped keys, `secure:` entries, no plaintext secrets; `workspace.LoadProjectStack` loads it. |
| 5. locks | while a library `up` is at its first `command:local:Command`, `pulumi up` exits 1 with `the stack is currently locked by 1 lock(s) ... pulumi cancel` | while `pulumi up` is at the same point, library `Up` returns `ConcurrentUpdate` wrapping the same message, in ~1 ms | Both sides use Pulumi's DIY lock files under `<backend>/.pulumi/locks`. |
| 6. cancellation | library `Cancel` mid-up, then `pulumi stack export`: `pending_operations` empty, resources `{stack, providers, first}`; `pulumi refresh` and `destroy` recover it | `SIGINT` to `pulumi up` mid-up, then `Stack.Export`: same resources, `pending_operations` empty; library `Refresh` and `Destroy` recover it | The in-flight `sleep 6` create is killed by `pulumi-command` on `SignalCancellation` and recorded as a failed create on both sides (`signal: killed`), so nothing is left pending. Both checkpoints have the same shape. |
| 7. plugins | with a private `PULUMI_HOME`: `pulumi plugin install resource random 4.16.8`, then `workspace.GetPluginPath` resolves to `$PULUMI_HOME/plugins/resource-random-v4.16.8/`, `workspace.GetPlugins` equals `pulumi plugin ls --json`, and a library `up` uses the cached plugin without touching the cache | an ambient `pulumi-resource-random` launcher on `PATH` (a script that records its parent pid and execs the real plugin) is what `GetPluginPath` returns and what both a library preview and a `pulumi preview` launch (marker has the test pid and the CLI pid respectively); both warn `using pulumi-resource-random from $PATH` | v3.250.0 launches the ambient provider twice for one preview (schema load and the step) where v3.218/3.237 and the library launch it once. |
| 8. version skew | v3.218.0, v3.237.0, v3.250.0 all pass | same | The local Homebrew CLI (3.218) is 19 minors behind the pin; nothing in these cases differed. `pulumi --event-log` needs `PULUMI_DEBUG_COMMANDS=true` on every version. |
| 9. tags (phase two) | library `SetTags({"exa:team": "infra"})` on a `file://` stack; `pulumi stack tag ls --json` (v3.218.0) prints it | `pulumi stack tag set exa:journal j9`; the library's `GetTags` (and `ListStacks` with `TagName`/`TagValue`) sees both | Both write `<stack>.pulumi-tags` (`{"version": 1, "tags": {...}}`) beside the checkpoint. Checked by hand, not in the suite. `pulumi stack history --json` on a `file://` backend prints the same entries the library's `History` returns (kind, message, timestamps; no version numbers on DIY). |
| 10. update plans (phase three) | library `Preview` with `SavePlan` on the YAML fixture; `pulumi up --plan=<file> --yes` applies it (3 creates); a second library plan of `same` steps, then `pulumi config set petLength 3` and `pulumi up --plan` exits 1 with `violates plan`, the pet untouched | `pulumi preview --save-plan`; library `Up` with `Plan: file` creates 3; a second CLI plan, then `SetConfig(petLength=3)` and `Up` with `PlanJSON` returns `PlanViolation` naming `::pet`, nothing applied | Same `DeploymentPlanV1` document both ways (`stack.SerializePlan` with the stack's encrypter, so secret goal values are ciphertexts the other side decrypts). The library-written plan's `manifest.version` is empty (it is the CLI binary's version string); the CLI does not check it. `TestIntegrationParityPlans`; CLI v3.218.0 locally, v3.237.0 in CI. |

## Findings

**Library bug, fixed: default-provider step events were dropped for preview
and up.** The engine marks default provider steps (and the refresh steps of
`up --refresh`) as internal. `display.ShowEvents` writes the `--event-log`
*before* filtering internal events and drops them only for the terminal
display and `--json`. The library filtered `e.Internal()` on the preview/up
channel, but its refresh/destroy events come through the loopback event
sink, which is the event-log path and includes them. So `up` reported
`create=3` with three step events while `destroy` reported `delete=3` with
four, and a `pulumi up --event-log` had four. `engine/operation.go` now
passes internal events through for every operation: the library's stream is
the Automation API's stream. The event fixtures under
`engine/testdata/events` were re-recorded.

**Fixed in phase two: the library's stream now ends with the `cancel`
event too.** Pulumi's engine emits a `cancelEvent` as a stream terminator and
`--event-log` writes it; phase one consumed it and closed the channel. The
library now numbers every event from 0, stamps it with the Unix time (what
`display.stampEvents` does for the CLI) and appends the terminator itself,
after the last event from every source (engine channel, event sink, plugin
host, captured stdout), also for operations that fail before the engine
starts. The Node facade no longer synthesizes any of this.

**Expected, documented: the banner is a `stdout` event in the library.**
The backends print `Updating (dev):` to the process's stdout with
`fmt.Printf`; the CLI's event log never sees it. The library captures it and
delivers it as a `stdout` event of the operation (README, "Stdout"), so its
stream has one `stdout` event the CLI's log lacks; the comparison treats it
like a diagnostic.

**Expected, documented: one extra CLI diagnostic.** The CLI warns
`using pulumi-language-yaml from $PATH at ... expected <dir of pulumi
binary>/pulumi-language-yaml` because it ships language hosts next to
itself. The library has no such expectation and emits no warning. Both
warn identically about ambient resource providers on `PATH`. Diagnostics are
compared by severity and reported, not failed on, for this reason.

**Expected: `pulumi config --json` without `--show-secrets` omits the value
of a secret** (`{"secret": true}`) rather than writing `[secret]`.
`Stack.GetConfig` always decrypts; redaction is the caller's job.

**Expected: `pulumi config get` validates the whole stack config**, so a
project that declares `secret: true` config refuses every `config` command
until that key is set. `Stack.GetConfig` does not validate; validation
happens at operation start (`ValidateStackConfigAndApplyProjectConfig`), the
same call the CLI's `up` makes.

**Tooling: overwriting the dylib in place kills Node with SIGKILL on
macOS.** `make node-build` copied `libpulumi.dylib` over the previous file;
macOS invalidates the cached code signature of a modified vnode and kills
the next process that pages the library in. The Makefile now removes the
file before copying. Unrelated to Pulumi, but it cost an hour.

## Not verified

- HTTP backends (Pulumi Cloud): the parity suite is file-backend only. The
  credential hand-off, tags, history and listing against the HTTP backend
  are covered by an httptest server (`engine/phase2_test.go`), not a real
  service.
- KMS secrets providers.
- Windows, or Linux other than CI's ubuntu-latest.
- Terminate-immediately (second Ctrl-C / second `Cancel`) checkpoints.
- `up --refresh` internal refresh steps against a CLI event log (the change
  above makes the library emit them; only default-provider steps are
  exercised by the fixture).
