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

// Package token implements the short-lived, request-bound authentication tokens
// exchanged between an archiver (client, in a spoke cluster) and the sidekick
// gRPC server (in the controller cluster). A token is the AES-256-GCM encryption
// of a small claims object, keyed by a shared per-Sidekick signing key.
//
// The token is NOT a long-lived bearer credential: it carries an expiry, a
// random single-use nonce (for replay rejection), and a digest binding it to the
// exact request payload it was minted for. A captured token therefore cannot be
// replayed, reused after its short TTL, or used with a tampered payload.
package token

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultTTL is how long a freshly minted token stays valid. It only needs to
// cover the gRPC call plus a few retries, so it is intentionally short.
const DefaultTTL = 5 * time.Minute

// b64 is the URL-safe, unpadded base64 encoding used to wrap the ciphertext.
var b64 = base64.RawURLEncoding

// Claims is the authenticated payload carried inside a token. Every field is
// covered by GCM authentication, so none of it can be altered without the key.
type Claims struct {
	// SnapshotName is the kubestash Snapshot the server should act on.
	SnapshotName string `json:"snap"`
	// SidekickName / Namespace identify the Sidekick (and thus the signing key)
	// the token was minted for; the server cross-checks them against the request.
	SidekickName string `json:"sk"`
	Namespace    string `json:"ns"`
	// Digest binds the token to the exact request payload (see RequestDigest).
	Digest string `json:"dig"`
	// Nonce is a random single-use value the server tracks to reject replays.
	Nonce string `json:"jti"`
	// ExpiresAt is the unix-seconds expiry; the server rejects tokens past it.
	ExpiresAt int64 `json:"exp"`
}

// NewNonce returns a random 128-bit nonce suitable for replay protection.
func NewNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("token: read nonce: %w", err)
	}
	return b64.EncodeToString(buf), nil
}

// RequestDigest computes a stable base64 SHA-256 over the request-identifying
// bytes. Both the minting client and the verifying server compute it the same
// way so the token can be cryptographically bound to a single request.
func RequestDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return b64.EncodeToString(sum[:])
}

// Mint encrypts claims with key using AES-256-GCM and returns a base64url token.
// A fresh random GCM nonce is used per call; determinism is neither needed nor
// wanted here because the token is minted per request by the client, not baked
// into any long-lived object.
func Mint(key string, c Claims) (string, error) {
	if key == "" {
		return "", errors.New("token: key must not be empty")
	}
	plaintext, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("token: marshal claims: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("token: read iv: %w", err)
	}
	sealed := gcm.Seal(iv, iv, plaintext, nil)
	return b64.EncodeToString(sealed), nil
}

// Open decrypts a token and returns its claims. GCM authenticates the ciphertext,
// so a wrong key or any tampering fails here. Open does NOT check expiry, nonce
// replay, or the payload binding — callers must do that (see Claims.Verify).
func Open(key, tok string) (*Claims, error) {
	if key == "" {
		return nil, errors.New("token: key must not be empty")
	}
	sealed, err := b64.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("token: decode: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("token: ciphertext too short")
	}
	iv, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("token: decryption failed: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(plaintext, &c); err != nil {
		return nil, fmt.Errorf("token: unmarshal claims: %w", err)
	}
	return &c, nil
}

// Verify checks the non-cryptographic parts of an already-decrypted token: that
// it has not expired and that its claimed identity and payload binding match the
// request the server actually received. Replay rejection is handled separately
// by the server (it needs to track nonces across requests).
func (c *Claims) Verify(now time.Time, sidekickName, namespace, digest string) error {
	if c.ExpiresAt == 0 || now.Unix() >= c.ExpiresAt {
		return errors.New("token: expired")
	}
	if c.SidekickName != sidekickName || c.Namespace != namespace {
		return errors.New("token: identity mismatch")
	}
	if c.Digest == "" || c.Digest != digest {
		return errors.New("token: payload binding mismatch")
	}
	if c.Nonce == "" {
		return errors.New("token: missing nonce")
	}
	return nil
}

// EncryptString encrypts plaintext with key using AES-256-GCM and a deterministic
// nonce derived from (key, plaintext). The output is therefore stable for a given
// (key, plaintext) pair. That determinism matters for a value injected into a
// pod's env: a random nonce would yield a different ciphertext on every reconcile,
// drifting the pod spec and triggering needless pod churn. It only reveals when
// the same plaintext is encrypted under the same key (acceptable for a single,
// fixed grant value per Sidekick); it is NOT a general-purpose encryptor for a
// stream of related secrets.
func EncryptString(key, plaintext string) (string, error) {
	if key == "" {
		return "", errors.New("token: key must not be empty")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	iv := deterministicNonce(key, plaintext, gcm.NonceSize())
	sealed := gcm.Seal(iv, iv, []byte(plaintext), nil)
	return b64.EncodeToString(sealed), nil
}

// DecryptString reverses EncryptString. GCM authenticates the ciphertext, so a
// wrong key or any tampering fails here.
func DecryptString(key, blob string) (string, error) {
	if key == "" {
		return "", errors.New("token: key must not be empty")
	}
	sealed, err := b64.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("token: decode: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("token: ciphertext too short")
	}
	iv, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("token: decryption failed: %w", err)
	}
	return string(plaintext), nil
}

// deterministicNonce derives a stable GCM nonce from key and plaintext via
// HMAC-SHA256, so identical inputs yield an identical nonce (and ciphertext).
func deterministicNonce(key, plaintext string, size int) []byte {
	mac := hmac.New(sha256.New, deriveKey(key))
	mac.Write([]byte(plaintext))
	return mac.Sum(nil)[:size]
}

// deriveKey turns an arbitrary-length signing key into a fixed 32-byte AES-256 key.
func deriveKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

// newGCM builds an AES-256-GCM AEAD from the signing key.
func newGCM(key string) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return nil, fmt.Errorf("token: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("token: new gcm: %w", err)
	}
	return gcm, nil
}
