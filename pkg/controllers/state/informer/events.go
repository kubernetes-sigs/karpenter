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

package informer

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	"sigs.k8s.io/karpenter/pkg/events"
)

func PredictionTargetUnsupportedEvent(vpa *vpav1.VerticalPodAutoscaler) events.Event {
	return events.Event{
		InvolvedObject: vpa,
		Type:           corev1.EventTypeWarning,
		Reason:         events.PredictionTargetUnsupported,
		Message:        fmt.Sprintf("Target %s %s/%s isn't a supported workload kind, prediction not used", vpa.Spec.TargetRef.APIVersion, vpa.Spec.TargetRef.Kind, vpa.Spec.TargetRef.Name),
		DedupeValues:   []string{string(vpa.UID)},
		DedupeTimeout:  time.Hour,
	}
}
