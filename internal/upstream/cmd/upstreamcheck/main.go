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

// Command upstreamcheck reports drift between the copied Pulumi code under
// internal/upstream and the pulumi/pulumi version pinned in go.mod, and with
// -update rewrites the copied files' headers to the pinned version after the
// drift has been reviewed.
//
//	go run ./internal/upstream/cmd/upstreamcheck            # check (make upstream-check)
//	go run ./internal/upstream/cmd/upstreamcheck -update    # accept the pinned version
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ryanjwong/pulumi-engine/internal/upstream/drift"
)

func main() {
	update := flag.Bool("update", false, "rewrite headers to the pinned version and sha256 (after review)")
	dir := flag.String("dir", "internal/upstream", "directory holding the copied units")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	units, err := drift.Scan(*dir)
	if err != nil {
		fatal(err)
	}
	r := &drift.Resolver{Root: root}
	if *update {
		for _, u := range units {
			changed, err := drift.Update(r, u)
			if err != nil {
				fatal(err)
			}
			if changed {
				fmt.Printf("updated %s\n", u.File)
			}
		}
	}
	findings, err := drift.Check(r, units)
	if err != nil {
		fatal(err)
	}
	for _, u := range units {
		fmt.Printf("%s: %d source(s), %d copied region(s)\n", filepath.Base(u.File), len(u.Sources), len(u.Regions))
		for _, s := range u.Sources {
			fmt.Printf("  %s@%s %s\n", s.Module, s.Version, s.Path)
		}
	}
	if len(findings) == 0 {
		fmt.Println("upstream-check: no drift")
		return
	}
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "upstream drift: %s\n", f)
	}
	fmt.Fprintf(os.Stderr, "upstream-check: %d changed upstream file(s); review the copied regions, then run with -update\n", len(findings))
	os.Exit(1)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "upstreamcheck:", err)
	os.Exit(2)
}
