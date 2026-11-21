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
	"fmt"
	"sync"
	"testing"

	sidekickgrpc "kubeops.dev/sidekick/grpc"

	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func walReport(tl uint32, end, lsn string) sidekickgrpc.LogInfo {
	return sidekickgrpc.LogInfo{Type: "success", LogLimit: 5, StartTime: "t0", EndTime: end, Component: ComponentWal, Lsn: lsn, Timeline: tl}
}

// checkWalPosition asserts the recorded pair, the timeline annotation and the
// status agree.
func checkWalPosition(t *testing.T, snap *storageapi.Snapshot, tl uint32, end, lsn string) {
	t.Helper()
	if got, want := snap.Annotations[AnnotationWalPosition], formatWalPosition(tl, lsn); got != want {
		t.Fatalf("position annotation: got %q want %q", got, want)
	}
	if got, want := snap.Annotations[AnnotationWalTimeline], fmt.Sprint(tl); got != want {
		t.Fatalf("timeline annotation: got %q want %q", got, want)
	}
	ls := snap.Status.Components[ComponentWal].LogStats
	if ls == nil || ls.Lsn == nil || ls.End == nil || *ls.Lsn != lsn || *ls.End != end {
		t.Fatalf("status: want end=%s lsn=%s, got %+v", end, lsn, ls)
	}
}

func TestWalPositionFormat(t *testing.T) {
	pos, err := parseWalPosition(formatWalPosition(7, "0/9000100"))
	if err != nil || pos.timeline != 7 || pos.lsn == nil || *pos.lsn != "0/9000100" {
		t.Fatalf("round trip: %+v, %v", pos, err)
	}
	for _, v := range []string{"", "7", "x:0/1", "7:bogus", "7:", "-1:0/1"} {
		if _, err := parseWalPosition(v); err == nil {
			t.Errorf("parseWalPosition(%q): expected an error", v)
		}
	}
}

// A Snapshot written by an older server has no position pair; the timeline
// annotation and the status Lsn are used, and the pair is written on the next
// report.
func TestStoredWalPosition_FallsBackToOlderFields(t *testing.T) {
	lsn := "0/9000000"
	snap := testSnapshotObj()
	snap.Annotations = map[string]string{AnnotationWalTimeline: "6"}
	snap.Status.Components = map[string]storageapi.Component{ComponentWal: {LogStats: &storageapi.LogStats{Lsn: &lsn}}}
	if pos := storedWalPosition(snap); pos.timeline != 6 || *pos.lsn != lsn {
		t.Fatalf("fallback: %+v", pos)
	}
	// the pair wins over the separately written fields
	snap.Annotations[AnnotationWalPosition] = "7:0/5000100"
	if pos := storedWalPosition(snap); pos.timeline != 7 || *pos.lsn != "0/5000100" {
		t.Fatalf("pair: %+v", pos)
	}
}

// newRacingServers returns server A, whose first metadata or status patch of
// the Snapshot (as selected by onStatus) first lets server B, another replica
// sharing the API server but not A's lock, apply its report.
func newRacingServers(t *testing.T, onStatus bool, seed []sidekickgrpc.LogInfo, b sidekickgrpc.LogInfo) (*CommandServer, *CommandServer) {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&storageapi.Snapshot{}).
		WithObjects(testSnapshotObj()).
		Build()
	srvB := &CommandServer{KBClient: cl, replay: newReplayCache()}
	for _, info := range seed {
		if err := srvB.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var once sync.Once
	race := func() {
		once.Do(func() {
			if err := srvB.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, b); err != nil {
				t.Errorf("B: %v", err)
			}
		})
	}
	icl := interceptor.NewClient(cl, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !onStatus {
				race()
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if onStatus {
				race()
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	return &CommandServer{KBClient: icl, replay: newReplayCache()}, srvB
}

// The failover overlap of review finding S1: the old archiver reports
// (TL6, 0/9000100) while the new one reports (TL7, 0/5000100), both against
// (TL6, 0/9000000). Whichever write lands in between, the old timeline's LSN
// must never be recorded next to the new timeline.
func TestUpdateSnapshot_ConcurrentTimelinesNeverMix(t *testing.T) {
	seed := []sidekickgrpc.LogInfo{walReport(6, "a0", "0/9000000")}
	for _, onStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("B lands before A's status patch=%v", onStatus), func(t *testing.T) {
			srvA, srvB := newRacingServers(t, onStatus, seed, walReport(7, "b1", "0/5000100"))
			if err := srvA.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, walReport(6, "a1", "0/9000100")); err != nil {
				t.Fatalf("A: %v", err)
			}
			snap := getTestSnapshot(t, srvB)
			checkWalPosition(t, snap, 7, "b1", "0/5000100")
			// A's report still counts, it just does not move the position.
			if n := snap.Status.Components[ComponentWal].LogStats.TotalSucceededCount; n != 3 {
				t.Fatalf("succeeded count %d, want 3", n)
			}
		})
	}
}

// Reports handled concurrently by one server end on the highest position, and
// the pair and the status agree.
func TestUpdateSnapshot_ConcurrentReportsOneServer(t *testing.T) {
	s := newTestCommandServer(t)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		tl := uint32(6)
		if i%2 == 0 {
			tl = 7
		}
		info := walReport(tl, fmt.Sprintf("e%d-%d", tl, i), fmt.Sprintf("0/%X", 0x9000000+i))
		if tl == 7 {
			info.Lsn = fmt.Sprintf("0/%X", 0x5000000+i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err != nil {
				t.Errorf("update: %v", err)
			}
		}()
	}
	wg.Wait()
	snap := getTestSnapshot(t, s)
	checkWalPosition(t, snap, 7, "e7-20", "0/5000014")
	if n := snap.Status.Components[ComponentWal].LogStats.TotalSucceededCount; n != 20 {
		t.Fatalf("succeeded count %d, want 20", n)
	}
}

func TestKeyedMutex(t *testing.T) {
	var k keyedMutex
	unlockA := k.lock("a")
	done := make(chan struct{})
	go func() {
		unlock := k.lock("a")
		unlock()
		close(done)
	}()
	// another key is not blocked
	k.lock("b")()
	select {
	case <-done:
		t.Fatal("second lock of a key did not wait")
	default:
	}
	unlockA()
	<-done
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.locks) != 0 {
		t.Fatalf("locks left: %v", k.locks)
	}
}
