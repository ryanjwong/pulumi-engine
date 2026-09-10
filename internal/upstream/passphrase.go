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

// Copied from pulumi/pulumi (Apache-2.0, Pulumi Corporation).
//
// upstream: github.com/pulumi/pulumi/pkg/v3@v3.237.0 secrets/passphrase/manager.go sha256=247fe23367c30ec749591d0d720b0851e87d36fd7b2fc03b4ce512e5c57865f2
// upstream-reason: passphrase.GetPassphraseSecretsManager caches managers
//   process-wide by encryption state (the salt) and returns the cached one
//   without looking at the passphrase, because the CLI only ever holds one
//   passphrase per process. A library opening the same stack for different
//   callers must verify the passphrase itself before consulting that cache;
//   the verification (state parsing and the "pulumi" probe) is unexported.
// upstream-delete-when: the passphrase cache is keyed by passphrase as well
//   as state, or the verification is exported.

package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
)

// VerifyPassphrase checks phrase against a passphrase secrets state
// ("v1:<salt>:<ciphertext>") and returns passphrase.ErrIncorrectPassphrase
// when it does not decrypt it.
func VerifyPassphrase(phrase, state string) error {
	_, err := symmetricCrypterFromPhraseAndState(phrase, state)
	return err
}

// upstream-begin: secrets/passphrase/manager.go (symmetricCrypterFromPhraseAndState, indexN)

// given a passphrase and an encryption state, construct a Crypter from it. Our encryption
// state value is a version tag followed by version specific state information. Presently, we only have one version
// we support (`v1`) which is AES-256-GCM using a key derived from a passphrase using 1,000,000 iterations of PDKDF2
// using SHA256.
func symmetricCrypterFromPhraseAndState(phrase string, state string) (config.Crypter, error) {
	splits := strings.SplitN(state, ":", 3)
	if len(splits) != 3 {
		return nil, errors.New("malformed state value")
	}

	if splits[0] != "v1" {
		return nil, errors.New("unknown state version")
	}

	salt, err := base64.StdEncoding.DecodeString(splits[1])
	if err != nil {
		return nil, err
	}

	decrypter := config.NewSymmetricCrypterFromPassphrase(phrase, salt)
	// symmetricCrypter does not use ctx, safe to pass context.Background()
	ignoredCtx := context.Background()
	decrypted, err := decrypter.DecryptValue(ignoredCtx, state[indexN(state, ":", 2)+1:])
	if err != nil || decrypted != "pulumi" {
		logging.V(7).Infof("incorrect passphrase: %v", err)
		return nil, passphrase.ErrIncorrectPassphrase
	}

	return decrypter, nil
}

func indexN(s string, substr string, n int) int {
	contract.Requiref(n > 0, "n", "must be greater than 0")
	scratch := s

	for i := n; i > 0; i-- {
		idx := strings.Index(scratch, substr)
		if i == -1 {
			return -1
		}

		scratch = scratch[idx+1:]
	}

	return len(s) - (len(scratch) + len(substr))
}

// upstream-end: secrets/passphrase/manager.go
