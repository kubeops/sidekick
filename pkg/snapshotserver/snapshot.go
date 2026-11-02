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
	"strconv"
	"strings"

	sidekickgrpc "kubeops.dev/sidekick/grpc"

	"gomodules.xyz/pointer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	kmc "kmodules.xyz/client-go/client"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ComponentLog = "log"
	ComponentWal = "wal"

	// AnnotationWalTimeline records the timeline of the wal component's
	// LogStats.Lsn, which KubeStash's LogStats has no field for.
	AnnotationWalTimeline = "sidekick.kubeops.dev/wal-timeline"

	// maxLogLimit caps the caller-supplied history limit. Without an upper bound
	// a hostile caller could set a huge limit so trimming never fires and the
	// Snapshot status grows until it hits etcd's object-size limit.
	maxLogLimit = 100
	// maxLogBytes caps the caller-supplied log/error string stored per entry.
	maxLogBytes = 4096
)

func (r *CommandServer) InitSnapshotComponentsStatus(ctx context.Context, name, namespace, componentName string) (*storageapi.Snapshot, error) {
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

	compLog := snapshot.Status.Components[componentName]
	compLog.Phase = storageapi.ComponentPhaseRunning
	if compLog.LogStats == nil {
		compLog.LogStats = &storageapi.LogStats{}
	}
	snapshot.Status.Components[componentName] = compLog

	if err := r.updateSnapshotStatus(ctx, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to update snapshot status :%w", err)
	}

	return &snapshot, nil
}

// GetSnapshot fetches the named Snapshot from the controller cluster and returns
// it JSON-encoded. It is the read counterpart of UpdateSnapshot: the gRPC handler
// only ever calls it with claims.SnapshotName, so a caller can retrieve only the
// Snapshot its token authorizes, never an arbitrary one.
func (s *CommandServer) GetSnapshot(ctx context.Context, name, namespace string) ([]byte, error) {
	var snapshot storageapi.Snapshot
	if err := s.KBClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &snapshot); err != nil {
		return nil, err
	}
	data, err := json.Marshal(&snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot %s/%s: %w", namespace, name, err)
	}
	return data, nil
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
		},
	)
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

	componentName, err := logComponentName(info.Component)
	if err != nil {
		return err
	}

	snapshot, err := s.InitSnapshotComponentsStatus(ctx, name, namespace, componentName)
	if err != nil {
		klog.Errorf("[grpc] failed to initialize snapshot components status: %v", err)
		return err
	}

	component := snapshot.Status.Components[componentName]
	if component.LogStats == nil {
		component.LogStats = &storageapi.LogStats{}
	}

	var recordTimeline uint32
	newLog := storageapi.Log{
		Start: pointer.StringP(info.StartTime),
		End:   pointer.StringP(info.EndTime),
	}
	if componentName == ComponentWal {
		// Start, End and Lsn are the archived range PITR restores within, so
		// they come only from a successful update and point at WAL that is
		// already in the bucket; End and Lsn never move backward.
		commitTime := info.CommitTime
		if commitTime == "" {
			commitTime = info.EndTime
		}
		if info.Type == "success" {
			if component.LogStats.Start == nil {
				component.LogStats.Start = pointer.StringP(commitTime)
			}
			current := walPosition{timeline: storedWalTimeline(snapshot), lsn: component.LogStats.Lsn}
			if walPositionAdvances(current, info.Timeline, info.Lsn) {
				component.LogStats.End = pointer.StringP(commitTime)
				component.LogStats.Lsn = pointer.StringP(info.Lsn)
				recordTimeline = info.Timeline
			}
		}
	} else {
		if component.LogStats.Start == nil {
			component.LogStats.Start = pointer.StringP(info.StartTime)
		}
		component.LogStats.End = pointer.StringP(info.EndTime)
		if info.Lsn != "" {
			component.LogStats.Lsn = pointer.StringP(info.Lsn)
		}
	}
	if info.Type == "success" {
		component.LogStats.TotalSucceededCount += 1
		component.LogStats.LastSucceededStats = append(component.LogStats.LastSucceededStats, newLog)
		trimLogHistory(&component.LogStats.LastSucceededStats, info.LogLimit)
	} else {
		newLog.Error = info.Log
		component.LogStats.TotalFailedCount += 1
		component.LogStats.LastFailedStats = append(component.LogStats.LastFailedStats, newLog)
		trimLogHistory(&component.LogStats.LastFailedStats, info.LogLimit)
	}
	snapshot.Status.Components[componentName] = component
	if err := s.updateSnapshotStatus(ctx, snapshot); err != nil {
		return err
	}
	// The timeline goes after the status: if this patch fails, the stored
	// timeline is older than Lsn's, and the next report on the new timeline
	// still wins. Written first, a failed status patch would leave the new
	// timeline next to the old timeline's higher Lsn and block every report.
	if recordTimeline != 0 && storedWalTimeline(snapshot) != recordTimeline {
		return s.recordWalTimeline(ctx, snapshot, recordTimeline)
	}
	return nil
}

func (s *CommandServer) recordWalTimeline(ctx context.Context, snapshot *storageapi.Snapshot, timeline uint32) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{AnnotationWalTimeline: strconv.FormatUint(uint64(timeline), 10)},
		},
	})
	if err != nil {
		return err
	}
	if err := s.KBClient.Patch(ctx, snapshot, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("failed to record wal timeline %d on snapshot %s/%s: %w", timeline, snapshot.Namespace, snapshot.Name, err)
	}
	return nil
}

// storedWalTimeline returns the timeline recorded with the wal component's Lsn,
// or 0 when none is recorded.
func storedWalTimeline(snapshot *storageapi.Snapshot) uint32 {
	v, ok := snapshot.Annotations[AnnotationWalTimeline]
	if !ok {
		return 0
	}
	tl, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		klog.Warningf("[grpc] ignoring invalid %s=%q on snapshot %s/%s", AnnotationWalTimeline, v, snapshot.Namespace, snapshot.Name)
		return 0
	}
	return uint32(tl)
}

// walPosition is the recorded end of the archived WAL. timeline is 0 when it
// is not known.
type walPosition struct {
	timeline uint32
	lsn      *string
}

// walPositionAdvances reports whether the WAL position (nextTimeline, next) can
// replace the recorded one. next must parse. A later timeline always wins and
// an earlier one never does: a failover can start the new timeline below the
// position the old primary archived, and a cut-off old primary can still push
// to its abandoned timeline. Within one timeline the LSN must not go down.
//
// A zero timeline is unknown. When next has none (an older client), only LSNs
// are compared. When the recorded one has none (recorded by an older server),
// it is taken as timeline 1, so a report from any later timeline is accepted.
func walPositionAdvances(current walPosition, nextTimeline uint32, next string) bool {
	n, err := parseLSN(next)
	if err != nil {
		klog.Warningf("[grpc] ignoring wal position %q: %v", next, err)
		return false
	}
	if current.lsn == nil || *current.lsn == "" {
		return true
	}
	if nextTimeline != 0 {
		curTimeline := current.timeline
		if curTimeline == 0 {
			curTimeline = 1
		}
		if nextTimeline != curTimeline {
			return nextTimeline > curTimeline
		}
	}
	c, err := parseLSN(*current.lsn)
	if err != nil {
		return true
	}
	return n >= c
}

// parseLSN parses a Postgres LSN in its text form "X/Y" (two hex numbers).
func parseLSN(lsn string) (uint64, error) {
	hi, lo, ok := strings.Cut(lsn, "/")
	if !ok {
		return 0, fmt.Errorf("invalid lsn %q", lsn)
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid lsn %q: %w", lsn, err)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid lsn %q: %w", lsn, err)
	}
	return h<<32 | l, nil
}

// logComponentName returns the Snapshot component a log update is recorded in.
// Only the log components are allowed, so a caller cannot overwrite the status
// of another component (for example the full backup's) through this command.
func logComponentName(c string) (string, error) {
	switch c {
	case "", ComponentLog:
		return ComponentLog, nil
	case ComponentWal:
		return ComponentWal, nil
	default:
		return "", fmt.Errorf("unsupported snapshot component %q", c)
	}
}
