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
	"reflect"
	"slices"
	"testing"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"
	"kubeops.dev/sidekick/pkg/snapshotserver"

	kubesliceapi "github.com/kubeslice/worker-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// walgSidekick builds a Sidekick carrying a wal-g container with the given env.
func walgSidekick(env ...corev1.EnvVar) *appsv1alpha1.Sidekick {
	return &appsv1alpha1.Sidekick{
		ObjectMeta: metav1.ObjectMeta{Name: "sk", Namespace: "demo", UID: "uid-123"},
		Spec: appsv1alpha1.SidekickSpec{
			Containers: []appsv1alpha1.Container{
				{Name: "wal-g", Env: env},
			},
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := appsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("appsv1alpha1: %v", err)
	}
	if err := kubesliceapi.AddToScheme(s); err != nil {
		t.Fatalf("kubesliceapi: %v", err)
	}
	if err := storageapi.AddToScheme(s); err != nil {
		t.Fatalf("storageapi: %v", err)
	}
	return s
}

func TestDeploymentNameForPod(t *testing.T) {
	cases := map[string]string{
		"kubedb-sidekick-5f9d777f8f-rjvdq": "kubedb-sidekick",
		"sidekick-7d9b6c8f5d-abcde":        "sidekick",
		"a-b-c":                            "a",
		"two-parts":                        "two-parts", // not enough segments to strip
		"single":                           "single",
	}
	for in, want := range cases {
		if got := deploymentNameForPod(in); got != want {
			t.Errorf("deploymentNameForPod(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSliceDNS(t *testing.T) {
	got := sliceDNS("kubedb-sidekick", "kubedb")
	want := "kubedb-sidekick.kubedb.svc.slice.local"
	if got != want {
		t.Fatalf("sliceDNS = %q, want %q", got, want)
	}
}

func TestGRPCSliceAddress(t *testing.T) {
	t.Setenv(envPodName, "kubedb-sidekick-5f9d777f8f-rjvdq")
	t.Setenv(envPodNamespace, "kubedb")

	got, err := grpcSliceAddress()
	if err != nil {
		t.Fatalf("grpcSliceAddress error: %v", err)
	}
	want := "kubedb-sidekick.kubedb.svc.slice.local"
	if got != want {
		t.Fatalf("grpcSliceAddress = %q, want %q", got, want)
	}
}

func TestGRPCSliceAddress_MissingEnv(t *testing.T) {
	t.Setenv(envPodName, "")
	t.Setenv(envPodNamespace, "")
	if _, err := grpcSliceAddress(); err == nil {
		t.Fatal("expected error when POD_NAME/POD_NAMESPACE are unset")
	}
}

func TestSetEnv(t *testing.T) {
	c := &appsv1alpha1.Container{Env: []corev1.EnvVar{{Name: "A", Value: "1"}}}

	setEnv(c, "B", "2") // append
	setEnv(c, "A", "9") // update in place

	want := map[string]string{"A": "9", "B": "2"}
	got := map[string]string{}
	for _, e := range c.Env {
		got[e.Name] = e.Value
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

// TestGetSidekickContainer_MutationPersists guards the fix for the range-copy bug:
// the returned pointer must reference the slice element so mutations stick.
func TestGetSidekickContainer_MutationPersists(t *testing.T) {
	sk := walgSidekick(corev1.EnvVar{Name: "SNAPSHOT_NAME", Value: "snap"})

	c := getSidekickContainer(sk)
	if c == nil {
		t.Fatal("wal-g container not found")
	}
	setEnv(c, "TOKEN", "abc")

	found := false
	for _, e := range sk.Spec.Containers[0].Env {
		if e.Name == "TOKEN" && e.Value == "abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("mutation via getSidekickContainer did not persist onto the Sidekick")
	}
}

func TestGetSliceName(t *testing.T) {
	sk := walgSidekick(corev1.EnvVar{Name: envSliceName, Value: "my-slice"})
	got, err := getSliceName(sk)
	if err != nil {
		t.Fatalf("getSliceName error: %v", err)
	}
	if got != "my-slice" {
		t.Fatalf("getSliceName = %q, want %q", got, "my-slice")
	}

	if _, err := getSliceName(walgSidekick()); err == nil {
		t.Fatal("expected error when SLICE_NAME env is absent")
	}
}

func TestStableSelectorLabels(t *testing.T) {
	in := map[string]string{
		"app.kubernetes.io/name":     "sidekick",
		"app.kubernetes.io/instance": "kubedb",
		"pod-template-hash":          "5f9d777f8f",
	}
	want := map[string]string{
		"app.kubernetes.io/name":     "sidekick",
		"app.kubernetes.io/instance": "kubedb",
	}
	if got := stableSelectorLabels(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("stableSelectorLabels = %v, want %v", got, want)
	}
}

func TestSetGRPCAddress(t *testing.T) {
	t.Setenv(envPodName, "kubedb-sidekick-5f9d777f8f-rjvdq")
	t.Setenv(envPodNamespace, "kubedb")

	r := &SidekickReconciler{}
	sk := walgSidekick(corev1.EnvVar{Name: "SNAPSHOT_NAME", Value: "snap"})
	if err := r.setGRPCAddress(sk); err != nil {
		t.Fatalf("setGRPCAddress error: %v", err)
	}

	var got string
	for _, e := range sk.Spec.Containers[0].Env {
		if e.Name == envGRPCServerAddress {
			got = e.Value
		}
	}
	// setGRPCAddress appends the gRPC port to the slice DNS name.
	want := "kubedb-sidekick.kubedb.svc.slice.local:50051"
	if got != want {
		t.Fatalf("%s env = %q, want %q", envGRPCServerAddress, got, want)
	}
}

func TestEnsureServiceExport(t *testing.T) {
	const (
		podName = "kubedb-sidekick-5f9d777f8f-rjvdq"
		ns      = "kubedb"
		svcName = "kubedb-sidekick"
	)
	t.Setenv(envPodName, podName)
	t.Setenv(envPodNamespace, ns)

	operatorPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "sidekick",
				"app.kubernetes.io/instance": "kubedb",
				"pod-template-hash":          "5f9d777f8f",
			},
		},
	}

	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(operatorPod).
		Build()

	r := &SidekickReconciler{Client: cl}
	sk := walgSidekick(corev1.EnvVar{Name: envSliceName, Value: "my-slice"})

	if err := r.ensureServiceExport(context.Background(), sk); err != nil {
		t.Fatalf("ensureServiceExport error: %v", err)
	}

	var se kubesliceapi.ServiceExport
	if err := cl.Get(context.Background(), types.NamespacedName{Name: svcName, Namespace: ns}, &se); err != nil {
		t.Fatalf("ServiceExport %s/%s not created: %v", ns, svcName, err)
	}

	if se.Spec.Slice != "my-slice" {
		t.Errorf("Slice = %q, want %q", se.Spec.Slice, "my-slice")
	}

	wantSelector := map[string]string{
		"app.kubernetes.io/name":     "sidekick",
		"app.kubernetes.io/instance": "kubedb",
	}
	if se.Spec.Selector == nil || !reflect.DeepEqual(se.Spec.Selector.MatchLabels, wantSelector) {
		t.Errorf("Selector = %+v, want MatchLabels %v (volatile labels must be stripped)", se.Spec.Selector, wantSelector)
	}

	wantAlias := "kubedb-sidekick.kubedb.svc.slice.local"
	if !slices.Contains(se.Spec.Aliases, wantAlias) {
		t.Errorf("Aliases = %v, want to contain %q", se.Spec.Aliases, wantAlias)
	}

	if len(se.Spec.Ports) != 1 ||
		se.Spec.Ports[0].ContainerPort != snapshotserver.GRPCPort ||
		se.Spec.Ports[0].Name != "grpc" ||
		se.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("Ports = %+v, want single grpc/%d/TCP", se.Spec.Ports, snapshotserver.GRPCPort)
	}

	// Idempotency: a second call must not error.
	if err := r.ensureServiceExport(context.Background(), sk); err != nil {
		t.Fatalf("ensureServiceExport (second call) error: %v", err)
	}
}
