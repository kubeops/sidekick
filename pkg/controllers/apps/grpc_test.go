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
	"net"
	"testing"

	"kubeops.dev/sidekick/grpc/protogen"

	"google.golang.org/grpc"
)

// startTestServer spins up a CommandService server on a random local port and
// returns its address plus a stop function.
func startTestServer(t *testing.T, secret string) (addr string, stop func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	srv := grpc.NewServer()
	protogen.RegisterCommandServiceServer(srv, &CommandServer{Secret: secret})

	go func() { _ = srv.Serve(lis) }()

	return lis.Addr().String(), srv.Stop
}

func TestSendCommand_ValidToken(t *testing.T) {
	const secret = "super-secret-signing-key"
	addr, stop := startTestServer(t, secret)
	defer stop()

	payload := []byte(`{"seqno": 42}`)
	resp, err := SendCommand(context.Background(), addr, secret, "demo", CommandUpdateSnapshot, payload)
	if err != nil {
		t.Fatalf("SendCommand returned error: %v", err)
	}
	if resp.GetStatus() != "success" {
		t.Fatalf("expected status success, got %q (error=%q)", resp.GetStatus(), resp.GetError())
	}
	if string(resp.GetOutput()) != string(payload) {
		t.Fatalf("expected output %q, got %q", payload, resp.GetOutput())
	}
}

func TestSendCommand_UnknownCommand(t *testing.T) {
	const secret = "super-secret-signing-key"
	addr, stop := startTestServer(t, secret)
	defer stop()

	resp, err := SendCommand(context.Background(), addr, secret, "demo", "bogus", []byte("x"))
	if err != nil {
		t.Fatalf("SendCommand returned transport error: %v", err)
	}
	if resp.GetStatus() != "error" {
		t.Fatalf("expected status error for unknown command, got %q", resp.GetStatus())
	}
}

func TestSendCommand_WrongSecret(t *testing.T) {
	addr, stop := startTestServer(t, "server-secret")
	defer stop()

	// Client signs with a different secret -> server must reject the token.
	resp, err := SendCommand(context.Background(), addr, "client-secret", "demo", CommandUpdateSnapshot, []byte("x"))
	if err != nil {
		t.Fatalf("SendCommand returned transport error: %v", err)
	}
	if resp.GetStatus() != "error" {
		t.Fatalf("expected status error for bad token, got %q", resp.GetStatus())
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
}
