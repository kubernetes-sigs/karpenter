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

package common

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseCondition(t *testing.T) {
	for value, want := range map[string]corev1.NodeCondition{
		"KernelReady=False": {Type: "KernelReady", Status: corev1.ConditionFalse},
		"AcceleratedHardwareReady=False/NvidiaXID46Error": {Type: "AcceleratedHardwareReady", Status: corev1.ConditionFalse, Reason: "NvidiaXID46Error"},
	} {
		if got := parseCondition("test", value); got != want {
			t.Errorf("parseCondition(%q) = %+v, want %+v", value, got, want)
		}
	}
	for _, value := range []string{"KernelReady", "=False", "KernelReady=", "KernelReady=/Reason"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("parseCondition(%q) didn't panic", value)
				}
			}()
			parseCondition("test", value)
		}()
	}
}
