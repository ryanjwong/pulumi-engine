# pulumi_engine (Python)

The Pulumi engine as an in-process library for Python. Loads `libpulumi`
(the C ABI of [pulumi-engine](../../README.md)) through
[cffi](https://cffi.readthedocs.io) in ABI mode, so there is no compile
step: the shared library is found next to the package or through
`PULUMI_ENGINE_LIB`.

```python
from pulumi_engine import open_stack, Secret, LocalProgram, Options, ResourceOpFailedError

stack = open_stack({
    "name": "dev",
    "project": {"name": "demo"},
    "backend": {"url": "file:///var/lib/demo/state"},
    "secrets": {"provider": "passphrase", "passphrase": os.environ["PASSPHRASE"]},
    "config": {"token": Secret("...")},
    "create": True,
})

# local: a project directory run by its language host
with stack.up(LocalProgram("/path/to/project"), Options(message="deploy")) as op:
    for event in op:                     # engine events, secrets redacted
        print(event["type"])
    try:
        result = op.result()             # typed exceptions
        print(result["changes"], result["outputs"]["values"])
    except ResourceOpFailedError as e:
        print(e.urn, e.op, e.message)

# inline: a function in this process (needs the `inline` extra: pulumi + grpcio)
import pulumi, pulumi_random as random
def program():
    pet = random.RandomPet("pet")
    pulumi.export("name", pet.id)
with stack.up(program) as op:
    print(op.result()["outputs"]["values"]["name"])
```

- `open_stack(spec)` takes a dict with the same keys as the Node binding's
  `StackSpec` (`name`, `project`, `backend`, `secrets`, `config`, `env`,
  `create`); config values are strings, `Secret(...)` or
  `{"value", "secret", "object"}` dicts. A `Stack` is a context manager
  (`close()` on exit).
- `stack.preview/up/refresh/destroy(program=None, options=None)` return an
  `Operation`: an iterator of event dicts (Pulumi's engine event JSON plus
  `type`, `sequence`, `timestamp`; every stream ends with a `cancel`
  terminator after the summary) with `result()` (blocks; returns the result
  dict or raises a typed `PulumiError` subclass carrying the partial result),
  `wait()` (returns `(result, error)`), `cancel()` (graceful; a second call
  terminates), `release()` and `done`. As a context manager, leaving the
  block cancels an operation that is still running, waits for it and
  releases the handle.
- Programs: `LocalProgram(dir, language_version=None)` (or the dict
  `{"mode": "local", "dir": ..., "languageVersion": ...}`),
  `CallbackProgram(address)` (a LanguageRuntime server you run), or a plain
  callable (inline, below).
- Options: an `Options` dataclass (snake_case fields) or a dict with either
  snake_case or the ABI's camelCase keys: `parallel`, `message`, `targets`,
  `target_dependents`, `excludes`, `replaces`, `refresh`, `continue_on_error`,
  `show_secrets`, `dry_run` (refresh/destroy as a preview), `env` (the
  environment for this operation's plugins and language host, over the
  spec's `env`), and the plan options below.
- Update plans: `Options(save_plan=path)` or `generate_plan=True` on a
  preview writes the CLI's plan file (`pulumi preview --save-plan`) or returns
  it in `result()["plan"]`; `Options(plan=path)` or `plan_json=<dict|str>` on
  an up constrains it (`pulumi up --plan`) and raises `PlanViolationError`
  (`.resources` is `[{"urn", "message"}]`) when the program exceeds it.
- Errors (`pulumi_engine.errors`): `PulumiError(kind, message, result)` and
  `InvalidSpecError(field)`, `ResourceOpFailedError(urn, type, op, provider)`,
  `ProgramFailedError`, `ConcurrentUpdateError`, `StackNotFoundError(name)`,
  `StackExistsError(name)`, `PendingOperationsError(urns)`,
  `CancelledError(operation)`, `PlanViolationError(resources)`,
  `UnsupportedError(feature, backend)`.
- `stack.outputs(show_secrets=False)`, `export()`, `import_()`,
  `set_config()`, `get_config()`, `get_tags()`, `set_tags()`,
  `history(limit=, page=, show_secrets=)`, `cancel()`, `remove(force=False)`,
  `close()`; module-level `list_stacks(backend, filter=None)` and
  `version()`.
- Blocking calls (`result()`, event iteration) release the GIL: cffi drops
  it around every foreign call, so other Python threads keep running (a test
  checks a ticker thread makes progress while the main thread waits, and
  cancels an operation from another thread). Event iteration polls with a
  250 ms timeout so `KeyboardInterrupt` is delivered.

## Inline programs

A callable is served as the Pulumi program through the SDK's own
`LanguageServer` (`pulumi.automation._server`, the class the Python
Automation API uses for inline programs) on a loopback gRPC port that the
engine connects to as a callback program. The program runs in this process
(the tests export `os.getpid()` to prove it) and needs `pulumi` and `grpcio`
(`pip install 'pulumi_engine[inline]'`) plus the provider SDKs it imports.
One inline program runs at a time per process, as in the Automation API
(the SDK's runtime settings are process-wide).

Two things the binding does around the SDK's server (a third, clamping the
engine's parallelism value, went away when the engine adopted the CLI's
default of `4 * GOMAXPROCS`: the SDK's server multiplies `parallel` by four
into an int32 field, so an "unlimited" `MaxInt32` overflowed it):

- **Program exceptions** are re-raised as `pulumi.RunError`, so the server
  answers the engine's `Run` with an error and the operation fails as
  `ProgramFailedError` with the message and traceback. (An error-level log
  alone does not fail an operation.)
- **Daemon worker threads.** After a failed deployment Pulumi's engine does
  not shut down its resource monitor (see the repository README, "Upstream
  leak on failed deployments"), so the SDK's `SignalAndWaitForShutdown` call
  inside a `Run` handler can block forever. The gRPC server therefore uses a
  small executor whose workers are daemon threads: a stuck handler is
  abandoned instead of being joined at interpreter exit.

gRPC's Python core registers fork handlers and logs one `fork_posix.cc` line
per provider plugin the engine launches from this process; the lines are
informational. Set `GRPC_ENABLE_FORK_SUPPORT=0` before `grpc` is imported to
silence them if the process never forks Python-side.

## Library lookup

`$PULUMI_ENGINE_LIB`, else `pulumi_engine/native/libpulumi-<darwin|linux>-<amd64|arm64>.<dylib|so>`
bundled in the package, else `build/libpulumi.<ext>` of a repository checkout
containing the package. The release tarballs (`libpulumi-<os>-<arch>.tar.gz`,
see [docs/releasing.md](../../docs/releasing.md)) carry the library; drop it
into `pulumi_engine/native/` or point `PULUMI_ENGINE_LIB` at it.

## Build and test

From the repository root: `make python` builds the library, creates
`bindings/python/.venv` with the binding and its test dependencies
(`pytest`, `pulumi`, `grpcio`, `pulumi-random`) and runs the tests. Tests
need network on first run to download the `random`/`command` providers and
the YAML language host. Python 3.10+.
