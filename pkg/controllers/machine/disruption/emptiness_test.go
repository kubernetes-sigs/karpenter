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
		provisioner = test.Provisioner()
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name},
			},
		})
	})

	It("should mark machines as empty", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Format(time.RFC3339),
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		cond := machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)
		Expect(cond.IsTrue()).To(BeTrue())
		Expect(cond.Reason).To(Equal(v1alpha5.VoluntarilyDisruptedReasonEmpty))
	})
	It("should remove the status condition from the machine when emptiness is disabled", func() {
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonEmpty, "")
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)).To(BeNil())
	})
	It("should remove the status conditions from the machine when the node doesn't exist", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonEmpty, "")
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)).To(BeNil())
	})
	It("should remove the status conditions from the machine when the node doesn't have the emptiness timestamp", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonEmpty, "")
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)).To(BeNil())
	})
	It("should remove the status conditions from the machine when the node timestamp can't be parsed", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: "bad-value",
		})
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonEmpty, "")
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)).To(BeNil())
	})
	It("should remove the status condition from non-empty machines", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(200)
		machine.StatusConditions().MarkTrueWithReason(v1alpha5.MachineVoluntarilyDisrupted, v1alpha5.VoluntarilyDisruptedReasonEmpty, "")
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Add(time.Second * 100).Format(time.RFC3339),
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineVoluntarilyDisrupted)).To(BeNil())
	})
	It("should return the requeue interval for the time between now and when the machine emptiness TTL expires", func() {
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(200)
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Format(time.RFC3339),
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		fakeClock.Step(time.Second * 100)

		result := ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Second*100, time.Second))
	})
})
