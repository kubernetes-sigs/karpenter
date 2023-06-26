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

var _ = Describe("Expiration", func() {
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

	It("should remove the status condition from the machines when expiration is disabled", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired)).To(BeNil())
	})
	It("should mark machines as expired", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired).IsTrue()).To(BeTrue())
	})
	It("should remove the status condition from non-expired machines", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(200)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineExpired)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired)).To(BeNil())
	})
	It("should mark machines as expired if the node is expired but the machine isn't", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		ExpectApplied(ctx, env.Client, provisioner, node)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectApplied(ctx, env.Client, machine) // machine shouldn't be expired, but node will be
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired).IsTrue()).To(BeTrue())
	})
	It("should mark machines as expired if the machine is expired but the node isn't", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		// step forward to make the node expired
		fakeClock.Step(60 * time.Second)
		ExpectApplied(ctx, env.Client, node) // node shouldn't be expired, but machine will be
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired).IsTrue()).To(BeTrue())
	})
	It("should return the requeue interval for the time between now and when the machine expires", func() {
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(200)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)

		fakeClock.Step(time.Second * 100)

		result := ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))
		Expect(result.RequeueAfter).To(BeNumerically("~", time.Second*100, time.Second))
	})
})
