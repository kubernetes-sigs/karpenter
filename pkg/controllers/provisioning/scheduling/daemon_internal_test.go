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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
)

var _ = Describe("Daemon Overhead Internals", func() {
	var daemonPod *corev1.Pod
	var nct *NodeClaimTemplate

	BeforeEach(func() {
		// The first term only matches amd64, so evaluating an arm64 instance type first has to fall through to the second term
		daemonPod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "daemon", Namespace: "default"},
			Spec: corev1.PodSpec{
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}}},
						{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"arm64"}}}},
					}},
				}},
				Containers: []corev1.Container{{Name: "daemon", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}},
			},
		}
		nct = NewNodeClaimTemplate(&v1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: v1.NodePoolSpec{Template: v1.NodeClaimTemplate{Spec: v1.NodeClaimTemplateSpec{
				NodeClassRef: &v1.NodeClassReference{Group: "karpenter.test.sh", Kind: "TestNodeClass", Name: "default"},
			}}},
		})
		nct.InstanceTypeOptions = cloudprovider.InstanceTypes{
			fake.NewInstanceType("arm-instance-type", fake.WithArchitecture("arm64")),
			fake.NewInstanceType("amd-instance-type", fake.WithArchitecture("amd64")),
		}
	})

	It("should count daemon overhead for every instance type matched by any required node affinity term", func() {
		groups := buildDaemonOverheadGroups(context.Background(), nil, []*NodeClaimTemplate{nct}, []*corev1.Pod{daemonPod})[nct]
		Expect(groups).To(HaveLen(1))
		Expect(groups[0].InstanceTypes).To(HaveLen(2))
		Expect(groups[0].DaemonOverhead.Cpu().String()).To(Equal("1"))
	})
	It("should not remove required node affinity terms from the shared daemon pod", func() {
		buildDaemonOverheadGroups(context.Background(), nil, []*NodeClaimTemplate{nct}, []*corev1.Pod{daemonPod})
		Expect(daemonPod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms).To(HaveLen(2))
	})
	It("should consider any required node affinity term when matching an existing node", func() {
		s := &Scheduler{}
		Expect(s.isDaemonPodCompatibleWithNode(daemonPod, nil, map[string]string{corev1.LabelArchStable: "arm64"})).To(BeTrue())
		Expect(s.isDaemonPodCompatibleWithNode(daemonPod, nil, map[string]string{corev1.LabelArchStable: "amd64"})).To(BeTrue())
		Expect(s.isDaemonPodCompatibleWithNode(daemonPod, nil, map[string]string{corev1.LabelArchStable: "s390x"})).To(BeFalse())
	})
})
