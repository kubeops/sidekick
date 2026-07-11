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
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	cu "kmodules.xyz/client-go/client"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/meta"
	ocmclient "open-cluster-management.io/api/client/work/clientset/versioned"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	keyHash   = "sidekick.appscode.com/hash"
	keyLeader = "sidekick.appscode.com/leader"
)

// SidekickReconciler reconciles a Sidekick object
type SidekickReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	OCMClient ocmclient.Interface
	// grpcOnce guards starting the gRPC server exactly once across concurrent
	// reconciles (MaxConcurrentReconciles > 1).
	grpcOnce sync.Once
}

//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks/finalizers,verbs=update
//+kubebuilder:rbac:groups=networking.kubeslice.io,resources=serviceexports,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *SidekickReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infof("reconciling %v ", req.NamespacedName)
	logger := log.FromContext(ctx, "sidekick", req.Name, "ns", req.Namespace)
	ctx = log.IntoContext(ctx, logger)

	isPodFinalizerRemoved, err := r.removePodFinalizerIfMarkedForDeletion(ctx, req)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isPodFinalizerRemoved {
		return ctrl.Result{}, nil
	}

	var sidekick appsv1alpha1.Sidekick
	if err := r.Get(ctx, req.NamespacedName, &sidekick); err != nil {
		logger.Error(err, "unable to fetch Sidekick")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sidekick.Spec.Distributed {
		return r.ReconcileDistributedSidekick(ctx, req)
	}
	err = r.handleSidekickFinalizer(ctx, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}

	dropKey, err := r.updateSidekickPhase(ctx, req, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}
	if dropKey {
		return ctrl.Result{}, nil
	}

	leader, err := r.getLeader(ctx, sidekick)
	if errors.IsNotFound(err) || (err == nil && leader.Name != sidekick.Status.Leader.Name) {
		// Leader is gone or changed: remove the existing sidekick pod, if any.
		var pod corev1.Pod
		getErr := r.Get(ctx, req.NamespacedName, &pod)
		if getErr == nil {
			return ctrl.Result{}, r.deletePodAndResetStatus(ctx, &sidekick, &pod)
		} else if err != nil {
			// No leader and no pod: wait for a leader to appear.
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Second * 10,
			}, client.IgnoreNotFound(err)
		}
		// Leader changed but the pod is already gone; fall through to create
		// a pod for the new leader.
	} else if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var pod corev1.Pod
	err = r.Get(ctx, req.NamespacedName, &pod)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err == nil {
		return ctrl.Result{}, r.syncExistingPod(ctx, &sidekick, leader, &pod)
	}
	// Pod does not exist; create one for the current leader.
	return ctrl.Result{}, r.createSidekickPod(ctx, &sidekick, leader)
}

// podNeedsRecreate reports whether the existing sidekick pod is stale and must
// be deleted so a fresh one can be created.
func podNeedsRecreate(sidekick *appsv1alpha1.Sidekick, leader, pod *corev1.Pod) bool {
	return pod.Annotations[keyHash] != meta.GenerationHash(sidekick) ||
		pod.Annotations[keyLeader] != leader.Name ||
		pod.Spec.NodeName != leader.Spec.NodeName ||
		(pod.Status.Phase == corev1.PodFailed && sidekick.Spec.RestartPolicy == corev1.RestartPolicyNever)
}

// deletePodAndResetStatus deletes the sidekick pod and clears the leader and
// pod references from status.
func (r *SidekickReconciler) deletePodAndResetStatus(ctx context.Context, sidekick *appsv1alpha1.Sidekick, pod *corev1.Pod) error {
	if err := r.deletePod(ctx, pod); err != nil {
		return err
	}
	sidekick.Status.Leader.Name = ""
	sidekick.Status.Pod = ""
	sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
	return r.updateSidekickStatus(ctx, sidekick)
}

// syncExistingPod reconciles an already-existing sidekick pod: recreates it if
// stale, otherwise records its phase in status.
func (r *SidekickReconciler) syncExistingPod(ctx context.Context, sidekick *appsv1alpha1.Sidekick, leader, pod *corev1.Pod) error {
	if podNeedsRecreate(sidekick, leader, pod) {
		if leader.Spec.NodeName != pod.Spec.NodeName && pod.Spec.NodeName != "" {
			sidekick.Status.FailureCount[string(pod.GetUID())] = true
		}
		return r.deletePodAndResetStatus(ctx, sidekick, pod)
	}
	sidekick.Status.Pod = pod.Status.Phase
	sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
	return r.updateSidekickStatus(ctx, sidekick)
}

// createSidekickPod builds the sidekick pod, creates it with the operator
// finalizer attached, and records the new leader in status.
func (r *SidekickReconciler) createSidekickPod(ctx context.Context, sidekick *appsv1alpha1.Sidekick, leader *corev1.Pod) error {
	pod, err := newSidekickPod(sidekick, leader)
	if err != nil {
		return err
	}
	// Adding finalizer to pod because when user will delete this pod using
	// kubectl delete, then pod will be gracefully terminated which will led
	// to pod.status.phase: succeeded. We need to control this behaviour.
	// By adding finalizer, we will know who is deleting the object
	_, err = cu.CreateOrPatch(
		ctx, r.Client, pod,
		func(in client.Object, createOp bool) client.Object {
			p := in.(*corev1.Pod)
			p.ObjectMeta = core_util.AddFinalizer(p.ObjectMeta, getFinalizerName())
			return p
		},
	)
	if err != nil {
		return err
	}

	sidekick.Status.Leader.Name = leader.Name
	sidekick.Status.Pod = pod.Status.Phase
	sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
	return r.updateSidekickStatus(ctx, sidekick)
}

// re extracts the trailing ordinal from StatefulSet-style pod names, e.g. "db-2" -> 2.
var re = regexp.MustCompile(`.*-(\d+)`)

func (r *SidekickReconciler) getLeader(ctx context.Context, sidekick appsv1alpha1.Sidekick) (*corev1.Pod, error) {
	logger := log.FromContext(ctx)

	if sidekick.Spec.Leader.Name != "" {
		var leader corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: sidekick.Namespace, Name: sidekick.Spec.Leader.Name}, &leader); err != nil {
			logger.Error(err, "unable to fetch Leader", "leader", sidekick.Spec.Leader.Name)
			// we'll ignore not-found errors, since they can't be fixed by an immediate
			// requeue (we'll need to wait for a new notification), and we can get them
			// on deleted requests.
			return nil, err
		}
		return &leader, nil
	}
	var candidates corev1.PodList
	opts := []client.ListOption{client.InNamespace(sidekick.Namespace)}
	if sidekick.Spec.Leader.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(sidekick.Spec.Leader.Selector)
		if err != nil {
			return nil, err
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := r.List(ctx, &candidates, opts...); err != nil {
		return nil, err
	}

	leaders := make([]corev1.Pod, 0, len(candidates.Items))
	for _, pod := range candidates.Items {
		if pod.Status.Phase == corev1.PodRunning {
			leaders = append(leaders, pod)
		}
	}

	if len(leaders) == 0 {
		return nil, errors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	} else if len(leaders) == 1 {
		return &leaders[0], nil
	}

	sort.Slice(leaders, func(i, j int) bool {
		oi := re.FindStringSubmatch(leaders[i].Name)
		oj := re.FindStringSubmatch(leaders[j].Name)
		if oi != nil && oj != nil {
			pi, _ := strconv.Atoi(oi[1])
			pj, _ := strconv.Atoi(oj[1])
			return pi < pj
		}
		return leaders[i].Name < leaders[j].Name
	})
	if sidekick.Spec.Leader.SelectionPolicy == appsv1alpha1.PodSelectionPolicyFirst {
		return &leaders[0], nil
	}
	return &leaders[len(leaders)-1], nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SidekickReconciler) SetupWithManager(mgr ctrl.Manager) error {
	leaderHandler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, a client.Object) []reconcile.Request {
		sidekicks := &appsv1alpha1.SidekickList{}
		if err := r.List(ctx, sidekicks, client.InNamespace(a.GetNamespace())); err != nil {
			return nil
		}
		var req []reconcile.Request
		for _, sk := range sidekicks.Items {
			if sk.Status.Leader.Name == a.GetName() {
				req = append(req, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&sk)})
			}
		}
		return req
	})

	// Map ManifestWork changes to Sidekick reconcile requests. ManifestWorks are created with
	// the same name as the Sidekick (pod name) and are labeled with "sidekick-name" so we
	// find Sidekick objects with matching name across namespaces and enqueue them.
	mwHandler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, a client.Object) []reconcile.Request {
		sidekicks := &appsv1alpha1.SidekickList{}
		if err := r.List(ctx, sidekicks, &client.ListOptions{}); err != nil {
			return nil
		}
		var req []reconcile.Request
		for _, sk := range sidekicks.Items {
			if sk.Name == a.GetName() {
				req = append(req, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&sk)})
			}
		}
		return req
	})

	blder := ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Sidekick{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return !meta.MustAlreadyReconciled(o)
		}))).
		Owns(&corev1.Pod{}).
		Watches(&corev1.Pod{}, leaderHandler).
		WithOptions(
			controller.Options{MaxConcurrentReconciles: 5},
		)

	// build GVK without using the deprecated generated SchemeGroupVersion variable
	gv := schema.GroupVersion{Group: apiworkv1.GroupName, Version: "v1"}
	gvk := gv.WithKind("ManifestWork")
	_, err := mgr.GetClient().RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		blder = blder.Watches(&apiworkv1.ManifestWork{}, mwHandler)
	}

	return blder.Complete(r)
}

func (r *SidekickReconciler) terminate(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	err := r.Delete(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sidekick.Name,
			Namespace: sidekick.Namespace,
		},
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	_, err = cu.CreateOrPatch(
		ctx, r.Client, sidekick,
		func(in client.Object, createOp bool) client.Object {
			sk := in.(*appsv1alpha1.Sidekick)
			sk.ObjectMeta = core_util.RemoveFinalizer(sk.ObjectMeta, getFinalizerName())

			return sk
		},
	)
	return err
}

func getFinalizerName() string {
	return appsv1alpha1.SchemeGroupVersion.Group + "/finalizer"
}
