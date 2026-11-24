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

package perf_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/test/suites/performancev2/perf"
)

var _ = Describe("Utilization", func() {
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("4Gi"),
	}}}
	pod := func(cpu, mem string, owner string) corev1.Pod {
		p := corev1.Pod{Spec: corev1.PodSpec{NodeName: "n", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem),
		}}}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if owner != "" {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: owner, Name: "x"}}
		}
		return p
	}
	It("takes DaemonSet requests out of both the requests and the capacity", func() {
		cpu, mem := perf.Utilization([]corev1.Node{node}, []corev1.Pod{pod("400m", "1Gi", "ReplicaSet"), pod("200m", "2Gi", "DaemonSet")})
		Expect(cpu).To(BeNumerically("~", 0.5, 1e-9))
		Expect(mem).To(BeNumerically("~", 0.5, 1e-9))
	})
	It("ignores pods on other nodes", func() {
		other := pod("1", "1Gi", "ReplicaSet")
		other.Spec.NodeName = "elsewhere"
		cpu, _ := perf.Utilization([]corev1.Node{node}, []corev1.Pod{other})
		Expect(cpu).To(BeZero())
	})
})
