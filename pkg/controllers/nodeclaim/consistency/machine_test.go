/*
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

package consistency_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("MachineController", func() {
	var provisioner *v1alpha5.Provisioner

	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	Context("Termination failure", func() {
		It("should detect issues with a node that is stuck deleting due to a PDB", func() {
			machine, node := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       "default-instance-type",
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("1"),
						v1.ResourceMemory: resource.MustParse("1Gi"),
						v1.ResourcePods:   resource.MustParse("10"),
					},
				},
			})
			podsLabels := map[string]string{"myapp": "deleteme"}
			pdb := test.PodDisruptionBudget(test.PDBOptions{
				Labels:         podsLabels,
				MaxUnavailable: &intstr.IntOrString{IntVal: 0, Type: intstr.Int},
			})
			machine.Finalizers = []string{"prevent.deletion/now"}
			p := test.Pod(test.PodOptions{ObjectMeta: metav1.ObjectMeta{Labels: podsLabels}})
			ExpectApplied(ctx, env.Client, provisioner, machine, node, p, pdb)
			ExpectManualBinding(ctx, env.Client, p, node)
			_ = env.Client.Delete(ctx, machine)
			ExpectReconcileSucceeded(ctx, machineConsistencyController, client.ObjectKeyFromObject(machine))
			Expect(recorder.DetectedEvent(fmt.Sprintf("can't drain node, PDB %s/%s is blocking evictions", pdb.Namespace, pdb.Name))).To(BeTrue())
		})
	})

	Context("Node Shape", func() {
		It("should detect issues that launch with much fewer resources than expected", func() {
			machine, node := test.MachineAndNode(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
						v1.LabelInstanceTypeStable:       "arm-instance-type",
						v1alpha5.LabelNodeInitialized:    "true",
					},
				},
				Spec: v1alpha5.MachineSpec{
					Resources: v1alpha5.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("8"),
							v1.ResourceMemory: resource.MustParse("64Gi"),
							v1.ResourcePods:   resource.MustParse("5"),
						},
					},
				},
				Status: v1alpha5.MachineStatus{
					ProviderID: test.RandomProviderID(),
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    resource.MustParse("16"),
						v1.ResourceMemory: resource.MustParse("128Gi"),
						v1.ResourcePods:   resource.MustParse("10"),
					},
				},
			})
			node.Status.Capacity = v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("16"),
				v1.ResourceMemory: resource.MustParse("64Gi"),
				v1.ResourcePods:   resource.MustParse("10"),
			}
			ExpectApplied(ctx, env.Client, provisioner, machine, node)
			ExpectMakeMachinesInitialized(ctx, env.Client, machine)
			ExpectReconcileSucceeded(ctx, machineConsistencyController, client.ObjectKeyFromObject(machine))
			Expect(recorder.DetectedEvent("expected 128Gi of resource memory, but found 64Gi (50.0% of expected)")).To(BeTrue())
		})
	})
})
