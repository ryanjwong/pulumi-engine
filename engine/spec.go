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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/backend/diy"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// StackSpec fully describes a stack: where its state lives, how its secrets
// are encrypted, which project it belongs to and its configuration. Nothing in
// a StackSpec is read from process-global state (no ambient PULUMI_* variables,
// no current-workspace lookup); everything an operation needs is in the spec.
type StackSpec struct {
	// Name is the stack name: "dev", or a fully qualified reference such as
	// "org/project/dev" for backends that support organizations.
	Name string `json:"name"`

	// Project identifies the Pulumi project the stack belongs to.
	Project ProjectSpec `json:"project"`

	// Backend selects and configures the state backend.
	Backend BackendSpec `json:"backend"`

	// Secrets selects and configures the secrets provider used to encrypt
	// secret configuration and secret resource state.
	Secrets SecretsSpec `json:"secrets"`

	// Config is applied on top of any Pulumi.<stack>.yaml found in
	// Project.Dir. Keys without a namespace ("region") are scoped to the
	// project ("<project>:region").
	Config map[string]ConfigValue `json:"config,omitempty"`

	// Create creates the stack when it does not exist yet. When false, Open
	// returns StackNotFound for a missing stack.
	Create bool `json:"create,omitempty"`

	// Env is the environment given to every provider plugin and language
	// host an operation on this stack launches, overlaid on the process
	// environment (a key set here wins). It is where per-stack credentials,
	// PULUMI_HOME for the plugins' own use, NODE_PATH and program variables
	// go; it is never applied to this process, so concurrent operations with
	// different Env do not interfere. Options.Env adds to it per operation.
	//
	// Not covered: the library's own plugin resolution and downloads read
	// PULUMI_HOME (and GITHUB_TOKEN) from the process environment; see
	// docs/upstream.md.
	Env map[string]string `json:"env,omitempty"`
}

// ProjectSpec identifies the Pulumi project.
type ProjectSpec struct {
	// Name is the project name. It may be omitted when Dir contains a
	// Pulumi.yaml, in which case the name is read from there.
	Name string `json:"name,omitempty"`
	// Dir is the project root used as the working directory for operations
	// and as the location of Pulumi.<stack>.yaml. Optional; defaults to the
	// process working directory.
	Dir string `json:"dir,omitempty"`
	// Description is stored in stack tags on backends that support them.
	Description string `json:"description,omitempty"`
}

// BackendSpec selects the state backend by URL.
//
// DIY backends: "file://<path>", "file://~", "s3://bucket/prefix",
// "gs://bucket", "azblob://container". Cloud credentials for the object-store
// variants come from the usual SDK environment (that is a gocloud.dev
// property, not something this library can scope per stack).
//
// HTTP backends: "https://api.pulumi.com" or a self-hosted implementation.
type BackendSpec struct {
	URL string `json:"url"`
	// Token is the access token for HTTP backends. See the README for how it
	// reaches Pulumi's backend implementation and why.
	Token string `json:"token,omitempty"`
	// Insecure disables TLS verification for HTTP backends.
	Insecure bool `json:"insecure,omitempty"`
}

// SecretsSpec selects the secrets provider.
//
// Provider values: "passphrase" (default for DIY backends; requires Passphrase),
// "service" (default for HTTP backends), a cloud KMS URL ("awskms://...",
// "gcpkms://...", "azurekeyvault://...", "hashivault://..."), or "b64" (no
// encryption at all; only for tests).
//
// The empty passphrase is valid, as it is for the CLI when
// PULUMI_CONFIG_PASSPHRASE is set to "". Because Go cannot tell an empty
// string from an absent one, an empty passphrase must be confirmed with
// PassphraseSet; in JSON, the presence of the "passphrase" key is enough.
type SecretsSpec struct {
	Provider   string `json:"provider,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	// PassphraseSet confirms that Passphrase is intentionally empty. Set
	// automatically when the spec is decoded from JSON with a "passphrase"
	// key.
	PassphraseSet bool `json:"passphraseSet,omitempty"`
}

// UnmarshalJSON records whether the "passphrase" key was present so that an
// explicit empty passphrase is distinguishable from none.
func (s *SecretsSpec) UnmarshalJSON(data []byte) error {
	type plain SecretsSpec
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	if _, ok := keys["passphrase"]; ok {
		p.PassphraseSet = true
	}
	*s = SecretsSpec(p)
	return nil
}

// hasPassphrase reports whether a passphrase (possibly empty) was provided.
func (s SecretsSpec) hasPassphrase() bool { return s.Passphrase != "" || s.PassphraseSet }

// ConfigValue is one configuration value. Secret values are encrypted with
// the stack's secrets provider before they reach Pulumi.
type ConfigValue struct {
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
	// Object marks Value as a JSON-encoded structured value rather than a
	// plain string.
	Object bool `json:"object,omitempty"`
}

const (
	secretsPassphrase = "passphrase"
	secretsService    = "service"
	secretsB64        = "b64"
)

var cloudSecretsSchemes = []string{"awskms://", "gcpkms://", "azurekeyvault://", "hashivault://"}

func isCloudSecretsProvider(p string) bool {
	for _, s := range cloudSecretsSchemes {
		if strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

// isDIYBackend reports whether the backend URL selects the DIY (file/object
// store) backend rather than an HTTP backend.
func isDIYBackend(url string) bool {
	return diy.IsDIYBackendURL(url)
}

// validate checks the spec and fills in defaults. It returns InvalidSpec on
// failure.
func (s *StackSpec) validate() error {
	if s.Name == "" {
		return InvalidSpec{Field: "name", Message: "stack name is required"}
	}
	if _, err := tokens.ParseStackName(stackNameOnly(s.Name)); err != nil {
		return InvalidSpec{Field: "name", Message: err.Error()}
	}
	if s.Backend.URL == "" {
		return InvalidSpec{Field: "backend.url", Message: "backend URL is required"}
	}
	if !isDIYBackend(s.Backend.URL) && !strings.HasPrefix(s.Backend.URL, "https://") &&
		!strings.HasPrefix(s.Backend.URL, "http://") {
		return InvalidSpec{Field: "backend.url", Message: fmt.Sprintf(
			"unrecognised backend URL %q (expected file://, s3://, gs://, azblob:// or http(s)://)", s.Backend.URL)}
	}
	if s.Backend.Token != "" && isDIYBackend(s.Backend.URL) {
		return InvalidSpec{Field: "backend.token", Message: "a token is only meaningful for HTTP backends"}
	}

	if s.Project.Dir != "" {
		abs, err := filepath.Abs(s.Project.Dir)
		if err != nil {
			return InvalidSpec{Field: "project.dir", Message: err.Error()}
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return InvalidSpec{Field: "project.dir", Message: fmt.Sprintf("%q is not a directory", s.Project.Dir)}
		}
		s.Project.Dir = abs
	}
	if s.Project.Name == "" {
		if s.Project.Dir == "" {
			return InvalidSpec{Field: "project.name", Message: "project name is required (or set project.dir to a directory containing Pulumi.yaml)"}
		}
		proj, err := workspace.LoadProject(filepath.Join(s.Project.Dir, "Pulumi.yaml"))
		if err != nil {
			return InvalidSpec{Field: "project.name", Message: fmt.Sprintf("project name not set and %s could not be loaded: %v",
				filepath.Join(s.Project.Dir, "Pulumi.yaml"), err)}
		}
		s.Project.Name = proj.Name.String()
	}
	if _, err := tokens.ParseStackName(s.Project.Name); err != nil {
		return InvalidSpec{Field: "project.name", Message: fmt.Sprintf("invalid project name %q: %v", s.Project.Name, err)}
	}

	switch {
	case s.Secrets.Provider == "":
		if isDIYBackend(s.Backend.URL) {
			s.Secrets.Provider = secretsPassphrase
		} else {
			s.Secrets.Provider = secretsService
		}
	case s.Secrets.Provider == secretsPassphrase, s.Secrets.Provider == secretsService, s.Secrets.Provider == secretsB64,
		isCloudSecretsProvider(s.Secrets.Provider):
	default:
		return InvalidSpec{Field: "secrets.provider", Message: fmt.Sprintf("unknown secrets provider %q", s.Secrets.Provider)}
	}
	if s.Secrets.Provider == secretsPassphrase && !s.Secrets.hasPassphrase() {
		return InvalidSpec{Field: "secrets.passphrase", Message: "the passphrase secrets provider requires a passphrase (set passphraseSet for an intentionally empty one)"}
	}
	if s.Secrets.Provider == secretsService && isDIYBackend(s.Backend.URL) {
		return InvalidSpec{Field: "secrets.provider", Message: "the service secrets provider requires an HTTP backend"}
	}
	if s.Secrets.Provider != secretsPassphrase && s.Secrets.hasPassphrase() {
		return InvalidSpec{Field: "secrets.passphrase", Message: "passphrase is only used by the passphrase provider"}
	}

	for k := range s.Env {
		if k == "" || strings.Contains(k, "=") {
			return InvalidSpec{Field: "env", Message: fmt.Sprintf("invalid environment variable name %q", k)}
		}
	}

	for k, v := range s.Config {
		if k == "" {
			return InvalidSpec{Field: "config", Message: "empty config key"}
		}
		if v.Object && !looksLikeJSON(v.Value) {
			return InvalidSpec{Field: "config." + k, Message: "object values must be JSON encoded"}
		}
	}
	return nil
}

// stackNameOnly strips an "org/project/" prefix from a fully qualified name.
func stackNameOnly(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") || strings.HasPrefix(t, "\"") ||
		t == "true" || t == "false" || t == "null" || (len(t) > 0 && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9')))
}
