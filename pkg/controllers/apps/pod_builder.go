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
	"fmt"

	appsv1alpha1 "kubeops.dev/sidekick/apis/apps/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/meta"
)

// newSidekickPod builds the sidekick Pod object for the given leader.
func newSidekickPod(sidekick *appsv1alpha1.Sidekick, leader *corev1.Pod) (*corev1.Pod, error) {
	sidekickRef := metav1.NewControllerRef(sidekick, appsv1alpha1.SchemeGroupVersion.WithKind("Sidekick"))
	leaderRef := metav1.NewControllerRef(leader, corev1.SchemeGroupVersion.WithKind("Pod"))
	leaderRef.Controller = ptr.To(false)
	leaderRef.BlockOwnerDeletion = ptr.To(false)
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sidekick.Name,
			Namespace:       sidekick.Namespace,
			Annotations:     sidekick.Annotations,
			Labels:          sidekick.Labels,
			OwnerReferences: []metav1.OwnerReference{*sidekickRef, *leaderRef},
		},
		Spec: corev1.PodSpec{
			Volumes:                       listVolumes(leader, *sidekick), // leader
			InitContainers:                make([]corev1.Container, 0, len(sidekick.Spec.InitContainers)),
			Containers:                    make([]corev1.Container, 0, len(sidekick.Spec.Containers)),
			EphemeralContainers:           sidekick.Spec.EphemeralContainers,
			RestartPolicy:                 sidekick.Spec.RestartPolicy,
			TerminationGracePeriodSeconds: sidekick.Spec.TerminationGracePeriodSeconds,
			ActiveDeadlineSeconds:         sidekick.Spec.ActiveDeadlineSeconds,
			DNSPolicy:                     sidekick.Spec.DNSPolicy,
			// NodeSelector intentionally omitted: the pod is pinned to the leader's node via NodeName.
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
	pod.Annotations[keyHash] = meta.GenerationHash(sidekick)
	pod.Annotations[keyLeader] = leader.Name
	for _, spec := range sidekick.Spec.Containers {
		container, err := convContainer(leader, spec)
		if err != nil {
			return nil, err
		}
		if container.Env == nil {
			container.Env = make([]corev1.EnvVar, 0)
		}
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "LEADER_NAME",
			Value: leader.Name,
		})
		pod.Spec.Containers = append(pod.Spec.Containers, *container)
	}
	for _, spec := range sidekick.Spec.InitContainers {
		container, err := convContainer(leader, spec)
		if err != nil {
			return nil, err
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *container)
	}
	return &pod, nil
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
