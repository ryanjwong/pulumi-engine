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
