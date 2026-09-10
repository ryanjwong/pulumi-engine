# Upstream asks

Every place the library needed a hook that Pulumi (`pkg/v3`, `sdk/v3` at
v3.237.0) does not expose, what we do instead, and the concrete API that
would let us delete the workaround. The copied code lives under
[`internal/upstream/`](../internal/upstream) (one file per unit, headers
checked by `make upstream-check`); everything else here is a wrapper or a
documented limit.

| # | need | what we do today | proposed upstream API | deletes |
|-|-|-|-|-|
| 1 | **Per-operation environment for language hosts.** `plugin.Host.Provider(descriptor, env.Env)` takes an env that `ExecPlugin` appends to the subprocess environment, but `NewLanguageRuntime` (`sdk/go/common/resource/plugin/langruntime_plugin.go:132`) passes `nil`, so every language host inherits `os.Environ()`. | A `plugin.Host` wrapper (`engine/pluginhost.go`) supplied through `engine.UpdateOptions.Host` overrides `LanguageRuntime` and launches the host itself with `plugin.ExecPlugin` plus a copy of the port-read/dial half of `newPlugin`. | `NewLanguageRuntime(host, ctx, runtime, workingDirectory, env env.Env)` and `Host.LanguageRuntime(runtime string, env env.Env)`, mirroring `Provider`; or export `newPlugin`. | `internal/upstream/launch.go` |
| 2 | **A host on the engine's context.** `UpdateOptions.Host` is used as given, so a caller-built host cannot sit on the `plugin.Context` the engine creates in `ProjectInfoContext`; its `Diag` sink (where plugin stdout/stderr, `pulumi.log` and plugin-path warnings go) is the engine's unexported `newEventSink`. | The wrapper builds its own `plugin.Context` with a copy of the event sink feeding the operation's stream. | `UpdateOptions.NewHost func(*plugin.Context) (plugin.Host, error)` (a factory the engine calls with its context), or `engine.NewEventSink` exported. | `internal/upstream/eventsink.go` |
| 3 | **Credential injection for the HTTP backend.** `httpstate.New` reads the token from `workspace.GetAccount(url)` (the credentials file at `PULUMI_CREDENTIALS_PATH` / `$PULUMI_HOME/credentials.json`); the `cloudBackend` struct is unexported and has no other constructor. | Under a process-wide lock, store the spec's account for the URL, construct the backend (it keeps the token in its own client), restore the file byte for byte. Per spec and concurrency-safe, but the hand-off goes through Pulumi's credentials file for the duration of the constructor, and its location is the process's `PULUMI_HOME`. | `httpstate.NewWithAccount(ctx, sink, url, project, account workspace.Account)` (or `New` taking a `workspace.Context` that resolves credentials). | `withSpecCredentials` in `engine/stack.go` |
| 4 | **Backend banner on process stdout.** `diyBackend.apply` and `cloudBackend.apply` print `"Updating (dev):"` with `fmt.Printf` (`pkg/backend/diy/backend.go:1208`, `httpstate/backend.go:1691`), ignoring `display.Options.Stdout`; so do `displayBackendMessages`, `CreateStack` ("Created stack") and `display/json.go`'s encoder. | The first operation replaces the Go runtime's `os.Stdout` with a pipe; banner lines of running operations become their `stdout` events, everything else is forwarded to the original stdout (`engine/stdout.go`). | Print through `op.Opts.Display.Stdout` (one-line change per site). | `engine/stdout.go` |
| 5 | **Passphrase cache keyed by salt only.** `passphrase.GetPassphraseSecretsManager(phrase, state)` returns a process-wide cached manager for a known `state` without checking `phrase` (`pkg/secrets/passphrase/manager.go:194`). In a process opening one stack for several callers, a wrong passphrase would be accepted after a right one. | Verify the passphrase with a copy of the unexported state check before consulting the cache. | Key the cache by `(state, phrase)`, or export `VerifyPassphrase(phrase, state) error`. | `internal/upstream/passphrase.go` |
| 6 | **In-process language runtime server.** The Go Automation API's inline-program server (`sdk/go/auto/stack.go`, `languageRuntimeServer`) is unexported, and `sdk/go/auto` shells out to the CLI. | Copied. | Export it from a package that does not depend on the CLI (`sdk/go/auto/langserver`?). | `internal/upstream/langserver.go` |
| 7 | **Refresh/destroy events.** `Backend.Refresh` and `Backend.Destroy` take no event channel (`Preview`/`Update` do). | A loopback `Events` gRPC sink through `display.Options.EventLogPath = "tcp://..."` (`engine/eventsink.go`); refresh/destroy events are always secret-redacted because `logJSONEvent` hard-codes it. | Give `Refresh`/`Destroy` the same `events chan<- engine.Event` parameter. | `engine/eventsink.go` |
| 8 | **Event stamping.** `Sequence`/`Timestamp` are assigned only in the display layer (`display.stampEvents`), and `ConvertEngineEvent` leaves them zero. | The operation stamps every event it queues and appends the cancel terminator itself. | `display.StampEvents` (or a `ConvertEngineEvent` variant) exported. | `Operation.push` in `engine/operation.go` |
| 9 | **Per-spec `PULUMI_HOME` for the library's own plugin resolution.** `workspace.GetPluginPath`/`GetPluginDir` read `PULUMI_HOME` through `env.Home` and `GITHUB_TOKEN` in `newGithubSource` (`sdk/go/common/workspace/plugins.go`), `DetectProjectPath` uses the process cwd. | Not scoped: the plugin cache and download credentials are the process's; `StackSpec.Env` reaches the plugin *subprocesses* only. **Documented limit**, no copy. | `workspace.GetPluginPath(ctx, sink, spec, projectPlugins, opts PluginOptions{Home string, GitHubToken string})` or a `workspace.Context` carrying the home dir; `plugin.NewContextWithRoot` taking the project instead of detecting it. | nothing yet |
| 10 | **DIY tags file is never deleted.** `diyBackend.UpdateStackTags` with an empty map skips the write (`saveStackTags`: "Don't write a tags file if there are no tags"), so removing the last tag clears the in-memory copy but leaves `<stack>.pulumi-tags` for other readers. | Documented on `Stack.SetTags`. | Delete the file when the map is empty. | a doc note |
| 11 | **Failed deployments leak the resource monitor.** `deploymentExecutor.Execute` cancels the source iterator (closing the monitor gRPC server) only on success (`pkg/resource/deploy/deployment_executor.go`). | The Node binding unrefs the SDK's sockets after a run; Go callers leak a goroutine and a listener per failed operation. | Cancel the iterator on every exit path. | Node `inline.ts` workaround |
| 12 | **DIY backend env option is unexported.** `diyBackendOptions.Env env.Env` exists (`pkg/backend/diy/backend.go:220`) but only `newDIYBackend` takes it. | Nothing (object-store credentials come from the process environment, as for the CLI). | Export `diy.NewWithOptions`. | a README note |

## Process-global reads on the operation path

`StackSpec.Env` is applied to plugin and language-host subprocesses; it is
never applied to this process. What Pulumi reads from the process environment
while an operation runs (per call unless noted; from
`sdk/go/common/workspace`, `resource/plugin`, `pkg/engine`, `resource/deploy`,
`backend`):

| variable | where | scope in the library |
|-|-|-|
| `PULUMI_HOME` | `workspace/paths.go` `GetPulumiHomeDir` (plugin cache, credentials.json, config.json) | process; plugins see the spec's |
| `GITHUB_TOKEN`, `GITLAB_TOKEN`, `PULUMI_PLUGIN_DOWNLOAD_URL_OVERRIDES` | `workspace/plugins.go` (downloads; `GITHUB_TOKEN` captured once per source) | process |
| `PULUMI_ENABLE_LEGACY_PLUGIN_SEARCH` | `workspace/plugins.go:64`, **package var, read at init** | process |
| `PULUMI_IGNORE_AMBIENT_PLUGINS` | `workspace/plugins.go:2124` | process |
| `PULUMI_DEBUG_PROVIDERS`, `PULUMI_DEBUG_LANGUAGES` | `plugin/provider_plugin.go:147`, `langruntime_plugin.go:166` (attach ports) | process |
| `PULUMI_DISABLE_REFRESH_BEFORE_UPDATE` | `plugin/provider_plugin.go:61`, **package var, read at init** | process |
| `PULUMI_LEGACY_PROVIDER_PREVIEW` | `plugin/provider_plugin.go:282,408` | process |
| `PULUMI_DEBUG_GRPC` | `plugin/context.go:130`, `deploy/source_eval.go:808` | process |
| `PULUMI_DISABLE_AUTOMATIC_PLUGIN_ACQUISITION` | `engine/plugins.go:495`, `update.go:566`, `deploy/providers/registry.go:403` | process |
| `PULUMI_PARALLEL_ANALYZE`, `PULUMI_GOROUTINE_PANIC_RECOVERY` | `deploy/analyze_snapshot.go`, `deploy/goroutine_panic_recovery.go` | process |
| `PULUMI_SKIP_CHECKPOINTS`, `PULUMI_DISABLE_JOURNALING`, `PULUMI_OPTIMIZED_CHECKPOINT_PATCH`, `PULUMI_JOURNAL_BATCH_*` | `backend/journal.go`, `backend/snapshot.go`, `httpstate/backend.go`, `httpstate/journal` | process |
| `PULUMI_API` | `httpstate/backend.go:160` (only when the spec URL is empty; it never is) | unused |
| `PULUMI_ACCESS_TOKEN` | `httpstate/backend.go:439` (`LoginManager.Current`, not on our path); `httpstate.New` does not read it | unused: the token is the spec's |
| `PULUMI_CREDENTIALS_PATH` | `workspace/creds.go:197` | process (the hand-off file, #3) |
| `PULUMI_CONFIG_PASSPHRASE[_FILE]` | `secrets/passphrase/manager.go:380` (`NewPromptingPassphraseSecretsManager`, not on our path) | unused: the passphrase is the spec's |
| `PULUMI_STACK`, `PULUMI_BACKEND_URL` | `backend/state/stacks.go`, `pkg/workspace/creds.go` (CLI defaults, not on our path) | unused: both are spec fields |
| `PULUMI_ENABLE_STREAMING_JSON_PREVIEW`, `TERM` | display layer | irrelevant (display output is discarded) |
| `PULUMI_SKIP_UPDATE_CHECK`, `PULUMI_EXPERIMENTAL` | CLI only | unused |
