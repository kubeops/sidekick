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
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cu "kmodules.xyz/client-go/client"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/meta"
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
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.k8s.appscode.com,resources=sidekicks/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Sidekick object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *SidekickReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.Infoln(fmt.Sprintf("reconciling %v ", req.NamespacedName))
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
		var pod corev1.Pod
		e2 := r.Get(ctx, req.NamespacedName, &pod)
		if e2 == nil {
			err := r.deletePod(ctx, &pod)
			if err != nil {
				return ctrl.Result{}, err
			}
			sidekick.Status.Leader.Name = ""
			sidekick.Status.Pod = ""
			sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
			err = r.updateSidekickStatus(ctx, &sidekick)
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		} else if err != nil {
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Second * 10,
			}, client.IgnoreNotFound(err)
		}
	} else if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var pod corev1.Pod
	e2 := r.Get(ctx, req.NamespacedName, &pod)
	if e2 != nil && !errors.IsNotFound(e2) {
		return ctrl.Result{}, client.IgnoreNotFound(e2)
	}
	if e2 == nil {
		expectedHash := meta.GenerationHash(&sidekick)
		actualHash := pod.Annotations[keyHash]

		if expectedHash != actualHash ||
			leader.Name != pod.Annotations[keyLeader] ||
			leader.Spec.NodeName != pod.Spec.NodeName || (pod.Status.Phase == corev1.PodFailed && sidekick.Spec.RestartPolicy == corev1.RestartPolicyNever) {
			if leader.Spec.NodeName != pod.Spec.NodeName && pod.Spec.NodeName != "" {
				sidekick.Status.FailureCount[string(pod.GetUID())] = true
			}
			err := r.deletePod(ctx, &pod)
			if err != nil {
				return ctrl.Result{}, err
			}
			sidekick.Status.Leader.Name = ""
			sidekick.Status.Pod = ""
			sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
			err = r.updateSidekickStatus(ctx, &sidekick)
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// sidekick.Status.Leader.Name = ""
		sidekick.Status.Pod = pod.Status.Phase
		sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
		err := r.updateSidekickStatus(ctx, &sidekick)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	} else if !errors.IsNotFound(e2) {
		return ctrl.Result{}, e2
	}

	// pod not exists, so create one

	o1 := metav1.NewControllerRef(&sidekick, appsv1alpha1.SchemeGroupVersion.WithKind("Sidekick"))
	o2 := metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))
	o2.Controller = ptr.To(false)
	o2.BlockOwnerDeletion = ptr.To(false)
	pod = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sidekick.Name,
			Namespace:       sidekick.Namespace,
			Annotations:     sidekick.Annotations,
			Labels:          sidekick.Labels,
			OwnerReferences: []metav1.OwnerReference{*o1, *o2},
		},
		Spec: corev1.PodSpec{
			Volumes:                       listVolumes(leader, sidekick), // leader
			InitContainers:                make([]corev1.Container, 0, len(sidekick.Spec.InitContainers)),
			Containers:                    make([]corev1.Container, 0, len(sidekick.Spec.Containers)),
			EphemeralContainers:           sidekick.Spec.EphemeralContainers,
			RestartPolicy:                 sidekick.Spec.RestartPolicy,
			TerminationGracePeriodSeconds: sidekick.Spec.TerminationGracePeriodSeconds,
			ActiveDeadlineSeconds:         sidekick.Spec.ActiveDeadlineSeconds,
			DNSPolicy:                     sidekick.Spec.DNSPolicy,
			// NodeSelector:                  sidekick.Spec.NodeSelector,
			ServiceAccountName:           sidekick.Spec.ServiceAccountName,
			DeprecatedServiceAccount:     sidekick.Spec.DeprecatedServiceAccount,
			AutomountServiceAccountToken: sidekick.Spec.AutomountServiceAccountToken,
			NodeName:                     leader.Spec.NodeName, // leader
			HostNetwork:                  sidekick.Spec.HostNetwork,
			HostPID:                      sidekick.Spec.HostPID,
			HostIPC:                      sidekick.Spec.HostIPC,
			ShareProcessNamespace:        sidekick.Spec.ShareProcessNamespace,
			SecurityContext:              sidekick.Spec.SecurityContext,
			ImagePullSecrets:             sidekick.Spec.ImagePullSecrets,
			Hostname:                     sidekick.Spec.Hostname,
			Subdomain:                    sidekick.Spec.Subdomain,
			Affinity:                     sidekick.Spec.Affinity,
			SchedulerName:                sidekick.Spec.SchedulerName,
			Tolerations:                  sidekick.Spec.Tolerations,
			HostAliases:                  sidekick.Spec.HostAliases,
			PriorityClassName:            sidekick.Spec.PriorityClassName,
			Priority:                     sidekick.Spec.Priority,
			DNSConfig:                    sidekick.Spec.DNSConfig,
			ReadinessGates:               sidekick.Spec.ReadinessGates,
			RuntimeClassName:             sidekick.Spec.RuntimeClassName,
			EnableServiceLinks:           sidekick.Spec.EnableServiceLinks,
			PreemptionPolicy:             sidekick.Spec.PreemptionPolicy,
			Overhead:                     sidekick.Spec.Overhead,
			TopologySpreadConstraints:    sidekick.Spec.TopologySpreadConstraints,
			SetHostnameAsFQDN:            sidekick.Spec.SetHostnameAsFQDN,
			OS:                           sidekick.Spec.OS,
			HostUsers:                    sidekick.Spec.HostUsers,
		},
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	// Do not alter the assign order
	pod.Annotations[keyHash] = meta.GenerationHash(&sidekick)
	pod.Annotations[keyLeader] = leader.Name
	for _, c := range sidekick.Spec.Containers {
		c2, err := convContainer(leader, c)
		if err != nil {
			return ctrl.Result{}, err
		}
		pod.Spec.Containers = append(pod.Spec.Containers, *c2)
	}
	for _, c := range sidekick.Spec.InitContainers {
		c2, err := convContainer(leader, c)
		if err != nil {
			return ctrl.Result{}, err
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *c2)
	}
	// Adding finalizer to pod because when user will delete this pod using
	// kubectl delete, then pod will be gracefully terminated which will led
	// to pod.status.phase: succeeded. We need to control this behaviour.
	// By adding finalizer, we will know who is deleting the object
	_, e3 := cu.CreateOrPatch(context.TODO(), r.Client, &pod,
		func(in client.Object, createOp bool) client.Object {
			po := in.(*corev1.Pod)
			po.ObjectMeta = core_util.AddFinalizer(po.ObjectMeta, getFinalizerName())
			return po
		},
	)

	if e3 != nil {
		return ctrl.Result{}, e3
	}

	sidekick.Status.Leader.Name = leader.Name
	sidekick.Status.Pod = pod.Status.Phase
	sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
	err = r.updateSidekickStatus(ctx, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func convContainer(leader *corev1.Pod, c appsv1alpha1.Container) (*corev1.Container, error) {
	c2 := corev1.Container{
		Name:                     c.Name,
		Image:                    c.Image,
		Command:                  c.Command,
		Args:                     c.Args,
		WorkingDir:               c.WorkingDir,
		Ports:                    c.Ports,
		EnvFrom:                  c.EnvFrom,
		Env:                      c.Env,
		Resources:                c.Resources,
		VolumeMounts:             make([]corev1.VolumeMount, 0, len(c.VolumeMounts)),
		VolumeDevices:            c.VolumeDevices,
		LivenessProbe:            c.LivenessProbe,
		ReadinessProbe:           c.ReadinessProbe,
		StartupProbe:             c.StartupProbe,
		Lifecycle:                c.Lifecycle,
		TerminationMessagePath:   c.TerminationMessagePath,
		TerminationMessagePolicy: c.TerminationMessagePolicy,
		ImagePullPolicy:          c.ImagePullPolicy,
		SecurityContext:          c.SecurityContext,
		Stdin:                    c.Stdin,
		StdinOnce:                c.StdinOnce,
		TTY:                      c.TTY,
	}
	for _, vm := range c.VolumeMounts {
		empty := !vm.ReadOnly &&
			vm.MountPath == "" &&
			vm.SubPath == "" &&
			vm.MountPropagation == nil &&
			vm.SubPathExpr == ""
		if empty {
			if v2 := findMount(leader, vm.Name); v2 == nil {
				return nil, fmt.Errorf("missing volume mount %s for leader %s/%s", vm.Name, leader.Namespace, leader.Name)
			} else {
				c2.VolumeMounts = append(c2.VolumeMounts, *v2)
			}
		} else {
			v2 := corev1.VolumeMount{
				Name:             vm.Name,
				ReadOnly:         vm.ReadOnly,
				MountPath:        vm.MountPath,
				SubPath:          vm.SubPath,
				MountPropagation: vm.MountPropagation,
				SubPathExpr:      vm.SubPathExpr,
			}
			c2.VolumeMounts = append(c2.VolumeMounts, v2)
		}
	}

	return &c2, nil
}

func findMount(leader *corev1.Pod, name string) *corev1.VolumeMount {
	for _, c := range leader.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == name {
				return &vm
			}
		}
	}
	for _, c := range leader.Spec.InitContainers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == name {
				return &vm
			}
		}
	}
	return nil
}

func listVolumes(leader *corev1.Pod, sidekick appsv1alpha1.Sidekick) []corev1.Volume {
	vols := make([]corev1.Volume, 0)
	vols = core_util.UpsertVolume(vols, sidekick.Spec.Volumes...)
	for _, c := range sidekick.Spec.Containers {
		if len(c.VolumeMounts) > 0 {
			vols = core_util.UpsertVolume(vols, leader.Spec.Volumes...)
			return vols
		}
	}
	for _, c := range sidekick.Spec.InitContainers {
		if len(c.VolumeMounts) > 0 {
			vols = core_util.UpsertVolume(vols, leader.Spec.Volumes...)
			return vols
		}
	}
	return nil
}

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
		for _, c := range sidekicks.Items {
			if c.Status.Leader.Name == a.GetName() {
				req = append(req, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&c)})
			}
		}
		return req
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.Sidekick{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return !meta.MustAlreadyReconciled(o)
		}))).
		Owns(&corev1.Pod{}).
		Watches(&corev1.Pod{}, leaderHandler).
		WithOptions(
			controller.Options{MaxConcurrentReconciles: 5},
		).
		Complete(r)
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

	_, err = cu.CreateOrPatch(context.TODO(), r.Client, sidekick,
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
