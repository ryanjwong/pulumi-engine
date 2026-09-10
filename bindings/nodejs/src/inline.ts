// Copyright 2026 Ryan Wong
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Inline programs: run a JavaScript function as the Pulumi program in this
// process. This uses the same LanguageServer class @pulumi/pulumi's
// Automation API uses for inline programs (automation/server), served on a
// loopback gRPC port that the engine connects to as a "callback" program.
// @pulumi/pulumi and @grpc/grpc-js are optional peer dependencies; they are
// only required when an inline program is used.

import * as net from "node:net";

import { InvalidSpecError } from "./errors";

export interface InlineServer {
    address: string;
    close(): Promise<void>;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyModule = any;

function load(name: string): AnyModule {
    try {
        // eslint-disable-next-line @typescript-eslint/no-require-imports
        return require(name);
    } catch (e) {
        throw new InvalidSpecError(
            "program",
            `inline programs need ${name} installed in the calling project (${(e as Error).message})`,
        );
    }
}

export async function startInlineServer(program: () => unknown): Promise<InlineServer> {
    const grpc = load("@grpc/grpc-js");
    const { LanguageServer } = load("@pulumi/pulumi/automation/server");
    const langrpc = load("@pulumi/pulumi/proto/language_grpc_pb");
    let channelOptions: Record<string, unknown> = {};
    try {
        channelOptions = load("@pulumi/pulumi/runtime").grpcChannelOptions ?? {};
    } catch {
        // older SDKs: defaults are fine
    }

    // The SDK's LanguageServer runs the program inside its own async-local
    // store and closes the process-wide monitor/engine clients afterwards,
    // but not the store-scoped ones it actually used (in the Automation API
    // the CLI process exiting closes the peer, so it never shows). With the
    // engine in this process a failed run can leave a live socket behind
    // that keeps the event loop alive, so capture the run's store and close
    // its clients ourselves.
    const stores: AnyModule[] = [];
    const wrapped = async () => {
        try {
            stores.push(load("@pulumi/pulumi/runtime/state").getStore());
        } catch {
            // best effort
        }
        try {
            return await program();
        } catch (e) {
            // Report the failure as an error diagnostic instead of letting it
            // escape: an exception out of the program makes the SDK reject the
            // stack's output promises, which surface as process-level
            // unhandled rejections (the SDK relies on a process handler it
            // installs for the duration of the run). The engine still fails
            // the operation because an error was logged, and the message
            // reaches the caller as ProgramFailedError.
            const message = e instanceof Error ? (e.stack ?? e.message) : String(e);
            await load("@pulumi/pulumi/log").error(`program failed: ${message}`);
            return undefined;
        }
    };

    const server = new grpc.Server({ ...channelOptions });
    const languageServer = new LanguageServer(wrapped);
    // LanguageServer.run reports a program error to the engine and then also
    // rethrows it from its own promise, which nobody awaits (grpc-js ignores
    // the handler's return value). In the CLI-driven Automation API that
    // stray rejection is invisible; here it would surface as an unhandled
    // rejection after the operation has already failed with the same error.
    const originalRun = languageServer.run.bind(languageServer);
    languageServer.run = (call: unknown, callback: unknown) => {
        const p = originalRun(call, callback);
        if (p && typeof p.catch === "function") {
            p.catch(() => undefined);
        }
        return p;
    };
    server.addService(langrpc.LanguageRuntimeService, languageServer);
    const port: number = await new Promise((resolve, reject) => {
        server.bindAsync("127.0.0.1:0", grpc.ServerCredentials.createInsecure(), (err: Error | null, p: number) => {
            if (err) {
                reject(err);
            } else {
                resolve(p);
            }
        });
    });
    return {
        address: `127.0.0.1:${port}`,
        close: async () => {
            try {
                // Surface leaked promises like the Automation API does; a
                // failed program already reported its error.
                languageServer.onPulumiExit(false);
            } catch {
                // ignore leak diagnostics on close
            }
            server.forceShutdown();
            try {
                // Closes the process-wide monitor/engine clients the SDK keeps for
                // calls made outside the run's async-local store. The SDK does
                // this itself at the end of a successful run; after a failed
                // resource its own cleanup waits forever for an RPC that will
                // never complete.
                load("@pulumi/pulumi/runtime/settings").disconnectSync();
            } catch {
                // best effort
            }
            // Pulumi's engine does not shut down its resource monitor when a
            // deployment fails (deploymentExecutor.Execute only cancels the
            // source iterator on success; the CLI never notices because it
            // exits). The SDK's SignalAndWaitForShutdown call to that monitor
            // then never returns and its socket keeps the event loop alive. Unref
            // those sockets (to the monitor/engine addresses this run used) so
            // the process can exit; destroying them instead would turn the
            // SDK's pending promises into stray unhandled rejections.
            const ports = new Set<number>();
            for (const store of stores) {
                for (const addr of [store?.settings?.options?.monitorAddr, store?.settings?.options?.engineAddr]) {
                    const port = Number(String(addr ?? "").split(":").pop());
                    if (port > 0) {
                        ports.add(port);
                    }
                }
            }
            try {
                // eslint-disable-next-line @typescript-eslint/no-explicit-any
                const handles: unknown[] = (process as any)._getActiveHandles?.() ?? [];
                for (const h of handles) {
                    if (h instanceof net.Socket && h.remotePort !== undefined && ports.has(h.remotePort)) {
                        h.unref();
                    }
                }
            } catch {
                // internal API; best effort
            }
            for (const store of stores) {
                for (const client of [store?.settings?.monitor, store?.settings?.engine]) {
                    try {
                        client?.close();
                    } catch {
                        // already closed
                    }
                }
                try {
                    store?.callbacks?.shutdown();
                } catch {
                    // ignore
                }
            }
        },
    };
}
