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
	"net"
	"time"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"
	sidekickgrpc "kubeops.dev/sidekick/grpc"
	"kubeops.dev/sidekick/grpc/protogen"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// grpcPort is the port the CommandService server listens on.
	grpcPort = "50051"
)

// Commands understood by the CommandService server.
const (
	// CommandUpdateSnapshot updates the snapshot carried in the request data.
	CommandUpdateSnapshot = "UpdateSnapshot"
)

// CommandServer implements protogen.CommandServiceServer. Every request carries
// an encrypted token (inside the SnapShot envelope) that is decrypted with
// Secret before the request payload is accepted.
type CommandServer struct {
	protogen.UnimplementedCommandServiceServer

	// KBClient talks to the Kubernetes API server for any work a command needs
	// to perform (e.g. fetching or creating objects).
	KBClient client.Client

	// Secret is the shared secret (the Sidekick UID) used to decrypt the token
	// in each request. It must match the secret passed to GenerateToken on the
	// client side.
	Secret string
}

// ExecuteCommand decrypts the token shipped with the request and, on success,
// prints the decrypted claims and the data carried in the request.
func (s *CommandServer) ExecuteCommand(_ context.Context, req *protogen.CommandRequest) (*protogen.CommandResponse, error) {
	// The request data is a JSON-encoded SnapShot: {data, token}.
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(req.GetData(), &snap); err != nil {
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("failed to decode snapshot envelope: %v", err),
		}, nil
	}

	var sidekick appsv1alpha1.Sidekick
	if err := s.KBClient.Get(context.TODO(), types.NamespacedName{Namespace: snap.NameSpace, Name: snap.SidekickName}, &sidekick); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("failed to get sidekick: %v", err),
		}, nil
	}
	secret := string(sidekick.UID)

	claims, err := DecryptToken(secret, snap.Token)
	if err != nil {
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("token decryption failed: %v", err),
		}, nil
	}

	// Token decrypted: dispatch on the requested command.
	klog.Infof("[grpc] decrypted token for sidekick %s is %q", sidekick.Name, claims.Name)
	klog.Infof("[grpc] command: %s", req.GetCommand())

	switch req.GetCommand() {
	case CommandUpdateSnapshot:
		err := s.UpdateSnapshot(claims.Name, sidekick.Namespace, snap.LogInfo)
		if err != nil {
			klog.Error(err)
			return getError(err)
		}
		// Print the snapshot data we were passed.
		klog.Infof("[grpc] UpdateSnapshot data: %+v", snap.LogInfo)
		return &protogen.CommandResponse{
			Status: "success",
		}, nil
	default:
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("unknown command: %q", req.GetCommand()),
		}, nil
	}
}

// RunGRPCServer starts a Snapshot Updater Service gRPC server on :50051 and blocks until
// the server stops or the listener fails. Tokens are decrypted against secret.
func RunGRPCServer(secret string, kbClient client.Client) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%v", grpcPort))
	if err != nil {
		return fmt.Errorf("failed to listen on :%s: %w", grpcPort, err)
	}

	srv := grpc.NewServer()
	protogen.RegisterCommandServiceServer(srv, &CommandServer{
		KBClient: kbClient,
		Secret:   secret,
	})

	klog.Infof("[grpc] Snapshot Updater Service listening on :%s", grpcPort)
	return srv.Serve(lis)
}

// SendCommand is a small client helper that mints a token for name using secret,
// wraps the log info + token in a SnapShot envelope, and calls ExecuteCommand on
// the server at addr. data is the JSON-encoded LogInfo payload. It returns the
// server response.
func SendCommand(ctx context.Context, addr, secret, name, command string, data []byte) (*protogen.CommandResponse, error) {
	token, err := GenerateToken(secret, name)
	if err != nil {
		return nil, fmt.Errorf("failed to generate token: %w", err)
	}

	var info sidekickgrpc.LogInfo
	if len(data) > 0 {
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("failed to decode log info: %w", err)
		}
	}

	envelope, err := json.Marshal(sidekickgrpc.SnapShot{
		SidekickName: name,
		LogInfo:      info,
		Token:        token,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal snapshot envelope: %w", err)
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	cmdClient := protogen.NewCommandServiceClient(conn)

	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return cmdClient.ExecuteCommand(callCtx, &protogen.CommandRequest{
		Command: command,
		Data:    envelope,
	})
}

func getError(err error) (*protogen.CommandResponse, error) {
	return &protogen.CommandResponse{
		Status: "error",
		Error:  err.Error(),
	}, nil
}
