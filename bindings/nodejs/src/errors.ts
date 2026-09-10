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

/** Error kinds, mirroring engine.ErrorKind in Go. */
export type ErrorKind =
    | "invalidSpec"
    | "resourceOpFailed"
    | "programFailed"
    | "concurrentUpdate"
    | "stackNotFound"
    | "stackExists"
    | "pendingOperations"
    | "cancelled"
    | "unclassified";

/** Base class of every error raised by the library. */
export class PulumiError extends Error {
    readonly kind: ErrorKind;
    /** Partial operation result, when the error came from a finished operation. */
    readonly result?: unknown;

    constructor(kind: ErrorKind, message: string, result?: unknown) {
        super(message);
        this.name = "PulumiError";
        this.kind = kind;
        this.result = result;
    }
}

export class InvalidSpecError extends PulumiError {
    readonly field: string;
    constructor(field: string, message: string) {
        super("invalidSpec", `${field}: ${message}`);
        this.name = "InvalidSpecError";
        this.field = field;
    }
}

export class ResourceOpFailedError extends PulumiError {
    readonly urn: string;
    readonly type: string;
    readonly op: string;
    readonly provider: string;
    constructor(fields: { urn: string; type: string; op: string; provider: string; message: string }, result?: unknown) {
        super("resourceOpFailed", `${fields.op} ${fields.type} (${fields.urn}): ${fields.message}`, result);
        this.name = "ResourceOpFailedError";
        this.urn = fields.urn;
        this.type = fields.type;
        this.op = fields.op;
        this.provider = fields.provider;
    }
}

export class ProgramFailedError extends PulumiError {
    constructor(message: string, result?: unknown) {
        super("programFailed", message, result);
        this.name = "ProgramFailedError";
    }
}

export class ConcurrentUpdateError extends PulumiError {
    constructor(message: string, result?: unknown) {
        super("concurrentUpdate", message, result);
        this.name = "ConcurrentUpdateError";
    }
}

export class StackNotFoundError extends PulumiError {
    readonly stackName: string;
    constructor(name: string, message: string) {
        super("stackNotFound", message);
        this.name = "StackNotFoundError";
        this.stackName = name;
    }
}

export class StackExistsError extends PulumiError {
    readonly stackName: string;
    constructor(name: string, message: string) {
        super("stackExists", message);
        this.name = "StackExistsError";
        this.stackName = name;
    }
}

export class PendingOperationsError extends PulumiError {
    readonly urns: string[];
    constructor(urns: string[], message: string, result?: unknown) {
        super("pendingOperations", message, result);
        this.name = "PendingOperationsError";
        this.urns = urns;
    }
}

export class CancelledError extends PulumiError {
    readonly operation: string;
    constructor(operation: string, message: string, result?: unknown) {
        super("cancelled", message, result);
        this.name = "CancelledError";
        this.operation = operation;
    }
}

/** Build the right Error subclass from the ABI's error JSON. */
export function errorFromJSON(json: string): PulumiError {
    let e: Record<string, unknown>;
    try {
        e = JSON.parse(json);
    } catch {
        return new PulumiError("unclassified", json);
    }
    const message = String(e.message ?? "unknown error");
    const result = e.result;
    switch (e.kind as ErrorKind) {
        case "invalidSpec":
            return new InvalidSpecError(String(e.field ?? ""), message);
        case "resourceOpFailed":
            return new ResourceOpFailedError(
                {
                    urn: String(e.urn ?? ""),
                    type: String(e.type ?? ""),
                    op: String(e.op ?? ""),
                    provider: String(e.provider ?? ""),
                    message,
                },
                result,
            );
        case "programFailed":
            return new ProgramFailedError(message, result);
        case "concurrentUpdate":
            return new ConcurrentUpdateError(message, result);
        case "stackNotFound":
            return new StackNotFoundError(String(e.name ?? ""), message);
        case "stackExists":
            return new StackExistsError(String(e.name ?? ""), message);
        case "pendingOperations":
            return new PendingOperationsError((e.urns as string[]) ?? [], message, result);
        case "cancelled":
            return new CancelledError(String(e.operation ?? ""), message, result);
        default:
            return new PulumiError("unclassified", message, result);
    }
}
