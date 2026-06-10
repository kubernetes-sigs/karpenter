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
	"errors"
	"fmt"
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
	if !c.ShouldLog(podWithUID("p1"), errors.New("err")) {
		t.Fatal("expected true on first occurrence")
	}
}

func TestPodErrorCache_SuppressesRepeatSameType(t *testing.T) {
	c := NewPodErrorCache()
	err := errors.New("err")
	c.ShouldLog(podWithUID("p1"), err)
	if c.ShouldLog(podWithUID("p1"), err) {
		t.Fatal("expected false on repeated same-type error")
	}
}

func TestPodErrorCache_AllowsDifferentMessagesOfSameType(t *testing.T) {
	// Key uses err.Error() so distinct messages are treated as distinct errors, even if
	// they share the same Go type. This avoids over-deduplication when errors are wrapped
	// (e.g. via serrors.Wrap) and all carry the same concrete type.
	c := NewPodErrorCache()
	c.ShouldLog(podWithUID("p1"), errors.New("message one"))
	if !c.ShouldLog(podWithUID("p1"), errors.New("message two")) {
		t.Fatal("expected true: different messages should not be deduplicated")
	}
}

func TestPodErrorCache_AllowsDifferentErrorMessages(t *testing.T) {
	// Key is the error message string; different messages are always distinct entries.
	type errA struct{ error }
	c := NewPodErrorCache()
	if !c.ShouldLog(podWithUID("p1"), errA{errors.New("error for nodepool A")}) {
		t.Fatal("expected true for first message")
	}
	if !c.ShouldLog(podWithUID("p1"), errA{errors.New("error for nodepool B")}) {
		t.Fatal("expected true for different message (different nodepool failure)")
	}
}

func TestPodErrorCache_AllowsDifferentPods(t *testing.T) {
	c := NewPodErrorCache()
	err := fmt.Errorf("err")
	if !c.ShouldLog(podWithUID("p1"), err) {
		t.Fatal("expected true for pod p1")
	}
	if !c.ShouldLog(podWithUID("p2"), err) {
		t.Fatal("expected true for pod p2 (different pod)")
	}
}

func TestPodErrorCache_NilReceiverAlwaysLogs(t *testing.T) {
	var c *PodErrorCache
	if !c.ShouldLog(podWithUID("p1"), errors.New("err")) {
		t.Fatal("expected true for nil cache")
	}
}
