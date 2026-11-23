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

package perf

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func requests(cpu, memory string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
}

func planned(name string, s int, cpu, memory string) v1.NodeClaim {
	c := nc(name, s, 0)
	c.Spec.Resources.Requests = requests(cpu, memory)
	return c
}

var _ = Describe("plan kept", func() {
	DescribeTable("compares planned with bound requests", func(bound corev1.ResourceList, want PlanKept) {
		ncs := []v1.NodeClaim{planned("a", 0, "7000m", "56Gi")}
		Expect(planKept(t0, ncs, map[string]corev1.ResourceList{"a": bound})).To(Equal(want))
	},
		Entry("exact", requests("7000m", "56Gi"), PlanKept{Kept: 1}),
		Entry("within slack", requests("7040m", "56Gi"), PlanKept{Kept: 1}),
		Entry("4 small and 1 large on 2 large's plan", requests("7100m", "41072Mi"), PlanKept{Over: 1}),
		Entry("one large missing", requests("3500m", "28Gi"), PlanKept{Under: 1}),
	)
	It("counts NodeClaims without a sample as unknown and skips those from before the phase", func() {
		ncs := []v1.NodeClaim{planned("old", -30, "1", "1Gi"), planned("a", 0, "1", "1Gi"), planned("b", 0, "1", "1Gi")}
		Expect(planKept(t0, ncs, map[string]corev1.ResourceList{"a": requests("1", "1Gi")})).To(Equal(PlanKept{Kept: 1, Unknown: 1}))
	})
})
