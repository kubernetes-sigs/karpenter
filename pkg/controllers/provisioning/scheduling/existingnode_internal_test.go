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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

var _ = Describe("ExistingNode", func() {
	// buildStateNode constructs a minimal, valid StateNode backed by a real (unmanaged) corev1.Node so that
	// Available(), Labels(), and HostName() don't need a NodeClaim.
	buildStateNode := func(name string) *state.StateNode {
		sn := state.NewNode()
		sn.Node = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			},
		}
		return sn
	}

	It("should clone HostPortUsage and VolumeUsage rather than sharing the StateNode's trackers", func() {
		stateNode := buildStateNode("node-a")
		existingNode := NewExistingNode(stateNode, &Topology{}, nil, corev1.ResourceList{}, nil, false)

		Expect(existingNode.localHostPortUsage).ToNot(BeIdenticalTo(stateNode.HostPortUsage()))
		Expect(existingNode.localVolumeUsage).ToNot(BeIdenticalTo(stateNode.VolumeUsage()))
	})

	It("should not mutate the underlying StateNode's usage trackers when Add is called", func() {
		stateNode := buildStateNode("node-b")
		existingNode := NewExistingNode(stateNode, &Topology{}, nil, corev1.ResourceList{}, nil, false)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Ports: []corev1.ContainerPort{{HostPort: 8080, Protocol: corev1.ProtocolTCP}},
				}},
			},
		}

		// Simulate what Add() does to the local usage trackers, mirroring existingnode.go's Add().
		existingNode.localHostPortUsage.Add(pod, scheduling.GetHostPorts(pod))
		existingNode.localVolumeUsage.Add(pod, scheduling.Volumes{})

		// The clones used for this simulation recorded the pod...
		Expect(existingNode.localHostPortUsage.Conflicts(pod, scheduling.GetHostPorts(pod))).To(Succeed())
		conflictingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Ports: []corev1.ContainerPort{{HostPort: 8080, Protocol: corev1.ProtocolTCP}},
				}},
			},
		}
		Expect(existingNode.localHostPortUsage.Conflicts(conflictingPod, scheduling.GetHostPorts(pod))).ToNot(Succeed())

		// ...but the original StateNode's trackers, which a second, concurrent simulation might read, are untouched.
		Expect(stateNode.HostPortUsage().Conflicts(conflictingPod, scheduling.GetHostPorts(pod))).To(Succeed())
	})
})
