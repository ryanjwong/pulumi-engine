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

// Package drift checks the copied Pulumi code under internal/upstream against
// the pulumi/pulumi modules pinned in go.mod. See internal/upstream/doc.go for
// the header and marker format it parses.
package drift

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Source is one upstream file a copied unit was taken from.
type Source struct {
	Module  string // e.g. github.com/pulumi/pulumi/pkg/v3
	Version string // the version the copy was reviewed against
	Path    string // path within the module
	SHA256  string // sha256 of the upstream file at Version
	Line    int    // line of the header entry in the copied file (1-based)
}

// Region is a marked copied region.
type Region struct {
	Path  string // upstream path the region was copied from
	Begin int    // line of the upstream-begin marker
	End   int    // line of the upstream-end marker (0 when missing)
}

// Unit is one copied file.
type Unit struct {
	File       string
	Sources    []Source
	Regions    []Region
	Reason     string
	DeleteWhen string
}

// Finding is one drift-check failure.
type Finding struct {
	File    string
	Source  Source
	Message string
	Diff    string // unified diff of the upstream file between recorded and pinned version, when available
}

func (f Finding) String() string {
	s := fmt.Sprintf("%s: %s %s: %s", f.File, f.Source.Module, f.Source.Path, f.Message)
	if f.Diff != "" {
		s += "\n" + f.Diff
	}
	return s
}

var (
	sourceRe     = regexp.MustCompile(`^//\s*upstream:\s+(\S+)@(\S+)\s+(\S+)\s+sha256=([0-9a-f]{64})\s*$`)
	reasonRe     = regexp.MustCompile(`^//\s*upstream-reason:\s*(.*)$`)
	deleteWhenRe = regexp.MustCompile(`^//\s*upstream-delete-when:\s*(.*)$`)
	beginRe      = regexp.MustCompile(`^//\s*upstream-begin:\s+(\S+)`)
	endRe        = regexp.MustCompile(`^//\s*upstream-end:\s+(\S+)`)
)

// Scan parses every .go file directly under dir (not subdirectories) that
// carries an upstream header. Files without a header are reported as an
// error: everything in the fork boundary must declare its origin.
func Scan(dir string) ([]Unit, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var units []Unit
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "doc.go" {
			continue
		}
		path := filepath.Join(dir, name)
		u, err := parseUnit(path)
		if err != nil {
			return nil, err
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].File < units[j].File })
	return units, nil
}

func parseUnit(path string) (Unit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Unit{}, err
	}
	u := Unit{File: path}
	var open *Region
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		switch {
		case sourceRe.MatchString(line):
			m := sourceRe.FindStringSubmatch(line)
			u.Sources = append(u.Sources, Source{Module: m[1], Version: m[2], Path: m[3], SHA256: m[4], Line: n})
		case reasonRe.MatchString(line):
			u.Reason = reasonRe.FindStringSubmatch(line)[1]
		case deleteWhenRe.MatchString(line):
			u.DeleteWhen = deleteWhenRe.FindStringSubmatch(line)[1]
		case beginRe.MatchString(line):
			if open != nil {
				return Unit{}, fmt.Errorf("%s:%d: upstream-begin inside an open region (begun at line %d)", path, n, open.Begin)
			}
			u.Regions = append(u.Regions, Region{Path: beginRe.FindStringSubmatch(line)[1], Begin: n})
			open = &u.Regions[len(u.Regions)-1]
		case endRe.MatchString(line):
			if open == nil {
				return Unit{}, fmt.Errorf("%s:%d: upstream-end without upstream-begin", path, n)
			}
			if p := endRe.FindStringSubmatch(line)[1]; p != open.Path {
				return Unit{}, fmt.Errorf("%s:%d: upstream-end for %q closes a region of %q", path, n, p, open.Path)
			}
			open.End = n
			open = nil
		}
	}
	if open != nil {
		return Unit{}, fmt.Errorf("%s: region begun at line %d is never closed", path, open.Begin)
	}
	if len(u.Sources) == 0 {
		return Unit{}, fmt.Errorf("%s: no `// upstream: <module>@<version> <path> sha256=<hex>` header", path)
	}
	if len(u.Regions) == 0 {
		return Unit{}, fmt.Errorf("%s: no upstream-begin/upstream-end region", path)
	}
	if u.Reason == "" || u.DeleteWhen == "" {
		return Unit{}, fmt.Errorf("%s: header needs upstream-reason and upstream-delete-when", path)
	}
	declared := map[string]bool{}
	for _, s := range u.Sources {
		declared[s.Path] = true
	}
	for _, r := range u.Regions {
		if !declared[r.Path] {
			return Unit{}, fmt.Errorf("%s:%d: region of %q has no matching upstream: header line", path, r.Begin, r.Path)
		}
	}
	return u, nil
}

// Module is a resolved Go module.
type Module struct {
	Path    string
	Version string
	Dir     string
}

// Resolver locates module versions through the go tool, caching results.
type Resolver struct {
	// Root is the directory holding go.mod.
	Root  string
	cache map[string]Module
}

// Pinned returns the version of module pinned in go.mod and its directory in
// the module cache.
func (r *Resolver) Pinned(module string) (Module, error) {
	return r.resolve(module, "")
}

// At returns the directory of module at version, downloading it if needed.
func (r *Resolver) At(module, version string) (Module, error) {
	return r.resolve(module, version)
}

func (r *Resolver) resolve(module, version string) (Module, error) {
	key := module + "@" + version
	if m, ok := r.cache[key]; ok {
		return m, nil
	}
	var out bytes.Buffer
	var cmd *exec.Cmd
	if version == "" {
		cmd = exec.Command("go", "list", "-m", "-json", module)
	} else {
		cmd = exec.Command("go", "mod", "download", "-json", key)
	}
	cmd.Dir = r.Root
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return Module{}, fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	var m Module
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return Module{}, fmt.Errorf("decoding %s: %w", strings.Join(cmd.Args, " "), err)
	}
	if m.Dir == "" {
		if version == "" {
			// go list does not download; fetch the pinned version explicitly.
			return r.resolve(module, m.Version)
		}
		return Module{}, fmt.Errorf("%s: module directory unknown", key)
	}
	if r.cache == nil {
		r.cache = map[string]Module{}
	}
	r.cache[key] = m
	return m, nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Check compares every source of every unit against the pinned module
// version. It returns one Finding per changed upstream file, with a unified
// diff of the upstream file between the recorded and the pinned version when
// the recorded version can be fetched.
func Check(r *Resolver, units []Unit) ([]Finding, error) {
	var findings []Finding
	for _, u := range units {
		for _, s := range u.Sources {
			pinned, err := r.Pinned(s.Module)
			if err != nil {
				return nil, err
			}
			path := filepath.Join(pinned.Dir, filepath.FromSlash(s.Path))
			sum, err := fileSHA256(path)
			if errors.Is(err, os.ErrNotExist) {
				findings = append(findings, Finding{File: u.File, Source: s,
					Message: fmt.Sprintf("file no longer exists at %s", pinned.Version)})
				continue
			}
			if err != nil {
				return nil, err
			}
			if sum == s.SHA256 {
				continue
			}
			f := Finding{File: u.File, Source: s, Message: fmt.Sprintf(
				"changed between %s (recorded) and %s (pinned); review the copied region and run upstreamcheck -update",
				s.Version, pinned.Version)}
			if old, err := r.At(s.Module, s.Version); err == nil {
				f.Diff = unifiedDiff(filepath.Join(old.Dir, filepath.FromSlash(s.Path)), s.Version, path, pinned.Version)
			} else {
				f.Message += fmt.Sprintf(" (no diff: %v)", err)
			}
			findings = append(findings, f)
		}
	}
	return findings, nil
}

func unifiedDiff(oldPath, oldVersion, newPath, newVersion string) string {
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		return "(no diff: " + err.Error() + ")"
	}
	newData, err := os.ReadFile(newPath)
	if err != nil {
		return "(no diff: " + err.Error() + ")"
	}
	text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(oldData)),
		B:        difflib.SplitLines(string(newData)),
		FromFile: oldVersion,
		ToFile:   newVersion,
		Context:  3,
	})
	if err != nil {
		return "(no diff: " + err.Error() + ")"
	}
	return text
}

// Update rewrites the header lines of u so that each source records the
// pinned version and its sha256. It is for use after a human has reviewed
// the drift and ported whatever mattered.
func Update(r *Resolver, u Unit) (changed bool, err error) {
	data, err := os.ReadFile(u.File)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(data), "\n")
	for _, s := range u.Sources {
		pinned, err := r.Pinned(s.Module)
		if err != nil {
			return false, err
		}
		sum, err := fileSHA256(filepath.Join(pinned.Dir, filepath.FromSlash(s.Path)))
		if err != nil {
			return false, err
		}
		line := fmt.Sprintf("// upstream: %s@%s %s sha256=%s", s.Module, pinned.Version, s.Path, sum)
		if lines[s.Line-1] != line {
			lines[s.Line-1] = line
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, os.WriteFile(u.File, []byte(strings.Join(lines, "\n")), 0o644)
}
