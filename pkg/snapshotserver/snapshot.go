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
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	kmc "kmodules.xyz/client-go/client"
	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ComponentLog = "log"
	ComponentWal = "wal"

	// AnnotationWalPosition records the wal component's position as one
	// "<timeline>:<lsn>" value (timeline 0 when unknown). It is written in a
	// single patch, so the timeline and the LSN always belong together; read
	// the position from here, not by pairing AnnotationWalTimeline with the
	// status Lsn, which are written separately.
	AnnotationWalPosition = "sidekick.kubeops.dev/wal-position"
	// AnnotationWalTimeline records the timeline of the recorded wal position
	// alone, for readers that predate AnnotationWalPosition.
	AnnotationWalTimeline = "sidekick.kubeops.dev/wal-timeline"
	// AnnotationWalGap records the WAL archive gap the archiver cannot close
	// (JSON written by the archiver: timeline, first, last, count, since). It is
	// set and removed by LogTypeWalGap reports on the wal component; the
	// Postgres operator turns it into a status condition.
	AnnotationWalGap = "sidekick.kubeops.dev/wal-gap"
	// LogTypeWalGap is the LogInfo.Type of a gap report: Log carries the gap,
	// an empty Log clears it. It is not a push result, so it touches neither
	// the recorded position nor the success/failure history.
	LogTypeWalGap = "wal-gap"

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

	// Reports for one Snapshot are handled one at a time: during a failover
	// the old and the new archiver report concurrently, and each report is a
	// read-check-write of the recorded WAL position.
	unlock := s.snapshotLocks.lock(namespace + "/" + name)
	defer unlock()

	if componentName == ComponentWal {
		// The writes carry the resourceVersion they were computed from, so a
		// concurrent writer elsewhere (another server replica) makes them
		// conflict; the report is then re-evaluated against the fresh Snapshot.
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			return s.updateWalComponent(ctx, name, namespace, info)
		})
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
	if component.LogStats.Start == nil {
		component.LogStats.Start = pointer.StringP(info.StartTime)
	}
	component.LogStats.End = pointer.StringP(info.EndTime)
	if info.Lsn != "" {
		component.LogStats.Lsn = pointer.StringP(info.Lsn)
	}
	appendLogHistory(component.LogStats, info)
	snapshot.Status.Components[componentName] = component
	return s.updateSnapshotStatus(ctx, snapshot)
}

// appendLogHistory counts the report and appends it to the success or failure
// history.
func appendLogHistory(ls *storageapi.LogStats, info sidekickgrpc.LogInfo) {
	newLog := storageapi.Log{
		Start: pointer.StringP(info.StartTime),
		End:   pointer.StringP(info.EndTime),
	}
	if info.Type == "success" {
		ls.TotalSucceededCount += 1
		ls.LastSucceededStats = append(ls.LastSucceededStats, newLog)
		trimLogHistory(&ls.LastSucceededStats, info.LogLimit)
	} else {
		newLog.Error = info.Log
		ls.TotalFailedCount += 1
		ls.LastFailedStats = append(ls.LastFailedStats, newLog)
		trimLogHistory(&ls.LastFailedStats, info.LogLimit)
	}
}

// updateWalComponent records one wal report. It is called again on a
// conflict, and is idempotent for a report whose position it already wrote.
//
// The WAL position is a (timeline, LSN) pair. The status subresource cannot
// carry the timeline and a status write cannot change annotations, so the pair
// is recorded as one value, AnnotationWalPosition, in a single metadata patch.
// It is the position every check compares against, and readers that need the
// timeline must read the pair from it, never the timeline from one place and
// the LSN from another. The patch is written first; the status (Lsn, End and
// the counters) follows. If the status write fails, the status only lags the
// pair, which understates what is archived; the next report brings it up.
//
// Both writes carry the resourceVersion they were computed from. A conflict
// is returned to the caller's retry, which re-reads the Snapshot and decides
// again, so a report never overwrites a position it did not compare against.
func (s *CommandServer) updateWalComponent(ctx context.Context, name, namespace string, info sidekickgrpc.LogInfo) error {
	snapshot := &storageapi.Snapshot{}
	if err := s.KBClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, snapshot); err != nil {
		return err
	}

	if info.Type == LogTypeWalGap {
		return s.recordWalGap(ctx, snapshot, info.Log)
	}

	advanced := false
	if info.Type == "success" {
		current := storedWalPosition(snapshot)
		if walPositionAdvances(current, info.Timeline, info.Lsn) {
			advanced = true
			// A report without a timeline (an older client) keeps the
			// recorded one: it was compared by LSN only.
			timeline := info.Timeline
			if timeline == 0 {
				timeline = current.timeline
			}
			if err := s.recordWalPosition(ctx, snapshot, timeline, info.Lsn); err != nil {
				return err
			}
		}
	}

	base := snapshot.DeepCopy()
	if snapshot.Status.Components == nil {
		snapshot.Status.Components = make(map[string]storageapi.Component)
	}
	component := snapshot.Status.Components[ComponentWal]
	component.Phase = storageapi.ComponentPhaseRunning
	if component.LogStats == nil {
		component.LogStats = &storageapi.LogStats{}
	}
	// Start, End and Lsn are the archived range PITR restores within, so they
	// come only from a successful update and point at WAL that is already in
	// the bucket; End and Lsn follow the recorded position and never move
	// backward.
	if info.Type == "success" {
		commitTime := info.CommitTime
		if commitTime == "" {
			commitTime = info.EndTime
		}
		if component.LogStats.Start == nil {
			component.LogStats.Start = pointer.StringP(commitTime)
		}
		if advanced {
			component.LogStats.End = pointer.StringP(commitTime)
			component.LogStats.Lsn = pointer.StringP(info.Lsn)
		}
	}
	appendLogHistory(component.LogStats, info)
	snapshot.Status.Components[ComponentWal] = component
	return s.KBClient.Status().Patch(ctx, snapshot, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// recordWalPosition writes the WAL position pair, and the timeline alone for
// readers of AnnotationWalTimeline, in one metadata patch that fails with a
// conflict if the Snapshot changed since it was read. On success snapshot
// holds the patched object.
func (s *CommandServer) recordWalPosition(ctx context.Context, snapshot *storageapi.Snapshot, timeline uint32, lsn string) error {
	if snapshot.Annotations[AnnotationWalPosition] == formatWalPosition(timeline, lsn) &&
		(timeline == 0 || snapshot.Annotations[AnnotationWalTimeline] == strconv.FormatUint(uint64(timeline), 10)) {
		return nil
	}
	base := snapshot.DeepCopy()
	annotations := snapshot.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationWalPosition] = formatWalPosition(timeline, lsn)
	if timeline != 0 {
		annotations[AnnotationWalTimeline] = strconv.FormatUint(uint64(timeline), 10)
	}
	snapshot.SetAnnotations(annotations)
	if err := s.KBClient.Patch(ctx, snapshot, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to record wal position %s on snapshot %s/%s: %w", formatWalPosition(timeline, lsn), snapshot.Namespace, snapshot.Name, err)
	}
	return nil
}

// recordWalGap sets AnnotationWalGap to gap, or removes it when gap is empty,
// in one metadata patch with the same optimistic lock as recordWalPosition.
func (s *CommandServer) recordWalGap(ctx context.Context, snapshot *storageapi.Snapshot, gap string) error {
	current, exists := snapshot.Annotations[AnnotationWalGap]
	if (gap == "" && !exists) || (gap != "" && current == gap) {
		return nil
	}
	base := snapshot.DeepCopy()
	annotations := snapshot.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if gap == "" {
		delete(annotations, AnnotationWalGap)
	} else {
		annotations[AnnotationWalGap] = gap
	}
	snapshot.SetAnnotations(annotations)
	if err := s.KBClient.Patch(ctx, snapshot, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to record the wal gap on snapshot %s/%s: %w", snapshot.Namespace, snapshot.Name, err)
	}
	return nil
}

// formatWalPosition encodes a WAL position as "<timeline>:<lsn>". Timeline 0
// means unknown.
func formatWalPosition(timeline uint32, lsn string) string {
	return strconv.FormatUint(uint64(timeline), 10) + ":" + lsn
}

// parseWalPosition decodes formatWalPosition.
func parseWalPosition(v string) (walPosition, error) {
	tl, lsn, ok := strings.Cut(v, ":")
	if !ok {
		return walPosition{}, fmt.Errorf("invalid wal position %q", v)
	}
	t, err := strconv.ParseUint(tl, 10, 32)
	if err != nil {
		return walPosition{}, fmt.Errorf("invalid wal position %q: %w", v, err)
	}
	if _, err := parseLSN(lsn); err != nil {
		return walPosition{}, fmt.Errorf("invalid wal position %q: %w", v, err)
	}
	return walPosition{timeline: uint32(t), lsn: &lsn}, nil
}

// walPositionAnnotation returns the position recorded in AnnotationWalPosition.
func walPositionAnnotation(snapshot *storageapi.Snapshot) (walPosition, bool) {
	v, ok := snapshot.Annotations[AnnotationWalPosition]
	if !ok {
		return walPosition{}, false
	}
	pos, err := parseWalPosition(v)
	if err != nil {
		klog.Warningf("[grpc] ignoring %s on snapshot %s/%s: %v", AnnotationWalPosition, snapshot.Namespace, snapshot.Name, err)
		return walPosition{}, false
	}
	return pos, true
}

// storedWalPosition returns the recorded WAL position: the pair in
// AnnotationWalPosition, or, for a Snapshot written by an older server, the
// status Lsn with the AnnotationWalTimeline timeline.
func storedWalPosition(snapshot *storageapi.Snapshot) walPosition {
	if pos, ok := walPositionAnnotation(snapshot); ok {
		return pos
	}
	pos := walPosition{timeline: storedWalTimeline(snapshot)}
	if c, ok := snapshot.Status.Components[ComponentWal]; ok && c.LogStats != nil {
		pos.lsn = c.LogStats.Lsn
	}
	return pos
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
