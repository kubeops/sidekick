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

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	cu "kmodules.xyz/client-go/client"
	core_util "kmodules.xyz/client-go/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *SidekickReconciler) handleSidekickFinalizer(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	if sidekick.DeletionTimestamp != nil {
		if core_util.HasFinalizer(sidekick.ObjectMeta, getFinalizerName()) {
			return r.terminate(ctx, sidekick)
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

func (r *SidekickReconciler) updateSidekickPhase(ctx context.Context, req ctrl.Request, sidekick *appsv1alpha1.Sidekick) (bool, error) {
	// TODO: currently not setting pending phase

	phase, err := r.calculateSidekickPhase(ctx, sidekick)
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

func (r *SidekickReconciler) calculateSidekickPhase(ctx context.Context, sidekick *appsv1alpha1.Sidekick) (appsv1alpha1.SideKickPhase, error) {
	if sidekick.Status.ContainerRestartCountsPerPod == nil {
		sidekick.Status.ContainerRestartCountsPerPod = make(map[string]int32)
	}
	if sidekick.Status.FailureCount == nil {
		sidekick.Status.FailureCount = make(map[string]bool)
	}

	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKeyFromObject(sidekick), &pod)
	if err != nil && !errors.IsNotFound(err) {
		return sidekick.Status.Phase, err
	}
	if err == nil {
		restartCounter := getContainerRestartCounts(&pod)
		podUID := string(pod.GetUID())
		sidekick.Status.ContainerRestartCountsPerPod[podUID] = restartCounter
		if pod.Status.Phase == corev1.PodFailed && pod.DeletionTimestamp == nil {
			sidekick.Status.FailureCount[podUID] = true
			// case: restartPolicy OnFailure and backOffLimit crosses when last time pod restarts,
			// in that situation we need to fail the pod, but this manual failure shouldn't take into account
			if sidekick.Spec.RestartPolicy != corev1.RestartPolicyNever && getTotalBackOffCounts(sidekick) > *sidekick.Spec.BackoffLimit {
				sidekick.Status.FailureCount[podUID] = false
			}
		}
	}

	phase := r.getSidekickPhase(sidekick, &pod)
	sidekick.Status.Phase = phase
	err = r.updateSidekickStatus(ctx, sidekick)
	if err != nil {
		return "", err
	}
	return phase, nil
}

func (r *SidekickReconciler) getSidekickPhase(sidekick *appsv1alpha1.Sidekick, pod *corev1.Pod) appsv1alpha1.SideKickPhase {
	// if restartPolicy is always, we will always try to keep a pod running
	// if pod.status.phase == failed, then we will start a new pod
	// TODO: which of these two should come first?
	if sidekick.Spec.RestartPolicy == corev1.RestartPolicyAlways {
		return appsv1alpha1.SideKickPhaseCurrent
	}
	if sidekick.Status.Phase == appsv1alpha1.SidekickPhaseSucceeded {
		return appsv1alpha1.SidekickPhaseSucceeded
	}

	// now restartPolicy onFailure & Never remaining
	// In both cases we return phase as succeeded if our
	// pod return with exit code 0
	if pod != nil && pod.Status.Phase == corev1.PodSucceeded && pod.DeletionTimestamp == nil {
		return appsv1alpha1.SidekickPhaseSucceeded
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

func (r *SidekickReconciler) updateSidekickStatus(ctx context.Context, sidekick *appsv1alpha1.Sidekick) error {
	_, err := cu.PatchStatus(ctx, r.Client, sidekick, func(obj client.Object) client.Object {
		sk := obj.(*appsv1alpha1.Sidekick)
		sk.Status = sidekick.Status
		return sk
	})
	return err
}

func getTotalBackOffCounts(sidekick *appsv1alpha1.Sidekick) int32 {
	// failureCount keeps track of the total number of pods that had pod.status.phase == failed
	failureCount := getFailureCountFromSidekickStatus(sidekick)
	// totalContainerRestartCount counts the overall
	// restart counts of all the containers over all
	// pods created by sidekick obj
	totalContainerRestartCount := getTotalContainerRestartCounts(sidekick)
	if sidekick.Spec.BackoffLimit == nil {
		sidekick.Spec.BackoffLimit = ptr.To(int32(0))
	}
	return failureCount + totalContainerRestartCount
}

func getFailureCountFromSidekickStatus(sidekick *appsv1alpha1.Sidekick) int32 {
	failureCount := int32(0)
	for _, val := range sidekick.Status.FailureCount {
		if val {
			failureCount++
		}
	}
	return failureCount
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

func getTotalContainerRestartCounts(sidekick *appsv1alpha1.Sidekick) int32 {
	totalContainerRestartCount := int32(0)
	for _, value := range sidekick.Status.ContainerRestartCountsPerPod {
		totalContainerRestartCount += value
	}
	return totalContainerRestartCount
}
