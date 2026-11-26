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

package provisioning

import (
	"fmt"
	"maps"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	"sigs.k8s.io/karpenter/pkg/test"
)

// virtualPodsMetric is the fully qualified name of the snapshot gauge.
const virtualPodsMetric = "karpenter_capacitybuffers_virtual_pods"

var _ = Describe("CapacityBuffer virtual pod placement metric", func() {
	BeforeEach(func() {
		recordCapacityBufferVirtualPods(scheduler.Results{})
	})
	AfterEach(func() {
		recordCapacityBufferVirtualPods(scheduler.Results{})
	})

	It("should report virtual pod counts per namespace, capacity buffer, and node", func() {
		teamA := test.ReadyBufferInNamespace("web", "team-a", 4)
		teamB := test.ReadyBufferInNamespace("web", "team-b", 2)
		batch := test.ReadyBufferInNamespace("batch", "jobs", 2)

		teamAPods := virtualpods.BuildVirtualPods(teamA, corev1.PodTemplateSpec{})
		teamBPods := virtualpods.BuildVirtualPods(teamB, corev1.PodTemplateSpec{})
		batchPods := virtualpods.BuildVirtualPods(batch, corev1.PodTemplateSpec{})
		realPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "team-a"}}

		nodeA := makeNamedExistingNode("node-a")
		// Two buffers share node-a. The real pod on that node is not a virtual pod.
		nodeA.Pods = []*corev1.Pod{teamAPods[0], teamAPods[1], batchPods[0], realPod}
		nodeB := makeNamedExistingNode("node-b")
		nodeB.Pods = []*corev1.Pod{teamBPods[0], teamBPods[1]}
		nodeC := makeNamedExistingNode("node-c")
		nodeC.Pods = []*corev1.Pod{realPod}

		// Pods on a new NodeClaim, pods that failed to schedule, and an existing
		// node with no name are not part of the snapshot.
		unnamed := makeExistingNode("provider-unnamed")
		unnamed.Pods = []*corev1.Pod{teamAPods[2]}

		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB, nodeC, unnamed},
			NewNodeClaims: []*scheduler.NodeClaim{{Pods: []*corev1.Pod{batchPods[1]}}},
			PodErrors:     map[*corev1.Pod]error{teamAPods[3]: fmt.Errorf("no capacity")},
		})

		expectVirtualPods("team-a", "web", "node-a", 2)
		expectVirtualPods("team-b", "web", "node-b", 2)
		expectVirtualPods("jobs", "batch", "node-a", 1)
		expectNoVirtualPods("team-a", "web", "node-b")
		expectNoVirtualPods("team-b", "web", "node-a")
		expectNoVirtualPods("jobs", "batch", "node-b")
		expectNoVirtualPods("jobs", "batch", "")
		expectNoVirtualPods("team-a", "web", "")
		_, found := findMetric(virtualPodsMetric, map[string]string{
			nodeNameLabel.Name: "node-c",
		})
		Expect(found).To(BeFalse())
	})

	It("should count an in-flight node by its NodeClaim name", func() {
		cb := test.ReadyBuffer("web", 1)
		pods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})

		inflight := &scheduler.ExistingNode{StateNode: state.NewNode()}
		inflight.NodeClaim = &v1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-a"}}
		inflight.Pods = pods

		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{inflight},
		})

		expectVirtualPods("default", "web", "claim-a", 1)
	})

	It("should update the gauge when a later simulation changes the count on a node", func() {
		cb := test.ReadyBuffer("web", 3)
		pods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})
		node := makeNamedExistingNode("node-a")

		node.Pods = pods
		recordCapacityBufferVirtualPods(scheduler.Results{ExistingNodes: []*scheduler.ExistingNode{node}})
		expectVirtualPods("default", "web", "node-a", 3)

		node.Pods = pods[:1]
		recordCapacityBufferVirtualPods(scheduler.Results{ExistingNodes: []*scheduler.ExistingNode{node}})
		expectVirtualPods("default", "web", "node-a", 1)
	})

	It("should drop series for nodes and buffers that no longer have virtual pods", func() {
		teamA := test.ReadyBufferInNamespace("web", "team-a", 2)
		teamB := test.ReadyBufferInNamespace("web", "team-b", 1)
		batch := test.ReadyBufferInNamespace("batch", "jobs", 1)

		teamAPods := virtualpods.BuildVirtualPods(teamA, corev1.PodTemplateSpec{})
		teamBPods := virtualpods.BuildVirtualPods(teamB, corev1.PodTemplateSpec{})
		batchPods := virtualpods.BuildVirtualPods(batch, corev1.PodTemplateSpec{})

		nodeA := makeNamedExistingNode("node-a")
		nodeB := makeNamedExistingNode("node-b")
		nodeA.Pods = []*corev1.Pod{teamAPods[0], teamAPods[1], batchPods[0]}
		nodeB.Pods = teamBPods

		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB},
		})
		expectVirtualPods("team-a", "web", "node-a", 2)
		expectVirtualPods("team-b", "web", "node-b", 1)
		expectVirtualPods("jobs", "batch", "node-a", 1)

		// team-a/web moves onto node-b, team-b/web is gone, and jobs/batch stays.
		nodeA.Pods = batchPods
		nodeB.Pods = teamAPods
		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB},
		})

		expectNoVirtualPods("team-a", "web", "node-a")
		expectNoVirtualPods("team-b", "web", "node-b")
		expectVirtualPods("team-a", "web", "node-b", 2)
		expectVirtualPods("jobs", "batch", "node-a", 1)
	})

	It("should drop every series when a later simulation places no virtual pods", func() {
		cb := test.ReadyBufferInNamespace("web", "team-a", 2)
		pods := virtualpods.BuildVirtualPods(cb, corev1.PodTemplateSpec{})
		nodeA := makeNamedExistingNode("node-a")
		nodeB := makeNamedExistingNode("node-b")
		nodeA.Pods = pods[:1]
		nodeB.Pods = pods[1:]

		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB},
		})
		expectVirtualPods("team-a", "web", "node-a", 1)
		expectVirtualPods("team-a", "web", "node-b", 1)

		// No pending virtual pods: the completed simulation's snapshot is empty.
		recordCapacityBufferVirtualPods(scheduler.Results{})
		expectNoVirtualPods("team-a", "web", "node-a")
		expectNoVirtualPods("team-a", "web", "node-b")

		// A simulation that only places real pods is also an empty snapshot.
		nodeA.Pods = []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "real", Namespace: "team-a"}}}
		nodeB.Pods = nil
		recordCapacityBufferVirtualPods(scheduler.Results{
			ExistingNodes: []*scheduler.ExistingNode{nodeA, nodeB},
		})
		expectNoVirtualPods("team-a", "web", "node-a")
		expectNoVirtualPods("team-a", "web", "node-b")
	})
})

func makeNamedExistingNode(name string) *scheduler.ExistingNode {
	node := makeExistingNode("provider-" + name)
	node.Node.Name = name
	return node
}

func virtualPodLabels(namespace, buffer, node string) map[string]string {
	return map[string]string{
		namespaceLabel.Name:      namespace,
		capacityBufferLabel.Name: buffer,
		nodeNameLabel.Name:       node,
	}
}

func expectVirtualPods(namespace, buffer, node string, count float64) {
	GinkgoHelper()
	metric, found := findMetric(virtualPodsMetric, virtualPodLabels(namespace, buffer, node))
	Expect(found).To(BeTrue(), "metric %s should be present for %s/%s on %s", virtualPodsMetric, namespace, buffer, node)
	Expect(metric.GetGauge().GetValue()).To(Equal(count))
}

func expectNoVirtualPods(namespace, buffer, node string) {
	GinkgoHelper()
	_, found := findMetric(virtualPodsMetric, virtualPodLabels(namespace, buffer, node))
	Expect(found).To(BeFalse())
}

// findMetric is a local copy of the expectations helper. The expectations
// package imports provisioning, so this internal test cannot import it.
func findMetric(name string, labelValues map[string]string) (*dto.Metric, bool) {
	GinkgoHelper()
	families, err := crmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			unmatched := make(map[string]string, len(labelValues))
			maps.Copy(unmatched, labelValues)
			for _, label := range metric.Label {
				if value, ok := unmatched[label.GetName()]; ok && value == label.GetValue() {
					delete(unmatched, label.GetName())
				}
			}
			if len(unmatched) == 0 {
				return metric, true
			}
		}
	}
	return nil, false
}
