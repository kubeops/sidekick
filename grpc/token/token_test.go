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

package token

import (
	"testing"
	"time"
)

const key = "47094817-de7f-4120-baf4-c2b6e5ee5d46"

func validClaims() Claims {
	return Claims{
		SnapshotName: "snap",
		SidekickName: "sk",
		Namespace:    "demo",
		Digest:       RequestDigest([]byte("payload")),
		Nonce:        "nonce-1",
		ExpiresAt:    time.Now().Add(DefaultTTL).Unix(),
	}
}

func TestMintOpenRoundTrip(t *testing.T) {
	c := validClaims()
	tok, err := Mint(key, c)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := Open(key, tok)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.SnapshotName != c.SnapshotName || got.SidekickName != c.SidekickName ||
		got.Namespace != c.Namespace || got.Digest != c.Digest || got.Nonce != c.Nonce {
		t.Fatalf("claims round-trip mismatch: %+v vs %+v", got, c)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	tok, err := Mint(key, validClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := Open("another-key", tok); err == nil {
		t.Fatal("expected Open to fail with the wrong key")
	}
}

func TestMintIsNonDeterministic(t *testing.T) {
	// Per-request minting must use a fresh GCM IV so identical claims still
	// differ on the wire (and each carries its own nonce in practice).
	c := validClaims()
	a, _ := Mint(key, c)
	b, _ := Mint(key, c)
	if a == b {
		t.Fatal("expected distinct ciphertexts for repeated Mint calls")
	}
}

func TestEncryptDecryptString(t *testing.T) {
	const k = "snapshot-enc-key-0123456789abcdef"

	blob, err := EncryptString(k, "demo-snap")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	got, err := DecryptString(k, blob)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if got != "demo-snap" {
		t.Fatalf("round-trip = %q, want demo-snap", got)
	}

	// Deterministic: identical (key, plaintext) must yield identical ciphertext so
	// the value injected into the pod env is stable and does not drift the spec.
	blob2, _ := EncryptString(k, "demo-snap")
	if blob != blob2 {
		t.Fatal("EncryptString must be deterministic for a fixed key/plaintext")
	}

	// A different plaintext yields a different ciphertext.
	other, _ := EncryptString(k, "other-snap")
	if other == blob {
		t.Fatal("distinct plaintexts must produce distinct ciphertexts")
	}

	// Wrong key fails to decrypt.
	if _, err := DecryptString("another-key", blob); err == nil {
		t.Fatal("DecryptString with wrong key must fail")
	}
}

func TestVerify(t *testing.T) {
	now := time.Now()
	digest := RequestDigest([]byte("payload"))

	base := func() *Claims {
		c := validClaims()
		return &c
	}

	if err := base().Verify(now, "sk", "demo", digest); err != nil {
		t.Fatalf("valid claims should verify: %v", err)
	}

	expired := base()
	expired.ExpiresAt = now.Add(-time.Second).Unix()
	if err := expired.Verify(now, "sk", "demo", digest); err == nil {
		t.Fatal("expired token should fail Verify")
	}

	wrongID := base()
	if err := wrongID.Verify(now, "other-sk", "demo", digest); err == nil {
		t.Fatal("identity mismatch should fail Verify")
	}

	wrongDigest := base()
	if err := wrongDigest.Verify(now, "sk", "demo", RequestDigest([]byte("other"))); err == nil {
		t.Fatal("payload-binding mismatch should fail Verify")
	}

	noNonce := base()
	noNonce.Nonce = ""
	if err := noNonce.Verify(now, "sk", "demo", digest); err == nil {
		t.Fatal("missing nonce should fail Verify")
	}
}
