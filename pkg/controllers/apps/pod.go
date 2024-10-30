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
	"k8s.io/klog/v2"
	cu "kmodules.xyz/client-go/client"
	core_util "kmodules.xyz/client-go/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *SidekickReconciler) removePodFinalizerIfMarkedForDeletion(ctx context.Context, req ctrl.Request) (bool, error) {
	var pod corev1.Pod
	err := r.Get(ctx, req.NamespacedName, &pod)
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	}

	if err == nil && pod.DeletionTimestamp != nil {
		// Increase the failureCount if the pod was terminated externally
		// if the pod was terminated externally, then it will not have
		// deletionInitiatorKey set in its annotations

		_, exists := pod.ObjectMeta.Annotations[deletionInitiatorKey]
		hash, exists1 := pod.ObjectMeta.Annotations[podHash]
		if !exists {
			var sk appsv1alpha1.Sidekick
			err = r.Get(ctx, req.NamespacedName, &sk)
			if err != nil && !errors.IsNotFound(err) {
				return false, err
			}
			// if sidekick is not found or it is in deletion state,
			// ignore updating failureCount in this case

			if err == nil && sk.DeletionTimestamp == nil && exists1 {
				if sk.Status.FailureCount == nil {
					sk.Status.FailureCount = make(map[string]bool)
				}
				sk.Status.FailureCount[hash] = true
				err = r.updateSidekickStatus(ctx, &sk)
				if err != nil && !errors.IsNotFound(err) {
					return false, err
				}
			}
		}

		// removing finalizer, the reason behind adding this finalizer is stated  below
		// where we created the pod
		if core_util.HasFinalizer(pod.ObjectMeta, getFinalizerName()) {
			err = r.removePodFinalizer(ctx, &pod)
			if err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *SidekickReconciler) deletePod(ctx context.Context, pod *corev1.Pod) error {
	err := r.setDeletionInitiatorAnnotation(ctx, pod)
	if err != nil {
		return err
	}
	return r.Delete(ctx, pod)
}

func getContainerRestartCounts(pod *corev1.Pod) int32 {
	restartCounter := int32(0)
	for _, cs := range pod.Status.ContainerStatuses {
		restartCounter += cs.RestartCount
	}
	for _, ics := range pod.Status.InitContainerStatuses {
		restartCounter += ics.RestartCount
	}
	return restartCounter
}

func getTotalContainerRestartCounts(sidekick *appsv1alpha1.Sidekick) int32 {
	totalContainerRestartCount := int32(0)
	for _, value := range sidekick.Status.ContainerRestartCountsPerPod {
		totalContainerRestartCount += value
	}
	return totalContainerRestartCount
}

func (r *SidekickReconciler) setDeletionInitiatorAnnotation(ctx context.Context, pod *corev1.Pod) error {
	_, err := cu.CreateOrPatch(ctx, r.Client, pod,
		func(in client.Object, createOp bool) client.Object {
			po := in.(*corev1.Pod)
			po.ObjectMeta.Annotations[deletionInitiatorKey] = deletionInitiatesBySidekickOperator
			return po
		},
	)
	klog.Infoln("pod annotation set: ", pod.Annotations)

	return err
}

func (r *SidekickReconciler) removePodFinalizer(ctx context.Context, pod *corev1.Pod) error {
	_, err := cu.CreateOrPatch(ctx, r.Client, pod,
		func(in client.Object, createOp bool) client.Object {
			po := in.(*corev1.Pod)
			po.ObjectMeta = core_util.RemoveFinalizer(po.ObjectMeta, getFinalizerName())
			return po
		},
	)
	return client.IgnoreNotFound(err)
}
