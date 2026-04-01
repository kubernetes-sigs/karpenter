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

package v1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetDriftSLO_ValidDuration(t *testing.T) {
	np := &NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{DriftSLOAnnotationKey: "168h"},
		},
	}
	d := np.GetDriftSLO()
	if d == nil {
		t.Fatal("expected non-nil duration")
	}
	if *d != 168*time.Hour {
		t.Errorf("expected 168h, got %v", *d)
	}
}

func TestGetDriftSLO_InvalidValue(t *testing.T) {
	np := &NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{DriftSLOAnnotationKey: "not-a-duration"},
		},
	}
	if np.GetDriftSLO() != nil {
		t.Error("expected nil for invalid duration")
	}
}

func TestGetDriftSLO_MissingAnnotation(t *testing.T) {
	np := &NodePool{
		ObjectMeta: metav1.ObjectMeta{},
	}
	if np.GetDriftSLO() != nil {
		t.Error("expected nil when annotation missing")
	}
}

func TestGetDriftSLO_VariousFormats(t *testing.T) {
	cases := []struct {
		input    string
		expected time.Duration
	}{
		{"1h", time.Hour},
		{"30m", 30 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"24h", 24 * time.Hour},
	}
	for _, tc := range cases {
		np := &NodePool{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{DriftSLOAnnotationKey: tc.input},
			},
		}
		d := np.GetDriftSLO()
		if d == nil {
			t.Errorf("expected non-nil for %q", tc.input)
			continue
		}
		if *d != tc.expected {
			t.Errorf("for %q: expected %v, got %v", tc.input, tc.expected, *d)
		}
	}
}
