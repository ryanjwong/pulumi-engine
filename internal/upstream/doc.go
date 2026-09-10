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

// Package upstream is the single fork boundary between this repository and
// pulumi/pulumi: every piece of Pulumi code that is copied or re-implemented
// because no public API provides the behaviour lives here, one file per
// copied unit. Nothing outside this package reaches into behaviour a public
// Pulumi API could provide.
//
// Each file carries a header the drift check (internal/upstream/drift, run by
// `make upstream-check`) parses:
//
//	// upstream: <module>@<version> <path within module> sha256=<hex>
//	// upstream-reason: why the code is copied
//	// upstream-delete-when: the upstream change that would let us delete it
//
// and marks the copied region with
//
//	// upstream-begin: <path within module> (<what>)
//	...
//	// upstream-end: <path within module>
//
// The check recomputes the sha256 of every referenced upstream file at the
// version pinned in go.mod and fails, with a unified diff of the upstream
// file between the recorded and the pinned version, when it changed. After
// reviewing the diff and porting what matters, `go run
// ./internal/upstream/cmd/upstreamcheck -update` rewrites the headers.
// docs/upstream.md lists the API each copy would be replaced by.
package upstream
