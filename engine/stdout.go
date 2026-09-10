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
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
)

// Pulumi's backends print one header line per operation ("Updating (dev):")
// with fmt.Printf, to the process's stdout, ignoring display.Options.Stdout
// (pkg/backend/diy/backend.go and httpstate/backend.go, `apply`). A library
// has no business writing to its host's stdout, so the first operation
// replaces the Go runtime's os.Stdout with a pipe. Lines that are the header
// of a running operation become that operation's stdout event; every other
// byte is forwarded to the original stdout unchanged, so a Go host that
// prints through os.Stdout is not affected beyond the forwarding hop.
//
// Only Go code sees os.Stdout: for the shared library this is the whole of
// the library's output, and the host process's own file descriptor 1 is
// untouched. SetStdoutCapture(false) before the first operation opts out.

var stdoutCapture = struct {
	sync.Mutex
	enabled bool
	started bool
	orig    *os.File
	pipe    *os.File
	// listeners are the running operations, keyed by the stack reference
	// their banner names.
	listeners map[string][]*Operation
	// flushes are waiters for a sentinel line to come through the pipe.
	flushes map[string]chan struct{}
	flushN  int
}{enabled: true, listeners: map[string][]*Operation{}, flushes: map[string]chan struct{}{}}

const flushSentinel = "\x00pulumi-engine-stdout-flush:"

// SetStdoutCapture enables or disables the capture of Pulumi's stray stdout
// writes into operation events (see stdout.go). It is enabled by default and
// takes effect for operations started after the call; once the capture has
// started it cannot be undone.
func SetStdoutCapture(enabled bool) {
	stdoutCapture.Lock()
	defer stdoutCapture.Unlock()
	stdoutCapture.enabled = enabled
}

// bannerRe matches the operation header lines the backends print.
var bannerRe = regexp.MustCompile(`^(Previewing update|Previewing refresh|Previewing destroy|Updating|Refreshing|Destroying|Importing|Renaming|Previewing import) \((.+)\):?\s*$`)

// bannerPrefixes are the first words of banner lines, for deciding whether a
// partial line may still become one.
var bannerPrefixes = []string{"Previewing ", "Updating ", "Refreshing ", "Destroying ", "Importing ", "Renaming "}

// startStdoutCapture swaps os.Stdout for the pipe, once.
func startStdoutCapture() {
	stdoutCapture.Lock()
	defer stdoutCapture.Unlock()
	if !stdoutCapture.enabled || stdoutCapture.started {
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	stdoutCapture.started = true
	stdoutCapture.orig = os.Stdout
	stdoutCapture.pipe = w
	os.Stdout = w
	go pumpStdout(r, stdoutCapture.orig)
}

// pumpStdout reads the pipe, routes banner lines and forwards the rest.
func pumpStdout(r io.Reader, orig io.Writer) {
	br := bufio.NewReader(r)
	var partial strings.Builder
	for {
		chunk, err := br.ReadString('\n')
		if chunk != "" {
			partial.WriteString(chunk)
			if strings.HasSuffix(chunk, "\n") {
				line := partial.String()
				partial.Reset()
				if !routeStdoutLine(line) {
					_, _ = io.WriteString(orig, line)
				}
			} else if !maybeBanner(partial.String()) {
				// A partial line that cannot become a banner is the host's
				// own output (a prompt, binary data); do not hold it back.
				_, _ = io.WriteString(orig, partial.String())
				partial.Reset()
			}
		}
		if err != nil {
			if partial.Len() > 0 {
				_, _ = io.WriteString(orig, partial.String())
			}
			return
		}
	}
}

func maybeBanner(partial string) bool {
	if strings.HasPrefix(partial, flushSentinel) || strings.HasPrefix(flushSentinel, partial) {
		return true
	}
	for _, p := range bannerPrefixes {
		if strings.HasPrefix(p, partial) || strings.HasPrefix(partial, p) {
			return true
		}
	}
	return false
}

// flushStdoutCapture writes a sentinel through the pipe and waits until the
// pump has read it, so that everything Pulumi printed before (the banner) has
// been routed. No-op when the capture is not running.
func flushStdoutCapture() {
	stdoutCapture.Lock()
	if !stdoutCapture.started {
		stdoutCapture.Unlock()
		return
	}
	stdoutCapture.flushN++
	id := fmt.Sprint(stdoutCapture.flushN)
	ch := make(chan struct{})
	stdoutCapture.flushes[id] = ch
	pipe := stdoutCapture.pipe
	stdoutCapture.Unlock()
	if _, err := io.WriteString(pipe, flushSentinel+id+"\n"); err != nil {
		return
	}
	<-ch
}

// routeStdoutLine delivers a banner line to the operation whose stack it
// names. It reports whether the line was consumed.
func routeStdoutLine(line string) bool {
	if strings.HasPrefix(line, flushSentinel) {
		id := strings.TrimSpace(strings.TrimPrefix(line, flushSentinel))
		stdoutCapture.Lock()
		ch := stdoutCapture.flushes[id]
		delete(stdoutCapture.flushes, id)
		stdoutCapture.Unlock()
		if ch != nil {
			close(ch)
		}
		return true
	}
	plain := colors.Never.Colorize(strings.TrimRight(line, "\r\n"))
	m := bannerRe.FindStringSubmatch(plain)
	if m == nil {
		return false
	}
	stdoutCapture.Lock()
	ops := stdoutCapture.listeners[m[2]]
	var target *Operation
	for _, op := range ops {
		if bannerVerb(op) == m[1] {
			target = op
			break
		}
	}
	stdoutCapture.Unlock()
	if target == nil {
		return false
	}
	target.handleEvent(eventFromAPI(apitype.EngineEvent{StdoutEvent: &apitype.StdoutEngineEvent{
		Message: plain + "\n",
		Color:   string(colors.Never),
	}}))
	return true
}

// bannerVerb is the header the backends print for an operation.
func bannerVerb(o *Operation) string {
	switch o.kind {
	case KindPreview:
		return "Previewing update"
	case KindUp:
		return "Updating"
	case KindRefresh:
		if o.dryRun {
			return "Previewing refresh"
		}
		return "Refreshing"
	case KindDestroy:
		if o.dryRun {
			return "Previewing destroy"
		}
		return "Destroying"
	}
	return ""
}

func (o *Operation) listenStdout(stackRef string) {
	startStdoutCapture()
	stdoutCapture.Lock()
	stdoutCapture.listeners[stackRef] = append(stdoutCapture.listeners[stackRef], o)
	stdoutCapture.Unlock()
}

func (o *Operation) unlistenStdout(stackRef string) {
	stdoutCapture.Lock()
	defer stdoutCapture.Unlock()
	ops := stdoutCapture.listeners[stackRef]
	for i, op := range ops {
		if op == o {
			ops = append(ops[:i], ops[i+1:]...)
			break
		}
	}
	if len(ops) == 0 {
		delete(stdoutCapture.listeners, stackRef)
	} else {
		stdoutCapture.listeners[stackRef] = ops
	}
}
