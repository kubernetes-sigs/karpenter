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

package lifecycle_test

import (
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider/fake"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Liveness", func() {
	var provisioner *v1alpha5.Provisioner

	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	It("shouldn't delete the NodeClaim when the node has registered past the registration ttl", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
			Spec: v1alpha5.MachineSpec{
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		machine = ExpectExists(ctx, env.Client, machine)
		node := test.MachineLinkedNode(machine)
		ExpectApplied(ctx, env.Client, node)

		// Node and NodeClaim should still exist
		fakeClock.Step(time.Minute * 20)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectExists(ctx, env.Client, machine)
		ExpectExists(ctx, env.Client, node)
	})
	It("should delete the NodeClaim when the Node hasn't registered past the registration ttl", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
			Spec: v1alpha5.MachineSpec{
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		machine = ExpectExists(ctx, env.Client, machine)

		// If the node hasn't registered in the registration timeframe, then we deprovision the NodeClaim
		fakeClock.Step(time.Minute * 20)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectFinalizersRemoved(ctx, env.Client, machine)
		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should delete the NodeClaim when the NodeClaim hasn't launched past the registration ttl", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
			Spec: v1alpha5.MachineSpec{
				Resources: v1alpha5.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceCPU:          resource.MustParse("2"),
						v1.ResourceMemory:       resource.MustParse("50Mi"),
						v1.ResourcePods:         resource.MustParse("5"),
						fake.ResourceGPUVendorA: resource.MustParse("1"),
					},
				},
			},
		})
		cloudProvider.AllowedCreateCalls = 0 // Don't allow Create() calls to succeed
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileFailed(ctx, machineController, client.ObjectKeyFromObject(machine))
		machine = ExpectExists(ctx, env.Client, machine)

		// If the node hasn't registered in the registration timeframe, then we deprovision the NodeClaim
		fakeClock.Step(time.Minute * 20)
		ExpectReconcileFailed(ctx, machineController, client.ObjectKeyFromObject(machine))
		ExpectFinalizersRemoved(ctx, env.Client, machine)
		ExpectNotFound(ctx, env.Client, machine)
	})
})
