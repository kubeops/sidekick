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

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"
	sidekickgrpc "kubeops.dev/sidekick/grpc"
	"kubeops.dev/sidekick/grpc/protogen"

	"google.golang.org/grpc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// startTestServer spins up a CommandService server backed by a fake client
// seeded with objs on a random local port, and returns its address plus a stop
// function. The server decrypts each request token with the looked-up Sidekick's
// UID, so seeded Sidekicks must carry the matching UID.
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
	protogen.RegisterCommandServiceServer(srv, &CommandServer{KBClient: cl})

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), srv.Stop
}

func sidekickWithUID(name, uid string) *appsv1alpha1.Sidekick {
	return &appsv1alpha1.Sidekick{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
	}
}

func TestSendCommand_ValidToken(t *testing.T) {
	const secret = "super-secret-signing-key"
	// The server decrypts with the Sidekick UID, so it must equal the signing secret.
	sk := sidekickWithUID("demo", secret)
	snap := &storageapi.Snapshot{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	addr, stop := startTestServer(t, sk, snap)
	defer stop()

	payload, err := json.Marshal(sidekickgrpc.LogInfo{Type: "success", LogLimit: 5})
	if err != nil {
		t.Fatalf("marshal log info: %v", err)
	}

	resp, err := SendCommand(context.Background(), addr, secret, "demo", CommandUpdateSnapshot, payload)
	if err != nil {
		t.Fatalf("SendCommand returned error: %v", err)
	}
	if resp.GetStatus() != "success" {
		t.Fatalf("expected status success, got %q (error=%q)", resp.GetStatus(), resp.GetError())
	}
}

func TestSendCommand_UnknownCommand(t *testing.T) {
	const secret = "super-secret-signing-key"
	sk := sidekickWithUID("demo", secret)
	addr, stop := startTestServer(t, sk)
	defer stop()

	resp, err := SendCommand(context.Background(), addr, secret, "demo", "bogus", nil)
	if err != nil {
		t.Fatalf("SendCommand returned transport error: %v", err)
	}
	if resp.GetStatus() != "error" {
		t.Fatalf("expected status error for unknown command, got %q", resp.GetStatus())
	}
}

func TestSendCommand_WrongSecret(t *testing.T) {
	// Server-side Sidekick UID differs from the secret the client signs with.
	sk := sidekickWithUID("demo", "server-secret")
	addr, stop := startTestServer(t, sk)
	defer stop()

	resp, err := SendCommand(context.Background(), addr, "client-secret", "demo", CommandUpdateSnapshot, nil)
	if err != nil {
		t.Fatalf("SendCommand returned transport error: %v", err)
	}
	if resp.GetStatus() != "error" {
		t.Fatalf("expected status error for bad token, got %q", resp.GetStatus())
	}
}

func TestSendCommand_UnknownSidekick(t *testing.T) {
	// No Sidekick seeded -> the server's lookup fails -> error response.
	addr, stop := startTestServer(t)
	defer stop()

	resp, err := SendCommand(context.Background(), addr, "secret", "demo", CommandUpdateSnapshot, nil)
	if err != nil {
		t.Fatalf("SendCommand returned transport error: %v", err)
	}
	if resp.GetStatus() != "error" {
		t.Fatalf("expected status error for missing sidekick, got %q", resp.GetStatus())
	}
}

func TestGenerateAndDecryptToken(t *testing.T) {
	const secret = "47094817-de7f-4120-baf4-c2b6e5ee5d46"

	token, err := GenerateToken(secret, "my-sidekick")
	if err != nil {
		t.Fatalf("GenerateToken error: %v", err)
	}

	claims, err := DecryptToken(secret, token)
	if err != nil {
		t.Fatalf("DecryptToken error: %v", err)
	}
	if claims.Name != "my-sidekick" {
		t.Fatalf("unexpected claims: %+v", claims)
	}

	// A wrong secret must fail to decrypt.
	if _, err := DecryptToken("other-secret", token); err == nil {
		t.Fatal("expected decryption to fail with wrong secret")
	}

	// Token generation must be deterministic: the same (secret, name) must
	// always yield the identical token, otherwise the operator re-mints it on
	// every reconcile and triggers a forbidden update of the running pod.
	token2, err := GenerateToken(secret, "my-sidekick")
	if err != nil {
		t.Fatalf("GenerateToken (second) error: %v", err)
	}
	if token != token2 {
		t.Fatalf("token not deterministic:\n  first=%q\n second=%q", token, token2)
	}

	// Distinct claims must still produce distinct tokens (distinct nonces).
	other, err := GenerateToken(secret, "different-sidekick")
	if err != nil {
		t.Fatalf("GenerateToken (other name) error: %v", err)
	}
	if other == token {
		t.Fatal("distinct claims produced identical tokens")
	}
}
