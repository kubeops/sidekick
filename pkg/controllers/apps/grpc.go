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
	"fmt"
	"log"
	"net"
	"time"

	sidekickgrpc "kubedb.dev/apimachinery/pkg/utils/grpc"
	"kubedb.dev/apimachinery/pkg/utils/grpc/sidekick/protogen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// CommandServer implements protogen.CommandServiceServer. Every request carries
// a JWT (inside the SnapShot envelope) that is verified against Secret before
// the request payload is accepted.
type CommandServer struct {
	protogen.UnimplementedCommandServiceServer

	// Secret is the shared HMAC secret used to verify the JWT in each request.
	// It must match the secret passed to GenerateToken on the client side.
	Secret string
}

// ExecuteCommand verifies the token shipped with the request and, on success,
// prints the data carried in the request.
func (s *CommandServer) ExecuteCommand(_ context.Context, req *protogen.CommandRequest) (*protogen.CommandResponse, error) {
	// The request data is a JSON-encoded SnapShot: {data, token}.
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(req.GetData(), &snap); err != nil {
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("failed to decode snapshot envelope: %v", err),
		}, nil
	}

	claims, err := VerifyToken(s.Secret, snap.Token)
	if err != nil {
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("token verification failed: %v", err),
		}, nil
	}

	// Token is valid: print the command and the data we were passed.
	log.Printf("[grpc] verified token for kind=%q name=%q", claims.Kind, claims.Name)
	log.Printf("[grpc] command: %s", req.GetCommand())
	log.Printf("[grpc] data: %s", string(snap.Data))

	return &protogen.CommandResponse{
		Status: "success",
		Output: snap.Data,
	}, nil
}

// RunGRPCServer starts a CommandService gRPC server on addr (e.g. ":9090") and
// blocks until the server stops or the listener fails. Tokens are verified
// against secret.
func RunGRPCServer(addr, secret string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	srv := grpc.NewServer()
	protogen.RegisterCommandServiceServer(srv, &CommandServer{Secret: secret})

	log.Printf("[grpc] CommandService listening on %s", addr)
	return srv.Serve(lis)
}

// SendCommand is a small client helper that mints a token for kind/name using
// secret, wraps data + token in a SnapShot envelope, and calls ExecuteCommand
// on the server at addr. It returns the server response.
func SendCommand(ctx context.Context, addr, secret, kind, name, command string, data []byte) (*protogen.CommandResponse, error) {
	token, err := GenerateToken(secret, kind, name)
	if err != nil {
		return nil, fmt.Errorf("failed to generate token: %w", err)
	}

	envelope, err := json.Marshal(sidekickgrpc.SnapShot{
		Data:  data,
		Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal snapshot envelope: %w", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	client := protogen.NewCommandServiceClient(conn)

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return client.ExecuteCommand(callCtx, &protogen.CommandRequest{
		Command: command,
		Data:    envelope,
	})
}
