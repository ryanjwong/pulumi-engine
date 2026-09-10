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

package engine

import (
	"runtime/debug"
	"sync"
)

// LibraryVersion is the version of this library. Release builds set it from
// the tag with `-ldflags "-X github.com/ryanjwong/pulumi-engine/engine.LibraryVersion=X.Y.Z"`
// (see the Makefile's VERSION); development builds report the default.
var LibraryVersion = "0.1.0-dev"

var (
	pulumiVersionOnce sync.Once
	pulumiVersion     string
)

// PulumiVersion returns the version of github.com/pulumi/pulumi/pkg/v3 the
// library was built against, as recorded in the binary's build info.
func PulumiVersion() string {
	pulumiVersionOnce.Do(func() {
		pulumiVersion = "unknown"
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/pulumi/pulumi/pkg/v3" {
				pulumiVersion = dep.Version
				if dep.Replace != nil {
					pulumiVersion = dep.Replace.Version
				}
				return
			}
		}
	})
	return pulumiVersion
}

// Version returns a human-readable version string for the library and the
// Pulumi packages it embeds.
func Version() string {
	return "pulumi-engine " + LibraryVersion + " (pulumi " + PulumiVersion() + ")"
}
