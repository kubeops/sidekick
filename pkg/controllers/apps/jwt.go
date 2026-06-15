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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SidekickClaims is the payload carried inside a token. It identifies the
// Kubernetes object (Kind/Name) a token was minted for.
type SidekickClaims struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// IssuedAt is the unix timestamp (seconds) at which the token was created.
	IssuedAt int64 `json:"iat"`
	// ExpiresAt is the unix timestamp (seconds) after which the token is invalid.
	// A zero value means the token never expires.
	ExpiresAt int64 `json:"exp,omitempty"`
}

// jwtHeader is the fixed header for an HS256 signed token.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// DefaultTokenValidity is how long a generated token stays valid.
const DefaultTokenValidity = time.Hour

// b64 is the URL-safe, unpadded base64 encoding used by JWT.
var b64 = base64.RawURLEncoding

// GenerateToken creates a signed JWT (HS256) for the given kind/name using the
// provided secret. The returned token can later be checked with VerifyToken
// using the same secret.
func GenerateToken(secret, kind, name string) (string, error) {
	now := time.Now()
	claims := SidekickClaims{
		Kind:      kind,
		Name:      name,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(DefaultTokenValidity).Unix(),
	}
	return signToken(secret, claims)
}

func signToken(secret string, claims SidekickClaims) (string, error) {
	if secret == "" {
		return "", errors.New("jwt: signing secret must not be empty")
	}

	headerJSON, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("jwt: marshal header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("jwt: marshal claims: %w", err)
	}

	signingInput := b64.EncodeToString(headerJSON) + "." + b64.EncodeToString(claimsJSON)
	signature := sign(secret, signingInput)
	return signingInput + "." + signature, nil
}

// VerifyToken validates the signature and expiry of a token and returns the
// claims it carries. An error is returned if the token is malformed, the
// signature does not match the secret, or the token has expired.
func VerifyToken(secret, token string) (*SidekickClaims, error) {
	if secret == "" {
		return nil, errors.New("jwt: signing secret must not be empty")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: token must have three segments")
	}

	signingInput := parts[0] + "." + parts[1]
	expected := sign(secret, signingInput)
	// Constant-time compare to avoid timing attacks.
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return nil, errors.New("jwt: signature verification failed")
	}

	claimsJSON, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("jwt: decode claims: %w", err)
	}
	var claims SidekickClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("jwt: unmarshal claims: %w", err)
	}

	if claims.ExpiresAt != 0 && time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("jwt: token expired at %s", time.Unix(claims.ExpiresAt, 0))
	}

	return &claims, nil
}

// sign computes the base64url-encoded HMAC-SHA256 signature of input.
func sign(secret, input string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(input))
	return b64.EncodeToString(mac.Sum(nil))
}
