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

// This file holds the reconciler-side wiring for the snapshot-update gRPC
// feature: it provisions the per-Sidekick keys/grant, injects the archiver's env
// (signing key, snapshot grant, server address), and publishes the operator's
// gRPC server onto the kubeslice slice via a ServiceExport. The gRPC server
// itself lives in the snapshotserver package; this is only the setup the
// distributed reconciler performs before shipping the sidekick pod.

package apps

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"
	"kubeops.dev/sidekick/grpc/token"
	"kubeops.dev/sidekick/pkg/snapshotserver"

	kubesliceapi "github.com/kubeslice/worker-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	cu "kmodules.xyz/client-go/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// envSliceName is the wal-g container env var carrying the kubeslice slice name.
	envSliceName = "SLICE_NAME"
	// envPodName / envPodNamespace are downward-API env vars identifying the
	// operator's own pod (which runs the gRPC server).
	envPodName      = "POD_NAME"
	envPodNamespace = "POD_NAMESPACE"
	// envGRPCServerAddress is the wal-g container env var carrying the gRPC server's
	// slice DNS address (set to the same value as the ServiceExport slice alias).
	envGRPCServerAddress = "GRPC_SERVER_ADDRESS"
	// envGRPCTokenSecret is the wal-g container env var carrying the per-Sidekick
	// signing key the archiver uses to mint request-bound gRPC tokens.
	envGRPCTokenSecret = "GRPC_TOKEN_SECRET"
	// envGRPCSnapshotToken is the wal-g container env var carrying the operator-
	// issued encrypted grant naming the single Snapshot the archiver may update.
	envGRPCSnapshotToken = "GRPC_SNAPSHOT_TOKEN"
	// envSnapshotName is the wal-g container env var naming the Snapshot to update;
	// it is set by the backup tooling and is the plaintext the grant is built from.
	envSnapshotName = "SNAPSHOT_NAME"

	// signingKeyBytes is the length of a freshly generated key.
	signingKeyBytes = 32

	// kubeSliceDomainSuffix is the DNS domain kubeslice serves exported services on.
	kubeSliceDomainSuffix = "slice.local"
)

// ensureGRPCKeys returns the Sidekick's two gRPC secrets — the token signing key
// and the snapshot-name encryption key — creating them with fresh random values
// on first use and storing both in the per-Sidekick Secret.
//
//   - The signing key authenticates the archiver: it mints request-bound tokens
//     with it and the gRPC server verifies them against this same value.
//   - The snapshot key authorizes WHICH Snapshot the archiver may touch: the
//     operator encrypts the allowed Snapshot name with it (see setSnapshotGrant)
//     and the server decrypts and enforces it.
//
// Both are high-entropy random values — deliberately NOT the Sidekick UID, which
// is discoverable and unsuitable as a secret. Neither is rotated implicitly once
// created, so the archiver's env (and thus the pod spec) is stable across
// reconciles.
func (r *SidekickReconciler) ensureGRPCKeys(ctx context.Context, sidekick *appsv1alpha1.Sidekick) (signingKey, snapshotKey string, err error) {
	name := snapshotserver.SigningSecretName(sidekick.Name)
	key := types.NamespacedName{Namespace: sidekick.Namespace, Name: name}

	var existing corev1.Secret
	err = r.Get(ctx, key, &existing)
	if err == nil && len(existing.Data[snapshotserver.SigningSecretKey]) > 0 && len(existing.Data[snapshotserver.SnapshotSecretKey]) > 0 {
		return string(existing.Data[snapshotserver.SigningSecretKey]), string(existing.Data[snapshotserver.SnapshotSecretKey]), nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return "", "", err
	}

	freshSigning, err := randomKey()
	if err != nil {
		return "", "", err
	}
	freshSnapshot, err := randomKey()
	if err != nil {
		return "", "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sidekick.Namespace},
	}
	_, err = cu.CreateOrPatch(ctx, r.Client, secret, func(obj client.Object, createOp bool) client.Object {
		s := obj.(*corev1.Secret)
		s.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(sidekick, appsv1alpha1.SchemeGroupVersion.WithKind("Sidekick")),
		}
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		// Only populate missing keys so a concurrent reconcile that already wrote
		// a value is never overwritten (which would invalidate live tokens/grants).
		if len(s.Data[snapshotserver.SigningSecretKey]) == 0 {
			s.Data[snapshotserver.SigningSecretKey] = []byte(freshSigning)
		}
		if len(s.Data[snapshotserver.SnapshotSecretKey]) == 0 {
			s.Data[snapshotserver.SnapshotSecretKey] = []byte(freshSnapshot)
		}
		return s
	})
	if err != nil {
		return "", "", err
	}

	// Re-read the authoritative values: a racing reconcile may have won the create.
	if err = r.Get(ctx, key, &existing); err != nil {
		return "", "", err
	}
	if len(existing.Data[snapshotserver.SigningSecretKey]) == 0 || len(existing.Data[snapshotserver.SnapshotSecretKey]) == 0 {
		return "", "", fmt.Errorf("grpc keys empty in secret %s/%s after ensure", sidekick.Namespace, name)
	}
	return string(existing.Data[snapshotserver.SigningSecretKey]), string(existing.Data[snapshotserver.SnapshotSecretKey]), nil
}

// randomKey returns a high-entropy base64url-encoded key of signingKeyBytes bytes.
func randomKey() (string, error) {
	raw := make([]byte, signingKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// setSigningKey hands the signing key to the archiver via the wal-g container's
// env so it can mint request-bound tokens. The value is stable for the life of
// the Sidekick, so it does not cause pod-spec drift.
func (r *SidekickReconciler) setSigningKey(sidekick *appsv1alpha1.Sidekick, key string) error {
	cont := getSidekickContainer(sidekick)
	if cont == nil {
		return fmt.Errorf("wal-g container not found")
	}
	setEnv(cont, envGRPCTokenSecret, key)
	return nil
}

// setSnapshotGrant issues the per-Sidekick snapshot authorization grant: it reads
// the Snapshot name the archiver is meant to update (SNAPSHOT_NAME on the wal-g
// container, set by the backup tooling), encrypts it with the snapshot key, and
// hands the opaque ciphertext to the archiver via env. The archiver cannot read
// or forge it — it only forwards it — and the server decrypts it to learn the one
// Snapshot it will accept updates for. Encryption is deterministic, so the env
// value is stable across reconciles and does not drift the pod spec.
//
// If SNAPSHOT_NAME is unset (a Sidekick that does not push snapshot updates) no
// grant is issued; such a Sidekick simply cannot pass snapshot authorization.
func (r *SidekickReconciler) setSnapshotGrant(sidekick *appsv1alpha1.Sidekick, snapshotKey string) error {
	cont := getSidekickContainer(sidekick)
	if cont == nil {
		return fmt.Errorf("wal-g container not found")
	}
	snapshotName := getEnv(cont, envSnapshotName)
	if snapshotName == "" {
		klog.V(3).Infof("[grpc] %s not set on wal-g container of %s/%s; skipping snapshot grant",
			envSnapshotName, sidekick.Namespace, sidekick.Name)
		return nil
	}
	grant, err := token.EncryptString(snapshotKey, snapshotName)
	if err != nil {
		return fmt.Errorf("encrypt snapshot grant: %w", err)
	}
	setEnv(cont, envGRPCSnapshotToken, grant)
	return nil
}

// getEnv returns the value of env var name on the container, or "" if unset.
func getEnv(cont *appsv1alpha1.Container, name string) string {
	for i := range cont.Env {
		if cont.Env[i].Name == name {
			return cont.Env[i].Value
		}
	}
	return ""
}

// setGRPCAddress sets the kubeslice DNS address of the operator's gRPC server on
// the wal-g container, so the sidecar can reach it across the slice. It must use
// the same value advertised by the ServiceExport alias (see grpcSliceAddress).
func (r *SidekickReconciler) setGRPCAddress(sidekick *appsv1alpha1.Sidekick) error {
	cont := getSidekickContainer(sidekick)
	if cont == nil {
		return fmt.Errorf("wal-g container not found")
	}
	addr, err := grpcSliceAddress()
	if err != nil {
		return err
	}
	addr = fmt.Sprintf("%s:%d", addr, snapshotserver.GRPCPort)
	setEnv(cont, envGRPCServerAddress, addr)
	return nil
}

func getSidekickContainer(sk *appsv1alpha1.Sidekick) *appsv1alpha1.Container {
	for i := range sk.Spec.Containers {
		if sk.Spec.Containers[i].Name == "wal-g" {
			return &sk.Spec.Containers[i]
		}
	}
	return nil
}

// setEnv upserts an env var on the container in place.
func setEnv(cont *appsv1alpha1.Container, name, value string) {
	for i := range cont.Env {
		if cont.Env[i].Name == name {
			cont.Env[i].Value = value
			return
		}
	}
	cont.Env = append(cont.Env, corev1.EnvVar{Name: name, Value: value})
}

var (
	clusterDomain     string
	clusterDomainOnce sync.Once
)

// findDomain resolves the cluster DNS domain (e.g. "cluster.local") from
// /etc/resolv.conf, falling back to "cluster.local" on any error.
func findDomain() string {
	clusterDomainOnce.Do(func() {
		d, err := resolveDomain()
		if err != nil {
			klog.Errorf("failed to find domain: %v", err)
			d = "cluster.local"
		}
		clusterDomain = d
	})
	return clusterDomain
}

func resolveDomain() (string, error) {
	const filePath = "/etc/resolv.conf"
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open %s: %v", filePath, err)
	}
	defer file.Close() //nolint:errcheck

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "search ") {
			// search demo.svc.cluster.local svc.cluster.local cluster.local
			for field := range strings.FieldsSeq(line) {
				if strings.HasPrefix(field, "svc.") && !strings.HasPrefix(field, "svc.svc.") {
					return strings.TrimPrefix(field, "svc."), nil
				}
			}
			return "", fmt.Errorf("failed to find domain: %s", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading %s: %v", filePath, err)
	}
	return "", fmt.Errorf("no suitable domain found in %s", filePath)
}

// get Slice Name returns the kubeslice slice name the sidekick belongs to. It is
// carried as an env var on the wal-g container (alongside SNAPSHOT_NAME).
func getSliceName(sidekick *appsv1alpha1.Sidekick) (string, error) {
	cont := getSidekickContainer(sidekick)
	if cont == nil {
		return "", fmt.Errorf("wal-g container not found")
	}
	for _, env := range cont.Env {
		if env.Name == envSliceName {
			return env.Value, nil
		}
	}
	return "", fmt.Errorf("no %s env in wal-g container", envSliceName)
}

// sliceDNS returns the kubeslice DNS name a service exported on the slice is
// reachable at, e.g. "kubedb-sidekick.<ns>.svc.slice.local".
func sliceDNS(svcName, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.%s", svcName, namespace, kubeSliceDomainSuffix)
}

// grpcSliceAddress returns the slice DNS address of the operator's gRPC server,
// derived from its own pod identity (POD_NAME/POD_NAMESPACE). wal-g sidecars in
// remote clusters dial this address; it matches the ServiceExport slice alias.
func grpcSliceAddress() (string, error) {
	podName := os.Getenv(envPodName)
	namespace := os.Getenv(envPodNamespace)
	if podName == "" || namespace == "" {
		return "", fmt.Errorf("%s/%s env not set; cannot derive gRPC slice address", envPodName, envPodNamespace)
	}
	return sliceDNS(deploymentNameForPod(podName), namespace), nil
}

// ensure ServiceExport creates/updates a kubeslice ServiceExport that exposes the
// operator's gRPC CommandService (:50051) onto the slice. This lets the wal-g
// sidecars running in remote (spoke) clusters reach the server over the slice
// overlay network. The slice is taken from the wal-g container env; the export
// selects the operator's own pod (which runs the gRPC server), discovered via
// the downward-API POD_NAME/POD_NAMESPACE env vars.
func (r *SidekickReconciler) ensureServiceExport(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	sliceName, err := getSliceName(sidekick)
	if err != nil {
		return err
	}

	podName := os.Getenv(envPodName)
	namespace := os.Getenv(envPodNamespace)
	if podName == "" || namespace == "" {
		return fmt.Errorf("%s/%s env not set; cannot locate gRPC server pod", envPodName, envPodNamespace)
	}

	var self corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &self); err != nil {
		return fmt.Errorf("failed to get operator pod %s/%s: %w", namespace, podName, err)
	}

	// The operator pod is managed by a Deployment, so POD_NAME has the
	// "<deployment>-<replicaset-hash>-<pod-suffix>" form. Strip the two generated
	// suffixes to recover the Deployment name and use it as the exported service name.
	svcName := deploymentNameForPod(podName)

	export := &kubesliceapi.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
		},
	}
	_, err = cu.CreateOrPatch(ctx, r.Client, export, func(obj client.Object, createOp bool) client.Object {
		in := obj.(*kubesliceapi.ServiceExport)
		in.Spec.Slice = sliceName
		in.Spec.Selector = &metav1.LabelSelector{MatchLabels: stableSelectorLabels(self.Labels)}
		in.Spec.Aliases = []string{
			fmt.Sprintf("%s.%s.svc.%s", svcName, namespace, findDomain()),
			sliceDNS(svcName, namespace),
		}
		in.Spec.Ports = []kubesliceapi.ServicePort{
			{
				Name:          "grpc",
				ContainerPort: snapshotserver.GRPCPort,
				Protocol:      corev1.ProtocolTCP,
			},
		}
		return in
	})
	return err
}

// deploymentNameForPod recovers the Deployment name from a pod name by stripping
// the ReplicaSet hash and pod suffix a Deployment appends to its pods
// (e.g. "kubedb-sidekick-5f9d777f8f-rjvdq" -> "kubedb-sidekick").
func deploymentNameForPod(podName string) string {
	parts := strings.Split(podName, "-")
	if len(parts) <= 2 {
		return podName
	}
	return strings.Join(parts[:len(parts)-2], "-")
}

// stableSelectorLabels drops volatile, per-revision labels so the resulting
// label selector keeps matching the operator pod across rollouts.
func stableSelectorLabels(labels map[string]string) map[string]string {
	volatile := map[string]bool{
		"pod-template-hash":                  true,
		"controller-revision-hash":           true,
		"statefulset.kubernetes.io/pod-name": true,
		"apps.kubernetes.io/pod-index":       true,
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !volatile[k] {
			out[k] = v
		}
	}
	return out
}
