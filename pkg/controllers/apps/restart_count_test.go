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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	apiworkv1 "open-cluster-management.io/api/work/v1"
)

func TestGetDistributedContainerRestartCounts(t *testing.T) {
	mw := func(values ...apiworkv1.FeedbackValue) *apiworkv1.ManifestWork {
		return &apiworkv1.ManifestWork{
			Status: apiworkv1.ManifestWorkStatus{
				ResourceStatus: apiworkv1.ManifestResourceStatus{
					Manifests: []apiworkv1.ManifestCondition{{
						StatusFeedbacks: apiworkv1.StatusFeedbackResult{Values: values},
					}},
				},
			},
		}
	}
	integer := func(name string, n int64) apiworkv1.FeedbackValue {
		return apiworkv1.FeedbackValue{Name: name, Value: apiworkv1.FieldValue{Type: apiworkv1.Integer, Integer: ptr.To(n)}}
	}
	raw := func(name, v string) apiworkv1.FeedbackValue {
		return apiworkv1.FeedbackValue{Name: name, Value: apiworkv1.FieldValue{Type: apiworkv1.JsonRaw, JsonRaw: ptr.To(v)}}
	}

	tests := []struct {
		name string
		mw   *apiworkv1.ManifestWork
		want int32
	}{
		{"no manifests", &apiworkv1.ManifestWork{}, 0},
		{"single container as integer", mw(integer("ContainerRestartCount", 8), integer("InitContainerRestartCount", 0)), 8},
		{"several containers as json", mw(raw("ContainerRestartCount", "[2,3]"), integer("InitContainerRestartCount", 1)), 6},
		{"other feedback ignored", mw(integer("PodRestarts", 4)), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getDistributedContainerRestartCounts(tt.mw); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsDistributedSidekickPodRunning(t *testing.T) {
	running := "Running"
	mw := func(available metav1.ConditionStatus) *apiworkv1.ManifestWork {
		m := apiworkv1.ManifestCondition{
			StatusFeedbacks: apiworkv1.StatusFeedbackResult{Values: []apiworkv1.FeedbackValue{
				{Name: "PodPhase", Value: apiworkv1.FieldValue{Type: apiworkv1.String, String: &running}},
			}},
		}
		if available != "" {
			m.Conditions = []metav1.Condition{{Type: apiworkv1.ManifestAvailable, Status: available}}
		}
		return &apiworkv1.ManifestWork{Status: apiworkv1.ManifestWorkStatus{
			ResourceStatus: apiworkv1.ManifestResourceStatus{Manifests: []apiworkv1.ManifestCondition{m}},
		}}
	}
	if !isDistributedSidekickPodRunning(mw(metav1.ConditionTrue)) {
		t.Error("available and Running: want running")
	}
	if !isDistributedSidekickPodRunning(mw("")) {
		t.Error("no Available condition, Running: want running")
	}
	// the pod was deleted on the member cluster; the feedback still says Running
	if isDistributedSidekickPodRunning(mw(metav1.ConditionFalse)) {
		t.Error("not available with stale Running feedback: want not running")
	}
}
