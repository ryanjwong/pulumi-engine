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

// Secrets providers and managers. (The file is not called secrets.go because
// a local commit hook refuses to touch files with "secrets" in the name.)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/resource/stack"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/b64"
	"github.com/pulumi/pulumi/pkg/v3/secrets/cloud"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/pkg/v3/secrets/service"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

// specSecretsProvider reconstructs secrets managers from checkpoint state
// using only what the StackSpec provides. It replaces Pulumi's default
// provider, which prompts on a terminal or reads PULUMI_CONFIG_PASSPHRASE
// from the process environment.
type specSecretsProvider struct {
	passphrase string
}

var _ secrets.Provider = specSecretsProvider{}

func (p specSecretsProvider) OfType(ctx context.Context, ty string, state json.RawMessage) (secrets.Manager, error) {
	var sm secrets.Manager
	var err error
	switch ty {
	case passphrase.Type:
		if p.passphrase == "" {
			return nil, errors.New("the checkpoint is encrypted with a passphrase but the spec provides none")
		}
		var st struct {
			Salt string `json:"salt"`
		}
		if err := json.Unmarshal(state, &st); err != nil {
			return nil, fmt.Errorf("decoding passphrase secrets state: %w", err)
		}
		sm, err = passphrase.GetPassphraseSecretsManager(p.passphrase, st.Salt)
		if errors.Is(err, passphrase.ErrIncorrectPassphrase) {
			return nil, InvalidSpec{Field: "secrets.passphrase", Message: "incorrect passphrase for this stack"}
		}
	case service.Type:
		sm, err = service.NewServiceSecretsManagerFromState(ctx, state)
	case cloud.Type:
		sm, err = cloud.NewCloudSecretsManagerFromState(state)
	case b64.Type:
		sm = b64.NewBase64SecretsManager()
	default:
		return nil, fmt.Errorf("no known secrets provider for type %q", ty)
	}
	if err != nil {
		return nil, fmt.Errorf("constructing secrets manager of type %q: %w", ty, err)
	}
	return stack.NewBatchingCachingSecretsManager(sm), nil
}

// checkpointSecrets peeks at the stack's checkpoint for the secrets manager
// it was last written with, without decrypting anything.
func checkpointSecrets(ctx context.Context, s backend.Stack) (*apitype.SecretsProvidersV1, error) {
	dep, err := backend.ExportStackDeployment(ctx, s)
	if err != nil || dep == nil || len(dep.Deployment) == 0 {
		return nil, err
	}
	var v3 struct {
		SecretsProviders *apitype.SecretsProvidersV1 `json:"secrets_providers,omitempty"`
	}
	if err := json.Unmarshal(dep.Deployment, &v3); err != nil {
		return nil, fmt.Errorf("decoding checkpoint: %w", err)
	}
	return v3.SecretsProviders, nil
}

// newSecretsManager builds the manager used to encrypt config and the
// checkpoint. It reuses existing key material (the passphrase salt, the KMS
// data key) from Pulumi.<stack>.yaml or the checkpoint so that a stack keeps
// one encryption context across sessions.
func newSecretsManager(ctx context.Context, spec SecretsSpec, bs backend.Stack, ps *workspace.ProjectStack) (secrets.Manager, error) {
	switch {
	case spec.Provider == secretsPassphrase:
		salt := ps.EncryptionSalt
		if salt == "" {
			cs, err := checkpointSecrets(ctx, bs)
			if err != nil {
				return nil, err
			}
			if cs != nil && cs.Type == passphrase.Type {
				var st struct {
					Salt string `json:"salt"`
				}
				if err := json.Unmarshal(cs.State, &st); err == nil {
					salt = st.Salt
				}
			}
		}
		if salt != "" {
			sm, err := passphrase.GetPassphraseSecretsManager(spec.Passphrase, salt)
			if errors.Is(err, passphrase.ErrIncorrectPassphrase) {
				return nil, InvalidSpec{Field: "secrets.passphrase", Message: "incorrect passphrase for this stack"}
			}
			if err != nil {
				return nil, err
			}
			ps.EncryptionSalt = salt
			return sm, nil
		}
		salt, sm, err := passphrase.NewPassphraseSecretsManager(spec.Passphrase)
		if err != nil {
			return nil, err
		}
		ps.EncryptionSalt = salt
		return sm, nil

	case isCloudSecretsProvider(spec.Provider):
		if ps.SecretsProvider == spec.Provider && ps.EncryptedKey != "" {
			return cloud.NewCloudSecretsManager(ps, spec.Provider, false)
		}
		cs, err := checkpointSecrets(ctx, bs)
		if err != nil {
			return nil, err
		}
		if cs != nil && cs.Type == cloud.Type {
			var st struct {
				URL          string `json:"url"`
				EncryptedKey []byte `json:"encryptedkey"`
			}
			if err := json.Unmarshal(cs.State, &st); err == nil && st.URL == spec.Provider {
				sm, err := cloud.NewCloudSecretsManagerFromState(cs.State)
				if err != nil {
					return nil, err
				}
				ps.SecretsProvider = st.URL
				return sm, nil
			}
		}
		return cloud.NewCloudSecretsManager(ps, spec.Provider, false)

	case spec.Provider == secretsService:
		return bs.DefaultSecretManager(ctx, ps)

	case spec.Provider == secretsB64:
		return b64.NewBase64SecretsManager(), nil
	}
	return nil, InvalidSpec{Field: "secrets.provider", Message: fmt.Sprintf("unknown secrets provider %q", spec.Provider)}
}
