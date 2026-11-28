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

package scheduling_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/labels/labelstest"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

// BenchmarkRequirementsString measures Requirements.String over an offering-sized set of requirements, which the
// NodeOverlay store computes for every offering.
func BenchmarkRequirementsString(b *testing.B) {
	labelstest.Register(b, fake.LabelOptions()...)
	reqs := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "m5.large"),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "amd64"),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-zone-1"),
		scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		scheduling.NewRequirement(fake.LabelInstanceSize, corev1.NodeSelectorOpIn, "large"),
		scheduling.NewRequirement(corev1.LabelHostname, corev1.NodeSelectorOpExists),
		scheduling.NewRequirement("example.com/custom", corev1.NodeSelectorOpIn, "a"),
	)
	b.ReportAllocs()
	for b.Loop() {
		_ = reqs.String()
	}
}
