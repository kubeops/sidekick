/*
Copyright AppsCode Inc. and Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apps

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// SidekickClaims is the payload that gets encrypted into a token. It identifies
// the Kubernetes object (Kind/Name) a token was minted for. There is no expiry.
type SidekickClaims struct {
	Name string `json:"name"`
}

// b64 is the URL-safe, unpadded base64 encoding used to wrap the ciphertext.
var b64 = base64.RawURLEncoding

// deriveKey turns an arbitrary-length secret into a fixed 32-byte AES-256 key.
func deriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// GenerateToken encrypts the given kind/name with the provided secret using
// AES-256-GCM and returns the result as a base64url string. The same secret is
// required to recover the claims via DecryptToken.
//
// The token is deterministic: the same (secret, name) always produces the
// identical token. This is required because the operator re-mints the token on
// every reconcile and writes it into the pod's TOKEN env; a non-deterministic
// token would change the desired pod spec each reconcile and trigger a forbidden
// update of the already-running pod. Determinism is achieved with a synthetic,
// plaintext-derived nonce (SIV-style) instead of a random one.
func GenerateToken(secret, name string) (string, error) {
	if secret == "" {
		return "", errors.New("token: secret must not be empty")
	}

	plaintext, err := json.Marshal(SidekickClaims{Name: name})
	if err != nil {
		return "", fmt.Errorf("token: marshal claims: %w", err)
	}

	gcm, err := newGCM(secret)
	if err != nil {
		return "", err
	}

	// Deterministic nonce derived from the plaintext, prepended to the ciphertext.
	// Identical claims under the same secret intentionally map to the identical
	// nonce (and thus the identical token); distinct claims get distinct nonces,
	// so GCM nonce reuse across different plaintexts never happens.
	nonce := deterministicNonce(secret, plaintext, gcm.NonceSize())

	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return b64.EncodeToString(sealed), nil
}

// deterministicNonce derives a GCM nonce from the secret and plaintext via
// HMAC-SHA256, making GenerateToken deterministic for identical inputs.
func deterministicNonce(secret string, plaintext []byte, size int) []byte {
	// Use a nonce-derivation key distinct from the AES key so the nonce HMAC
	// never reuses the encryption key material.
	keySum := sha256.Sum256([]byte("sidekick-token-nonce:" + secret))
	mac := hmac.New(sha256.New, keySum[:])
	mac.Write(plaintext)
	return mac.Sum(nil)[:size]
}

// DecryptToken decrypts a token produced by GenerateToken using secret and
// returns the claims it carries. GCM authenticates the ciphertext, so a wrong
// secret or any tampering fails decryption.
func DecryptToken(secret, token string) (*SidekickClaims, error) {
	if secret == "" {
		return nil, errors.New("token: secret must not be empty")
	}

	sealed, err := b64.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("token: decode: %w", err)
	}

	gcm, err := newGCM(secret)
	if err != nil {
		return nil, err
	}

	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("token: ciphertext too short")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("token: decryption failed: %w", err)
	}

	var claims SidekickClaims
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		return nil, fmt.Errorf("token: unmarshal claims: %w", err)
	}
	return &claims, nil
}

// newGCM builds an AES-256-GCM AEAD from the secret.
func newGCM(secret string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(secret))
	if err != nil {
		return nil, fmt.Errorf("token: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("token: new gcm: %w", err)
	}
	return gcm, nil
}
