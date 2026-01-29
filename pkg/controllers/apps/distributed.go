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
	"sort"
	"strconv"
	"time"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	cu "kmodules.xyz/client-go/client"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/meta"
	apiworkv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	ManifestWorkClusterNameLabel = "open-cluster-management.io/cluster-name"
)

func (r *SidekickReconciler) ReconcileDistributedSidekick(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	klog.Infoln(fmt.Sprintf("Starting reconciliation for distributed sidekick %v ", req.NamespacedName))

	var sidekick appsv1alpha1.Sidekick
	if err := r.Get(ctx, req.NamespacedName, &sidekick); err != nil {
		logger.Error(err, "unable to fetch Sidekick")
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	err := r.handleDistributedSidekickFinalizer(ctx, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}

	dropKey, err := r.updateDistributedSidekickPhase(ctx, req, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}
	if dropKey {
		return ctrl.Result{}, nil
	}

	leader, err := r.getDistributedLeader(ctx, sidekick)

	if errors.IsNotFound(err) || (err == nil && leader.Name != sidekick.Status.Leader.Name) {
		ns, err2 := r.getDistributedPodNamespace(ctx, req.Name)
		var mw apiworkv1.ManifestWork

		e2 := err2
		if err2 == nil {
			nsName := types.NamespacedName{
				Name:      sidekick.Name,
				Namespace: ns,
			}
			e2 = r.Get(ctx, nsName, &mw)
		}

		if e2 == nil {
			err := r.deleteMW(ctx, &mw)
			if err != nil {
				klog.V(5).Infof("error on deleting mw %v/%v, error: %v", mw.Namespace, mw.Name, err)
				return ctrl.Result{}, err
			}

			sidekick.Status.Leader.Name = ""
			sidekick.Status.Pod = ""
			sidekick.Status.ObservedGeneration = sidekick.GetGeneration()

			err = r.updateDistributedSidekickStatus(ctx, &sidekick)
			if err != nil {
				klog.V(5).Infoln("update sidekick error", err)
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
		klog.V(5).Infoln("error on leader getting", err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var mw apiworkv1.ManifestWork
	ns, err2 := r.getDistributedPodNamespace(ctx, req.Name)
	e2 := err2
	if err2 == nil {
		nsName := types.NamespacedName{
			Name:      sidekick.Name,
			Namespace: ns,
		}
		e2 = r.Get(ctx, nsName, &mw)
	}

	if e2 != nil && !errors.IsNotFound(e2) {
		return ctrl.Result{}, client.IgnoreNotFound(e2)
	}

	if e2 == nil {
		podPhase := r.extractPodStatusFromMW(&mw)
		pod, err := ExtractPodFromManifestWork(&mw)
		if err != nil {
			return ctrl.Result{}, err
		}

		nodeName := pod.Spec.NodeName
		if nodeName == "" || podPhase == "" {
			klog.V(5).Infof("node name or pod phase is empty for manifestwork %s, requeuing...", mw.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		expectedHash := meta.GenerationHash(&sidekick)
		actualHash := pod.Annotations[keyHash]
		if expectedHash != actualHash ||
			leader.Name != pod.Annotations[keyLeader] ||
			leader.Spec.NodeName != nodeName || (podPhase == string(corev1.PodFailed) && sidekick.Spec.RestartPolicy == corev1.RestartPolicyNever) {
			if leader.Spec.NodeName != nodeName {
				sidekick.Status.FailureCount[string(mw.GetUID())] = true
			}
			err := r.deleteMW(ctx, &mw)
			if err != nil {
				return ctrl.Result{}, err
			}
			sidekick.Status.Leader.Name = ""
			sidekick.Status.Pod = ""
			sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
			err = r.updateDistributedSidekickStatus(ctx, &sidekick)
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// sidekick.Status.Leader.Name = ""
		sidekick.Status.Pod = getPodPhase(podPhase)
		sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
		err = r.updateDistributedSidekickStatus(ctx, &sidekick)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	} else if !errors.IsNotFound(e2) {
		return ctrl.Result{}, e2
	}

	// pod not exists, so create one
	o1 := metav1.NewControllerRef(&sidekick, appsv1alpha1.SchemeGroupVersion.WithKind("Sidekick"))
	annotation := sidekick.Annotations
	for k, v := range leader.Annotations {
		annotation[k] = v
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sidekick.Name,
			Namespace:       sidekick.Namespace,
			Annotations:     annotation,
			Labels:          sidekick.Labels,
			OwnerReferences: []metav1.OwnerReference{*o1},
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
		if c2.Env == nil {
			c2.Env = make([]corev1.EnvVar, 0)
		}
		c2.Env = append(c2.Env, corev1.EnvVar{
			Name:  "LEADER_NAME",
			Value: leader.Name,
		})
		pod.Spec.Containers = append(pod.Spec.Containers, *c2)
	}

	for _, c := range sidekick.Spec.InitContainers {
		c2, err := convContainer(leader, c)
		if err != nil {
			return ctrl.Result{}, err
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *c2)
	}

	err = r.ensurePodManifestWork(&sidekick, &pod)
	if err != nil {
		klog.V(5).Infoln("error on manifest work creation, err:", err)
		return ctrl.Result{}, err
	}

	sidekick.Status.Leader.Name = leader.Name
	sidekick.Status.Pod = pod.Status.Phase
	sidekick.Status.ObservedGeneration = sidekick.GetGeneration()
	err = r.updateDistributedSidekickStatus(ctx, &sidekick)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SidekickReconciler) ensurePodManifestWork(sidekick *appsv1alpha1.Sidekick, pod *corev1.Pod) error {
	namespace := pod.Annotations[ManifestWorkClusterNameLabel]
	if namespace == "" {
		return fmt.Errorf("%v annotation is empty for pod %v/%v", ManifestWorkClusterNameLabel, pod.Namespace, pod.Name)
	}

	pod.APIVersion = "v1"
	pod.Kind = "Pod"
	pod.GenerateName = ""
	pod.OwnerReferences = nil

	podUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pod)
	if err != nil {
		return fmt.Errorf("failed to convert pod to unstructured: %w", err)
	}

	// Adding an extra label to only delete the pod and ignore deleting pvc
	labels := sidekick.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	labels["sidekick-name"] = sidekick.Name

	mw := &apiworkv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: apiworkv1.ManifestWorkSpec{
			Workload: apiworkv1.ManifestsTemplate{
				Manifests: []apiworkv1.Manifest{
					{
						RawExtension: runtime.RawExtension{
							Object: &unstructured.Unstructured{Object: podUnstructured},
						},
					},
				},
			},

			ManifestConfigs: []apiworkv1.ManifestConfigOption{
				{
					ResourceIdentifier: apiworkv1.ResourceIdentifier{
						Group:     "",
						Resource:  "pods",
						Name:      pod.Name,
						Namespace: pod.Namespace,
					},
					FeedbackRules: []apiworkv1.FeedbackRule{
						{
							Type: apiworkv1.JSONPathsType,
							JsonPaths: []apiworkv1.JsonPath{
								{
									Name: "PodPhase",
									Path: ".status.phase",
								},
								{
									Name: "PodIP",
									Path: ".status.podIP",
								},
								{
									Name: "ReadyCondition",
									Path: ".status.conditions[?(@.type=='Ready')]",
								},
								{
									Name: "NodeName",
									Path: ".spec.nodeName",
								},
								{
									Name: "ContainerRestartCount",
									Path: ".status.containerStatuses[*].restartCount",
								},
								{
									Name: "InitContainerRestartCount",
									Path: ".status.initContainerStatuses[*].restartCount",
								},
							},
						},
					},
					UpdateStrategy: &apiworkv1.UpdateStrategy{
						Type: apiworkv1.UpdateStrategyTypeServerSideApply,
						ServerSideApply: &apiworkv1.ServerSideApplyConfig{
							Force: true,
							IgnoreFields: []apiworkv1.IgnoreField{
								{
									Condition: apiworkv1.IgnoreFieldsConditionOnSpokePresent,
									JSONPaths: []string{
										"$.metadata.labels",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = cu.CreateOrPatch(context.TODO(), r.Client, mw, func(obj client.Object, createOp bool) client.Object {
		in := obj.(*apiworkv1.ManifestWork)
		in.Spec = mw.Spec
		in.Labels = mw.Labels
		return in
	})

	return err
}

func (r *SidekickReconciler) getDistributedLeader(ctx context.Context, sidekick appsv1alpha1.Sidekick) (*corev1.Pod, error) {
	leaders, err := r.findDistributedLeader(ctx, sidekick)
	if err != nil {
		klog.V(5).Infoln(fmt.Errorf("failed to get distributed leaders: %v", err))
		return nil, err
	}

	if sidekick.Spec.Leader.SelectionPolicy == appsv1alpha1.PodSelectionPolicyFirst {
		return &leaders[0], nil
	}

	return &leaders[len(leaders)-1], nil
}

func (r *SidekickReconciler) findDistributedLeader(ctx context.Context, sidekick appsv1alpha1.Sidekick) ([]corev1.Pod, error) {
	logger := log.FromContext(ctx)
	if sidekick.Spec.Leader.Name != "" {
		ns, err := r.getDistributedPodNamespace(ctx, sidekick.Name)
		if err != nil {
			return nil, err
		}

		mw, err := r.OCMClient.WorkV1().ManifestWorks(ns).Get(ctx, sidekick.Spec.Leader.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		var err2 error
		var leader *corev1.Pod
		if leader, err2 = ExtractPodFromManifestWork(mw); err2 != nil {
			logger.Error(err2, "unable to fetch Leader", "leader", sidekick.Spec.Leader.Name)
			// we'll ignore not-found errors, since they can't be fixed by an immediate
			// requeue (we'll need to wait for a new notification), and we can get them
			// on deleted requests.
			klog.Info(fmt.Errorf("unable to fetch leader: %v", err2))
			return nil, err2
		}
		return []corev1.Pod{*leader}, nil
	}

	removeRole := false

	opts := metav1.ListOptions{}
	if sidekick.Spec.Leader.Selector != nil {
		selector := sidekick.Spec.Leader.Selector.DeepCopy()
		if selector.MatchLabels != nil {
			if _, ok := selector.MatchLabels["kubedb.com/role"]; ok {
				removeRole = true
				delete(selector.MatchLabels, "kubedb.com/role")
			}
		}
		opts.LabelSelector = metav1.FormatLabelSelector(selector)
	}

	// Get all namespaces
	var nsList corev1.NamespaceList
	err := r.List(ctx, &nsList, &client.ListOptions{})
	if err != nil {
		return nil, err
	}

	var allMW []apiworkv1.ManifestWork
	for _, ns := range nsList.Items {
		mwList, err := r.OCMClient.WorkV1().ManifestWorks(ns.Name).List(ctx, opts)
		if err != nil {
			return nil, err
		}
		allMW = append(allMW, mwList.Items...)
	}

	leaders := make([]corev1.Pod, 0)
	for _, mw := range allMW {
		if len(mw.Spec.Workload.Manifests) == 0 {
			continue
		}
		manifest := mw.Spec.Workload.Manifests[0]
		pod := &corev1.Pod{}

		if err := json.Unmarshal(manifest.Raw, pod); err != nil {
			continue
		}

		if len(mw.Status.ResourceStatus.Manifests) == 0 {
			continue
		}

		feedback := mw.Status.ResourceStatus.Manifests[0].StatusFeedbacks
		newStatus := corev1.PodStatus{}
		primary := false

		nodeName := ""
		if !removeRole {
			primary = true
		}

		for _, value := range feedback.Values {
			switch value.Name {
			case "PodPhase":
				if value.Value.String != nil {
					newStatus.Phase = corev1.PodPhase(*value.Value.String)
				}
			case "PodIP":
				if value.Value.String != nil {
					newStatus.PodIP = *value.Value.String
				}
			case "ReadyCondition":
				if value.Value.JsonRaw != nil {
					var readyCondition corev1.PodCondition
					if err := json.Unmarshal([]byte(*value.Value.JsonRaw), &readyCondition); err == nil {
						newStatus.Conditions = append(newStatus.Conditions, readyCondition)
					}
				}
			case "PodRoleLabel":
				if value.Value.String != nil && *value.Value.String == "Primary" {
					primary = true
				}

			case "NodeName":
				if value.Value.String != nil {
					nodeName = *value.Value.String
				}
			}
		}

		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}

		pod.Annotations[ManifestWorkClusterNameLabel] = mw.Namespace
		pod.Status = newStatus
		pod.Spec.NodeName = nodeName

		if primary {
			leaders = append(leaders, *pod)
		}

	}
	if len(leaders) == 0 {
		klog.V(5).Infoln(fmt.Errorf("no leader found for %s", sidekick.Name))
		return nil, errors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	} else if len(leaders) == 1 {
		return []corev1.Pod{leaders[0]}, nil
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
	return leaders, nil
}

func (r *SidekickReconciler) getDistributedSidekickCurrentLeader(ctx context.Context, sidekick appsv1alpha1.Sidekick) (*apiworkv1.ManifestWork, error) {
	// Get all namespaces
	var nsList corev1.NamespaceList
	err := r.List(ctx, &nsList, &client.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, ns := range nsList.Items {
		mw, err2 := r.OCMClient.WorkV1().ManifestWorks(ns.Name).Get(ctx, sidekick.Name, metav1.GetOptions{})
		if err2 != nil && !errors.IsNotFound(err2) {
			return nil, err
		} else if err2 == nil {
			return mw, nil
		}
		err = err2
	}
	return nil, err
}

func (r *SidekickReconciler) updateDistributedSidekickPhase(ctx context.Context, req ctrl.Request, sidekick *appsv1alpha1.Sidekick) (bool, error) {
	phase, err := r.calculateDistributedSidekickPhase(ctx, sidekick)
	if err != nil {
		return false, err
	}

	if phase == appsv1alpha1.SideKickPhaseFailed {
		var pod corev1.Pod
		if err = r.Get(ctx, req.NamespacedName, &pod); err != nil {
			return true, client.IgnoreNotFound(err)
		}
		if pod.Status.Phase == corev1.PodRunning {
			pod.Status.Phase = corev1.PodFailed
			// Note: (taken from https://kubernetes.io/docs/concepts/workloads/controllers/job/#pod-backoff-failure-policy )
			// If your sidekick has restartPolicy = "OnFailure", keep in mind that your Pod running the Job will be
			// terminated once the job backoff limit has been reached. This can make debugging the Job's executable
			// more difficult. We suggest setting restartPolicy = "Never" when debugging the Job or using a logging
			// system to ensure output from failed Jobs is not lost inadvertently.
			err = r.Status().Update(ctx, &pod)
			if err != nil {
				return false, err
			}
		}
		// increasing backOffLimit or changing restartPolicy will change
		// the sidekick phase from failed to current
		return true, nil
	}
	if sidekick.Status.Phase == appsv1alpha1.SidekickPhaseSucceeded {
		return true, nil
	}
	return false, nil
}

func (r *SidekickReconciler) updateDistributedSidekickStatus(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	_, err := cu.PatchStatus(ctx, r.Client, sidekick, func(obj client.Object) client.Object {
		sk := obj.(*appsv1alpha1.Sidekick)
		sk.Status = sidekick.Status
		return sk
	})
	return err
}

func (r *SidekickReconciler) calculateDistributedSidekickPhase(ctx context.Context, sidekick *appsv1alpha1.Sidekick) (appsv1alpha1.SideKickPhase, error) {
	if sidekick.Status.ContainerRestartCountsPerPod == nil {
		sidekick.Status.ContainerRestartCountsPerPod = make(map[string]int32)
	}

	if sidekick.Status.FailureCount == nil {
		sidekick.Status.FailureCount = make(map[string]bool)
	}

	leader, err := r.getDistributedSidekickCurrentLeader(ctx, *sidekick)
	if err != nil && !errors.IsNotFound(err) {
		return sidekick.Status.Phase, err
	}

	if err == nil && leader != nil {
		restartCounter := getDistributedContainerRestartCounts(leader)
		podUID := string(leader.GetUID())
		sidekick.Status.ContainerRestartCountsPerPod[podUID] = restartCounter
		phase := r.extractPodStatusFromMW(leader)
		if phase == string(corev1.PodFailed) && leader.DeletionTimestamp == nil {
			sidekick.Status.FailureCount[podUID] = true
			// case: restartPolicy OnFailure and backOffLimit crosses when last time pod restarts,
			// in that situation we need to fail the pod, but this manual failure shouldn't take into account
			if sidekick.Spec.RestartPolicy != corev1.RestartPolicyNever && getTotalBackOffCounts(sidekick) > *sidekick.Spec.BackoffLimit {
				sidekick.Status.FailureCount[podUID] = false
			}
		}
	}

	phase := r.getDistributedSidekickPhase(sidekick, leader)
	sidekick.Status.Phase = phase

	err = r.updateDistributedSidekickStatus(ctx, sidekick)
	if err != nil {
		return "", err
	}
	return phase, nil
}

func (r *SidekickReconciler) getDistributedSidekickPhase(sidekick *appsv1alpha1.Sidekick, mw *apiworkv1.ManifestWork) appsv1alpha1.SideKickPhase {
	// if restartPolicy is always, we will always try to keep a pod running
	// if pod.status.phase == failed, then we will start a new pod
	if sidekick.Spec.RestartPolicy == corev1.RestartPolicyAlways {
		return appsv1alpha1.SideKickPhaseCurrent
	}
	if sidekick.Status.Phase == appsv1alpha1.SidekickPhaseSucceeded {
		return appsv1alpha1.SidekickPhaseSucceeded
	}

	// now restartPolicy onFailure & Never remaining
	// In both cases we return phase as succeeded if our
	// pod return with exit code 0
	if mw != nil {
		for _, manifestStatus := range mw.Status.ResourceStatus.Manifests {
			feedback := manifestStatus.StatusFeedbacks
			for _, value := range feedback.Values {
				switch value.Name {
				case "PodPhase":
					if value.Value.String != nil && *value.Value.String == "Succeeded" {
						return appsv1alpha1.SidekickPhaseSucceeded
					}
				}
			}
		}
	}

	// Now we will figure if we should update the sidekick phase
	// as failed or not by checking the backOffLimit

	backOffCounts := getTotalBackOffCounts(sidekick)
	// TODO: is it > or >= ?
	if backOffCounts > *sidekick.Spec.BackoffLimit {
		return appsv1alpha1.SideKickPhaseFailed
	}
	return appsv1alpha1.SideKickPhaseCurrent
}

func (r *SidekickReconciler) getDistributedPodNamespace(ctx context.Context, mwName string) (string, error) {
	// Get all namespaces
	var err error
	var nsList corev1.NamespaceList
	err = r.List(ctx, &nsList, &client.ListOptions{})
	if err != nil {
		return "", err
	}
	namespace := ""
	for _, ns := range nsList.Items {
		mw, err2 := r.OCMClient.WorkV1().ManifestWorks(ns.Name).Get(ctx, mwName, metav1.GetOptions{})
		if err2 != nil && !errors.IsNotFound(err2) {
			return "", err2
		}
		if err2 == nil && mw != nil {
			namespace = mw.Namespace
			break
		}
		err = err2
	}

	return namespace, err
}

func (r *SidekickReconciler) deleteMW(ctx context.Context, mw *apiworkv1.ManifestWork) error {
	err := r.setMWDeletionInitiatorAnnotation(ctx, mw)
	if err != nil {
		return err
	}
	return r.Delete(ctx, mw)
}

func (r *SidekickReconciler) setMWDeletionInitiatorAnnotation(ctx context.Context, mw *apiworkv1.ManifestWork) error {
	_, err := cu.CreateOrPatch(ctx, r.Client, mw,
		func(in client.Object, createOp bool) client.Object {
			po := in.(*apiworkv1.ManifestWork)
			if po.Annotations == nil {
				po.Annotations = make(map[string]string)
			}
			po.Annotations[deletionInitiatorKey] = deletionInitiatesBySidekickOperator
			return po
		},
	)
	return err
}

func (r *SidekickReconciler) extractPodStatusFromMW(mw *apiworkv1.ManifestWork) string {
	if len(mw.Status.ResourceStatus.Manifests) == 0 {
		return ""
	}
	feedback := mw.Status.ResourceStatus.Manifests[0].StatusFeedbacks
	podPhase := ""
	for _, value := range feedback.Values {
		switch value.Name {
		case "PodPhase":
			if value.Value.String != nil {
				podPhase = *value.Value.String
			}
		}
	}
	return podPhase
}

func getPodPhase(podPhase string) corev1.PodPhase {
	switch podPhase {
	case "Pending":
		return corev1.PodPending
	case "Running":
		return corev1.PodRunning
	case "Succeeded":
		return corev1.PodSucceeded
	case "Failed":
		return corev1.PodFailed
	case "Unknown":
		return corev1.PodUnknown
	default:
		return corev1.PodUnknown
	}
}

// extractPodFromManifestWork return the pod manifest. podMeta contain the original namespace of the pods.
func ExtractPodFromManifestWork(mw *apiworkv1.ManifestWork) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	manifest := mw.Spec.Workload.Manifests[0]
	unstructuredObj := make(map[string]any)

	err := json.Unmarshal(manifest.Raw, &unstructuredObj)
	if err != nil {
		return nil, err
	}

	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj, pod)
	if err != nil {
		return nil, err
	}

	return pod, nil
}

func getDistributedContainerRestartCounts(mw *apiworkv1.ManifestWork) int32 {
	restartCounter := int32(0)

	if len(mw.Status.ResourceStatus.Manifests) == 0 {
		return restartCounter
	}

	feedback := mw.Status.ResourceStatus.Manifests[0].StatusFeedbacks

	for _, value := range feedback.Values {
		switch value.Name {
		case "ContainerRestartCount":
			if value.Value.JsonRaw != nil {
				restartCounter += extractRestartCountFromJSONRaw(value.Value.JsonRaw)
			}
		case "InitContainerRestartCount":
			if value.Value.JsonRaw != nil {
				restartCounter += extractRestartCountFromJSONRaw(value.Value.JsonRaw)
			}
		}
	}

	return restartCounter
}

func extractRestartCountFromJSONRaw(value *string) int32 {
	restartCounter := int32(0)
	var restartCounts []int32
	if err := json.Unmarshal([]byte(*value), &restartCounts); err == nil {
		for _, count := range restartCounts {
			restartCounter += count
		}
	}
	return restartCounter
}

func (r *SidekickReconciler) handleDistributedSidekickFinalizer(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	if sidekick.DeletionTimestamp != nil {
		if core_util.HasFinalizer(sidekick.ObjectMeta, getFinalizerName()) {
			return r.terminateManifestWork(ctx, sidekick)
		}
	}

	_, err := cu.CreateOrPatch(ctx, r.Client, sidekick,
		func(in client.Object, createOp bool) client.Object {
			sk := in.(*appsv1alpha1.Sidekick)
			sk.ObjectMeta = core_util.AddFinalizer(sk.ObjectMeta, getFinalizerName())
			return sk
		},
	)
	return err
}

func (r *SidekickReconciler) terminateManifestWork(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	ns, err := r.getDistributedPodNamespace(ctx, sidekick.Name)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if ns != "" {
		err = r.Delete(ctx, &apiworkv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sidekick.Name,
				Namespace: ns,
			},
		})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
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
