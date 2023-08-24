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

package disruption_test

import (
	"time"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Emptiness", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var node *v1.Node
	BeforeEach(func() {
		provisioner = test.Provisioner(test.ProvisionerOptions{
			TTLSecondsAfterEmpty: ptr.Int64(30),
		})
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       "default-instance-type", // need the instance type for the cluster state update
				},
			},
		})
	})

	It("should mark machines as empty", func() {
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty).IsTrue()).To(BeTrue())
	})
	It("should remove the status condition from the machine when emptiness is disabled", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = nil
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine initialization condition is false", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)
		machine.StatusConditions().MarkFalse(v1alpha5.MachineInitialized, "", "")
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine initialization condition doesn't exist", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)
		machine.Status.Conditions = lo.Reject(machine.Status.Conditions, func(s apis.Condition, _ int) bool {
			return s.Type == v1alpha5.MachineInitialized
		})
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
	It("should remove the status condition from the machine when the node doesn't exist", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
	It("should remove the status condition from non-empty machines", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		ExpectApplied(ctx, env.Client, test.Pod(test.PodOptions{
			NodeName:   node.Name,
			Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}},
		}))

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
	It("should remove the status condition when the cluster state node is nominated", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		// Add the node to the cluster state and nominate it in the internal cluster state
		Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
		cluster.NominateNodeForPod(ctx, node.Spec.ProviderID)

		result := ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		Expect(result.RequeueAfter).To(Equal(time.Second * 30))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
	})
})
