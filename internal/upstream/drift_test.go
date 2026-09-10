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

package upstream

import (
	"path/filepath"
	"testing"

	"github.com/ryanjwong/pulumi-engine/internal/upstream/drift"
)

// TestUpstreamDrift is `make upstream-check`: every copied file declares its
// upstream sources, and none of those sources changed between the version
// the copy was reviewed against and the version pinned in go.mod.
func TestUpstreamDrift(t *testing.T) {
	units, err := drift.Scan(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(units) == 0 {
		t.Fatal("no copied units found")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := drift.Check(&drift.Resolver{Root: root}, units)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("upstream drift: %s", f)
	}
	for _, u := range units {
		t.Logf("%s: %d source(s), %d region(s); delete when: %s", filepath.Base(u.File), len(u.Sources), len(u.Regions), u.DeleteWhen)
	}
}
