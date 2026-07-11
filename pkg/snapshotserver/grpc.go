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

package snapshotserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	sidekickgrpc "kubeops.dev/sidekick/grpc"
	"kubeops.dev/sidekick/grpc/protogen"
	"kubeops.dev/sidekick/grpc/token"

	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// grpcPort is the port the CommandService server listens on.
	grpcPort = "50051"

	// maxRecvMsgBytes caps the size of an incoming gRPC message. The snapshot
	// envelope is tiny; this just bounds memory a hostile peer can force us to
	// allocate per request.
	maxRecvMsgBytes = 64 * 1024

	// requestTimeout bounds how long a single command may run, including the
	// Kubernetes API calls it makes.
	requestTimeout = 15 * time.Second
)

// Commands understood by the CommandService server.
const (
	// CommandUpdateSnapshot updates the snapshot carried in the request data.
	CommandUpdateSnapshot = "UpdateSnapshot"
	// CommandGetSnapshot returns the authorized Snapshot, JSON-encoded in the
	// response Output. The client gets back only the Snapshot named in its token
	// (claims.SnapshotName), never an arbitrary one.
	CommandGetSnapshot = "GetSnapshot"
)

// errUnauthenticated is the single, deliberately vague error returned for every
// authentication/authorization failure. Returning the same message for "unknown
// sidekick", "bad token", "expired", "replayed", etc. denies an attacker an
// oracle for probing which Sidekicks exist or which tokens are valid. Details
// are logged server-side only.
const errUnauthenticated = "unauthenticated"

// CommandServer implements protogen.CommandServiceServer. Every request carries
// a short-lived, request-bound token (inside the SnapShot envelope) that is
// verified against the per-Sidekick signing key before the payload is accepted.
type CommandServer struct {
	protogen.UnimplementedCommandServiceServer

	// KBClient talks to the Kubernetes API server for any work a command needs
	// to perform (e.g. reading the signing-key Secret, patching Snapshot status).
	KBClient client.Client

	// replay rejects tokens whose nonce has already been seen.
	replay *replayCache
}

// ExecuteCommand authenticates the request token and, on success, dispatches the
// requested command. All auth failures return errUnauthenticated.
func (s *CommandServer) ExecuteCommand(ctx context.Context, req *protogen.CommandRequest) (*protogen.CommandResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	// The request data is a JSON-encoded SnapShot envelope.
	var snap sidekickgrpc.SnapShot
	if err := json.Unmarshal(req.GetData(), &snap); err != nil {
		klog.V(3).Infof("[grpc] bad envelope: %v", err)
		return authError(), nil
	}

	claims, err := s.authenticate(ctx, &snap)
	if err != nil {
		klog.V(3).Infof("[grpc] auth rejected for sidekick %q/%q: %v", snap.NameSpace, snap.SidekickName, err)
		return authError(), nil
	}

	klog.V(4).Infof("[grpc] authenticated request for sidekick %s/%s, command %s", snap.NameSpace, snap.SidekickName, req.GetCommand())

	switch req.GetCommand() {
	case CommandUpdateSnapshot:
		if err := s.UpdateSnapshot(ctx, claims.SnapshotName, snap.NameSpace, snap.LogInfo); err != nil {
			klog.Errorf("[grpc] UpdateSnapshot failed: %v", err)
			return getError(err), nil
		}
		return &protogen.CommandResponse{Status: "success"}, nil
	case CommandGetSnapshot:
		data, err := s.GetSnapshot(ctx, claims.SnapshotName, snap.NameSpace)
		if err != nil {
			klog.Errorf("[grpc] GetSnapshot failed: %v", err)
			return getError(err), nil
		}
		return &protogen.CommandResponse{Status: "success", Output: data}, nil
	default:
		return &protogen.CommandResponse{
			Status: "error",
			Error:  fmt.Sprintf("unknown command: %q", req.GetCommand()),
		}, nil
	}
}

// authenticate verifies the token shipped with the request and authorizes its
// target Snapshot. It loads the per-Sidekick signing and snapshot keys, decrypts
// the token, checks expiry + identity + payload binding, enforces the operator-
// issued snapshot grant, and rejects replays. It returns the validated claims.
func (s *CommandServer) authenticate(ctx context.Context, snap *sidekickgrpc.SnapShot) (*token.Claims, error) {
	signingKey, snapshotKey, err := s.grpcKeys(ctx, snap.NameSpace, snap.SidekickName)
	if err != nil {
		return nil, fmt.Errorf("load grpc keys: %w", err)
	}

	claims, err := token.Open(signingKey, snap.Token)
	if err != nil {
		return nil, err
	}

	digest, err := bindingDigest(snap)
	if err != nil {
		return nil, fmt.Errorf("compute digest: %w", err)
	}
	if err := claims.Verify(time.Now(), snap.SidekickName, snap.NameSpace, digest); err != nil {
		return nil, err
	}

	// Authorize the target Snapshot. The operator encrypted the one Snapshot name
	// this Sidekick may update into snap.SnapshotToken; decrypt it with the
	// per-Sidekick snapshot key and require the token's claimed SnapshotName to
	// match. A compromised archiver can put any name in its (signed) claims, but
	// it cannot forge a grant for a different Snapshot — it lacks the snapshot key
	// — so the mismatch is rejected here.
	granted, err := token.DecryptString(snapshotKey, snap.SnapshotToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt snapshot grant: %w", err)
	}
	if granted == "" || granted != claims.SnapshotName {
		return nil, fmt.Errorf("snapshot %q not authorized for sidekick %s/%s", claims.SnapshotName, snap.NameSpace, snap.SidekickName)
	}

	// Replay check is last: only burn a nonce once the rest of the token is
	// known-good, so a malformed/expired token can't evict a legitimate nonce.
	if !s.replay.accept(claims.Nonce, claims.ExpiresAt) {
		return nil, fmt.Errorf("nonce already used")
	}
	return claims, nil
}

// grpcKeys reads the random per-Sidekick signing key and snapshot encryption key
// from its Secret. Both are generated and stored by the operator (see
// ensureGRPCKeys); they are NOT the Sidekick UID, which is discoverable and
// therefore unsuitable as a secret.
func (s *CommandServer) grpcKeys(ctx context.Context, namespace, sidekickName string) (signingKey, snapshotKey string, err error) {
	var secret corev1.Secret
	if err = s.KBClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      SigningSecretName(sidekickName),
	}, &secret); err != nil {
		return "", "", err
	}
	sign := secret.Data[SigningSecretKey]
	snap := secret.Data[SnapshotSecretKey]
	if len(sign) == 0 || len(snap) == 0 {
		return "", "", fmt.Errorf("grpc keys missing in secret %s/%s", namespace, SigningSecretName(sidekickName))
	}
	return string(sign), string(snap), nil
}

// bindingDigest computes the request-payload digest the token must be bound to.
func bindingDigest(snap *sidekickgrpc.SnapShot) (string, error) {
	payload, err := snap.BindingPayload()
	if err != nil {
		return "", err
	}
	return token.RequestDigest(payload), nil
}

func authError() *protogen.CommandResponse {
	return &protogen.CommandResponse{Status: "error", Error: errUnauthenticated}
}

// RunGRPCServer starts the Snapshot Updater Service gRPC server on :50051 and
// blocks until the server stops or the listener fails.
func RunGRPCServer(kbClient client.Client) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%v", grpcPort))
	if err != nil {
		return fmt.Errorf("failed to listen on :%s: %w", grpcPort, err)
	}

	srv := grpc.NewServer(grpc.MaxRecvMsgSize(maxRecvMsgBytes))
	protogen.RegisterCommandServiceServer(srv, &CommandServer{
		KBClient: kbClient,
		replay:   newReplayCache(),
	})

	klog.Infof("[grpc] Snapshot Updater Service listening on :%s", grpcPort)
	return srv.Serve(lis)
}

func getError(err error) *protogen.CommandResponse {
	return &protogen.CommandResponse{
		Status: "error",
		Error:  err.Error(),
	}
}

// replayCache rejects token nonces that have already been seen, within the
// token's validity window. Entries are dropped once their token would have
// expired anyway, so the cache stays bounded by (request rate × TTL).
//
// The cache is in-memory and per-process: it resets on operator restart, which
// leaves a small replay window equal to the token TTL right after a restart.
// That residual risk is acceptable for this path; closing it fully would require
// transport-level authentication (mTLS), tracked separately.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]int64 // nonce -> expiry unix seconds
}

func newReplayCache() *replayCache {
	return &replayCache{seen: make(map[string]int64)}
}

// accept records nonce and returns true if it had not been seen before.
func (c *replayCache) accept(nonce string, exp int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().Unix()
	for k, e := range c.seen {
		if e <= now {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[nonce]; ok {
		return false
	}
	c.seen[nonce] = exp
	return true
}
