/*
Copyright The Kubernetes Authors.

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

package scheduling

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func podWithUID(uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}}
}

func TestPodErrorCache_LogsFirstOccurrence(t *testing.T) {
	c := NewPodErrorCache()
	if !c.ShouldLog(podWithUID("p1")) {
		t.Fatal("expected true on first occurrence")
	}
}

func TestPodErrorCache_SuppressesRepeatSamePod(t *testing.T) {
	c := NewPodErrorCache()
	c.ShouldLog(podWithUID("p1"))
	if c.ShouldLog(podWithUID("p1")) {
		t.Fatal("expected false on repeated same pod")
	}
}

func TestPodErrorCache_SuppressesSamePodWithDifferentError(t *testing.T) {
	c := NewPodErrorCache()
	c.ShouldLog(podWithUID("p1"))
	if c.ShouldLog(podWithUID("p1")) {
		t.Fatal("expected false for same pod even if the scheduling error changed")
	}
}

func TestPodErrorCache_AllowsDifferentPods(t *testing.T) {
	c := NewPodErrorCache()
	if !c.ShouldLog(podWithUID("p1")) {
		t.Fatal("expected true for pod p1")
	}
	if !c.ShouldLog(podWithUID("p2")) {
		t.Fatal("expected true for pod p2 (different pod)")
	}
}

func TestPodErrorCache_NilReceiverAlwaysLogs(t *testing.T) {
	var c *PodErrorCache
	if !c.ShouldLog(podWithUID("p1")) {
		t.Fatal("expected true for nil cache")
	}
}
