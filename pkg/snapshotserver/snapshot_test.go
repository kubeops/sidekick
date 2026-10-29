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
	"testing"

	sidekickgrpc "kubeops.dev/sidekick/grpc"

	storageapi "kubestash.dev/apimachinery/apis/storage/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestCommandServer(t *testing.T) *CommandServer {
	t.Helper()
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&storageapi.Snapshot{}).
		WithObjects(testSnapshotObj()).
		Build()
	return &CommandServer{KBClient: cl, replay: newReplayCache()}
}

func getTestSnapshot(t *testing.T, s *CommandServer) *storageapi.Snapshot {
	t.Helper()
	snap := &storageapi.Snapshot{}
	if err := s.KBClient.Get(context.TODO(), client.ObjectKey{Namespace: testNamespace, Name: testSnapshot}, snap); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	return snap
}

func TestUpdateSnapshot_DefaultsToLogComponent(t *testing.T) {
	s := newTestCommandServer(t)
	info := sidekickgrpc.LogInfo{Type: "success", LogLimit: 5, StartTime: "t0", EndTime: "t1"}
	if err := s.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err != nil {
		t.Fatalf("update: %v", err)
	}
	comp, ok := getTestSnapshot(t, s).Status.Components[ComponentLog]
	if !ok || comp.LogStats == nil || comp.LogStats.End == nil || *comp.LogStats.End != "t1" {
		t.Fatalf("log component not updated: %+v", comp)
	}
	if comp.LogStats.Lsn != nil {
		t.Fatalf("lsn set without one in the request: %v", *comp.LogStats.Lsn)
	}
}

func TestUpdateSnapshot_WalComponentWithLsn(t *testing.T) {
	s := newTestCommandServer(t)
	info := sidekickgrpc.LogInfo{Type: "success", LogLimit: 5, StartTime: "t0", EndTime: "t1", Component: ComponentWal, Lsn: "0/3000060"}
	if err := s.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err != nil {
		t.Fatalf("update: %v", err)
	}
	snap := getTestSnapshot(t, s)
	if _, ok := snap.Status.Components[ComponentLog]; ok {
		t.Fatalf("log component written for a wal update")
	}
	comp := snap.Status.Components[ComponentWal]
	if comp.LogStats == nil || comp.LogStats.Lsn == nil || *comp.LogStats.Lsn != "0/3000060" || *comp.LogStats.End != "t1" {
		t.Fatalf("wal component not updated: %+v", comp.LogStats)
	}
}

func TestUpdateSnapshot_RejectsOtherComponents(t *testing.T) {
	s := newTestCommandServer(t)
	info := sidekickgrpc.LogInfo{Type: "success", EndTime: "t1", Component: "dump"}
	if err := s.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err == nil {
		t.Fatalf("expected an error for component %q", info.Component)
	}
	if len(getTestSnapshot(t, s).Status.Components) != 0 {
		t.Fatalf("snapshot status changed on a rejected update")
	}
}

// Existing clients do not send component or lsn; their binding payload must stay
// byte-identical so tokens they mint still verify against a newer server.
func TestBindingPayload_UnchangedWithoutNewFields(t *testing.T) {
	snap := sidekickgrpc.SnapShot{SidekickName: "a", NameSpace: "b", LogInfo: sidekickgrpc.LogInfo{Type: "success", EndTime: "t1"}}
	got, err := snap.BindingPayload()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(struct {
		SidekickName string `json:"sidekickName"`
		NameSpace    string `json:"namespace"`
		LogInfo      struct {
			Type      string `json:"type"`
			Log       string `json:"log"`
			LogLimit  int    `json:"logLimit"`
			StartTime string `json:"startTime"`
			EndTime   string `json:"endTime"`
		} `json:"logInfo"`
	}{SidekickName: "a", NameSpace: "b", LogInfo: struct {
		Type      string `json:"type"`
		Log       string `json:"log"`
		LogLimit  int    `json:"logLimit"`
		StartTime string `json:"startTime"`
		EndTime   string `json:"endTime"`
	}{Type: "success", EndTime: "t1"}})
	if string(got) != string(want) {
		t.Fatalf("payload changed:\n got %s\nwant %s", got, want)
	}
}

func TestUpdateSnapshot_WalPositionNeverMovesBackward(t *testing.T) {
	s := newTestCommandServer(t)
	update := func(typ, end, lsn string) {
		t.Helper()
		info := sidekickgrpc.LogInfo{Type: typ, LogLimit: 5, StartTime: "t0", EndTime: end, Component: ComponentWal, Lsn: lsn, Log: "err"}
		if err := s.UpdateSnapshot(context.TODO(), testSnapshot, testNamespace, info); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	check := func(end, lsn string) {
		t.Helper()
		ls := getTestSnapshot(t, s).Status.Components[ComponentWal].LogStats
		if ls == nil || ls.End == nil || ls.Lsn == nil || *ls.End != end || *ls.Lsn != lsn {
			t.Fatalf("want end=%s lsn=%s, got %+v", end, lsn, ls)
		}
	}

	update("success", "t2", "0/A000060")
	check("t2", "0/A000060")
	// a lower position (late or reordered update) is ignored
	update("success", "t1", "0/9FFFFF0")
	check("t2", "0/A000060")
	// a higher position on a later timeline moves it; compared as numbers, not text
	update("success", "t3", "1/00000028")
	check("t3", "1/00000028")
	// a failure never moves it
	update("failed", "t4", "2/0")
	check("t3", "1/00000028")
	// an unparsable position is ignored
	update("success", "t5", "bogus")
	check("t3", "1/00000028")

	ls := getTestSnapshot(t, s).Status.Components[ComponentWal].LogStats
	if ls.TotalSucceededCount != 4 || ls.TotalFailedCount != 1 {
		t.Fatalf("counts: succeeded=%d failed=%d", ls.TotalSucceededCount, ls.TotalFailedCount)
	}
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{"0/0": 0, "0/A000060": 0xA000060, "1/28": 1<<32 | 0x28, "FFFFFFFF/FFFFFFFF": ^uint64(0)} {
		got, err := parseLSN(in)
		if err != nil || got != want {
			t.Fatalf("parseLSN(%q) = %x, %v; want %x", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "x/1", "1/", "100000000/0"} {
		if _, err := parseLSN(in); err == nil {
			t.Fatalf("parseLSN(%q): expected an error", in)
		}
	}
}
