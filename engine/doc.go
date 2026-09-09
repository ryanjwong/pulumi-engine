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

// Package engine is the Pulumi deployment engine as an in-process library.
//
// It links Pulumi's own engine, backend, plugin-management and secrets
// packages into the calling process. There is no CLI and no daemon. Provider
// plugins are still subprocesses, because that is what the Pulumi provider
// protocol requires; the library starts and owns them inside the caller's
// process tree.
package engine
