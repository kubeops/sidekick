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

	sidekickgrpc "kubeops.dev/sidekick/grpc"

	"gomodules.xyz/pointer"
	"k8s.io/klog/v2"
	kmc "kmodules.xyz/client-go/client"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ComponentLog = "log"

	// maxLogLimit caps the caller-supplied history limit. Without an upper bound
	// a hostile caller could set a huge limit so trimming never fires and the
	// Snapshot status grows until it hits etcd's object-size limit.
	maxLogLimit = 100
	// maxLogBytes caps the caller-supplied log/error string stored per entry.
	maxLogBytes = 4096
)

func (r *CommandServer) InitSnapshotComponentsStatus(ctx context.Context, name, namespace string) (*storageapi.Snapshot, error) {
	var snapshot storageapi.Snapshot
	err := r.KBClient.Get(ctx, client.ObjectKey{
		Namespace: namespace,
		Name:      name,
	}, &snapshot)
	if err != nil {
		return nil, err
	}

	if snapshot.Status.Components == nil {
		snapshot.Status.Components = make(map[string]storageapi.Component)
	}

	compLog := snapshot.Status.Components[ComponentLog]
	compLog.Phase = storageapi.ComponentPhaseRunning
	if compLog.LogStats == nil {
		compLog.LogStats = &storageapi.LogStats{}
	}
	snapshot.Status.Components[ComponentLog] = compLog

	if err := r.updateSnapshotStatus(ctx, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to update snapshot status :%w", err)
	}

	return &snapshot, nil
}

func (r *CommandServer) updateSnapshotStatus(ctx context.Context, snapshot *storageapi.Snapshot) error {
	_, err := kmc.PatchStatus(
		ctx,
		r.KBClient,
		snapshot,
		func(obj client.Object) client.Object {
			in := obj.(*storageapi.Snapshot)
			if in.Status.Components == nil {
				in.Status.Components = make(map[string]storageapi.Component)
			}
			in.Status.TotalComponents = snapshot.Status.TotalComponents
			in.Status.Components = snapshot.Status.Components
			return in
		})
	return err
}

func trimLogHistory(logs *[]storageapi.Log, limit int) {
	if len(*logs) > limit {
		*logs = (*logs)[1:]
	}
}

func (s *CommandServer) UpdateSnapshot(ctx context.Context, name, namespace string, info sidekickgrpc.LogInfo) error {
	// Clamp caller-controlled inputs before they touch the Snapshot status.
	if info.LogLimit > maxLogLimit {
		info.LogLimit = maxLogLimit
	}
	if info.LogLimit < 0 {
		info.LogLimit = 0
	}
	if len(info.Log) > maxLogBytes {
		info.Log = info.Log[:maxLogBytes]
	}

	snapshot, err := s.InitSnapshotComponentsStatus(ctx, name, namespace)
	if err != nil {
		klog.Errorf("[grpc] failed to initialize snapshot components status: %v", err)
		return err
	}

	component := snapshot.Status.Components[ComponentLog]
	if component.LogStats == nil {
		component.LogStats = &storageapi.LogStats{}
	}

	if component.LogStats.Start == nil {
		component.LogStats.Start = pointer.StringP(info.StartTime)
	}
	component.LogStats.End = pointer.StringP(info.EndTime)
	newLog := storageapi.Log{
		Start: pointer.StringP(info.StartTime),
		End:   pointer.StringP(info.EndTime),
	}
	if info.Type == "success" {
		component.LogStats.TotalSucceededCount += 1
		component.LogStats.End = pointer.StringP(info.EndTime)
		component.LogStats.LastSucceededStats = append(component.LogStats.LastSucceededStats, newLog)
		trimLogHistory(&component.LogStats.LastSucceededStats, info.LogLimit)
	} else {
		newLog.Error = info.Log
		component.LogStats.TotalFailedCount += 1
		component.LogStats.LastFailedStats = append(component.LogStats.LastFailedStats, newLog)
		trimLogHistory(&component.LogStats.LastFailedStats, info.LogLimit)
	}
	snapshot.Status.Components[ComponentLog] = component
	return s.updateSnapshotStatus(ctx, snapshot)
}
