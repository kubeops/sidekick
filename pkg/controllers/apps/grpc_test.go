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
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"
	sidekickgrpc "kubeops.dev/sidekick/grpc"
	"kubeops.dev/sidekick/grpc/protogen"
	"kubeops.dev/sidekick/grpc/token"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace   = "demo"
	testSidekick    = "demo"
	testSnapshot    = "demo-snap"
	testKey         = "test-signing-key-0123456789abcdef"
	testSnapshotKey = "test-snapshot-key-0123456789abcd"
)

// startTestServer spins up a CommandService server backed by a fake client seeded
// with objs on a random local port, and returns its address plus a stop function.
// The server verifies each request token against the per-Sidekick signing-key
// Secret, so seed signingSecret(...) for requests that should authenticate.
func startTestServer(t *testing.T, objs ...client.Object) (addr string, stop func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&storageapi.Snapshot{}).
		WithObjects(objs...).
		Build()

	srv := grpc.NewServer()
	protogen.RegisterCommandServiceServer(srv, &CommandServer{KBClient: cl, replay: newReplayCache()})

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), srv.Stop
}

// signingSecret seeds both per-Sidekick keys the server needs: the signing key
// (token auth) and the snapshot key (snapshot-grant authorization).
func signingSecret(namespace, sidekickName, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signingSecretName(sidekickName), Namespace: namespace},
		Data: map[string][]byte{
			signingSecretKey:  []byte(key),
			snapshotSecretKey: []byte(testSnapshotKey),
		},
	}
}

func testSnapshotObj() *storageapi.Snapshot {
	return &storageapi.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: testSnapshot, Namespace: testNamespace}}
}

func testSidekickObj() *appsv1alpha1.Sidekick {
	return &appsv1alpha1.Sidekick{ObjectMeta: metav1.ObjectMeta{Name: testSidekick, Namespace: testNamespace}}
}

// mintEnvelope builds a SnapShot envelope with a token minted for it. mutate can
// tweak the claims (e.g. expire them) before minting to exercise rejection paths.
func mintEnvelope(t *testing.T, key, snapshotName string, info sidekickgrpc.LogInfo, mutate func(*token.Claims)) []byte {
	t.Helper()
	snap := sidekickgrpc.SnapShot{
		SidekickName: testSidekick,
		NameSpace:    testNamespace,
		LogInfo:      info,
	}
	// Attach the operator-issued grant authorizing this exact snapshot. It is
	// independent of the signed token (the binding payload excludes it).
	grant, err := token.EncryptString(testSnapshotKey, snapshotName)
	if err != nil {
		t.Fatalf("encrypt snapshot grant: %v", err)
	}
	snap.SnapshotToken = grant

	payload, err := snap.BindingPayload()
	if err != nil {
		t.Fatalf("binding payload: %v", err)
	}
	nonce, err := token.NewNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	claims := token.Claims{
		SnapshotName: snapshotName,
		SidekickName: testSidekick,
		Namespace:    testNamespace,
		Digest:       token.RequestDigest(payload),
		Nonce:        nonce,
		ExpiresAt:    time.Now().Add(token.DefaultTTL).Unix(),
	}
	if mutate != nil {
		mutate(&claims)
	}
	tok, err := token.Mint(key, claims)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	snap.Token = tok
	envelope, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return envelope
}

func execute(t *testing.T, addr, command string, data []byte) *protogen.CommandResponse {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := protogen.NewCommandServiceClient(conn).ExecuteCommand(ctx, &protogen.CommandRequest{Command: command, Data: data})
	if err != nil {
		t.Fatalf("ExecuteCommand transport error: %v", err)
	}
	return resp
}

func TestExecuteCommand_ValidToken(t *testing.T) {
	addr, stop := startTestServer(t, testSidekickObj(), testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{Type: "success", LogLimit: 5}, nil)
	resp := execute(t, addr, CommandUpdateSnapshot, env)
	if resp.GetStatus() != "success" {
		t.Fatalf("expected success, got %q (error=%q)", resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_UnknownCommand(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{}, nil)
	resp := execute(t, addr, "bogus", env)
	if resp.GetStatus() != "error" {
		t.Fatalf("expected error for unknown command, got %q", resp.GetStatus())
	}
}

func TestExecuteCommand_WrongKey(t *testing.T) {
	// Server stores testKey; client signs with a different key -> token.Open fails.
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	env := mintEnvelope(t, "a-different-key", testSnapshot, sidekickgrpc.LogInfo{}, nil)
	resp := execute(t, addr, CommandUpdateSnapshot, env)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_MissingSecret(t *testing.T) {
	// No signing-key Secret seeded -> key lookup fails -> generic unauthenticated.
	addr, stop := startTestServer(t, testSnapshotObj())
	defer stop()

	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{}, nil)
	resp := execute(t, addr, CommandUpdateSnapshot, env)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_Expired(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{}, func(c *token.Claims) {
		c.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	})
	resp := execute(t, addr, CommandUpdateSnapshot, env)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q for expired token, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_PayloadTamper(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	// Mint a token bound to one payload, then alter the payload on the wire.
	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{Type: "success", LogLimit: 5}, nil)
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(env, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	snap.LogInfo.LogLimit = 999 // tamper: digest no longer matches
	tampered, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := execute(t, addr, CommandUpdateSnapshot, tampered)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q for tampered payload, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_SnapshotNotAuthorized(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	// A fully valid token claiming testSnapshot, but the grant authorizes a
	// DIFFERENT snapshot. The server must refuse: the archiver cannot forge a
	// grant for a snapshot it was not authorized for.
	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{Type: "success", LogLimit: 5}, nil)
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(env, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	otherGrant, err := token.EncryptString(testSnapshotKey, "someone-elses-snap")
	if err != nil {
		t.Fatalf("encrypt grant: %v", err)
	}
	snap.SnapshotToken = otherGrant
	swapped, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := execute(t, addr, CommandUpdateSnapshot, swapped)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q for unauthorized snapshot, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_MissingGrant(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	// Strip the grant: with no operator authorization the server cannot confirm
	// which snapshot is allowed, so it rejects.
	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{Type: "success", LogLimit: 5}, nil)
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(env, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	snap.SnapshotToken = ""
	stripped, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp := execute(t, addr, CommandUpdateSnapshot, stripped)
	if resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("expected %q for missing grant, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}

func TestExecuteCommand_Replay(t *testing.T) {
	addr, stop := startTestServer(t, testSnapshotObj(), signingSecret(testNamespace, testSidekick, testKey))
	defer stop()

	// The exact same envelope (same nonce) twice: first accepted, second rejected.
	env := mintEnvelope(t, testKey, testSnapshot, sidekickgrpc.LogInfo{Type: "success", LogLimit: 5}, nil)

	if resp := execute(t, addr, CommandUpdateSnapshot, env); resp.GetStatus() != "success" {
		t.Fatalf("first call should succeed, got status=%q error=%q", resp.GetStatus(), resp.GetError())
	}
	if resp := execute(t, addr, CommandUpdateSnapshot, env); resp.GetStatus() != "error" || resp.GetError() != errUnauthenticated {
		t.Fatalf("replayed call should be rejected with %q, got status=%q error=%q", errUnauthenticated, resp.GetStatus(), resp.GetError())
	}
}
