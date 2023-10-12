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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/options"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("Machine/Disruption", func() {
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
	It("should set multiple disruption conditions simultaneously", func() {
		cp.Drifted = "drifted"
		provisioner.Spec.TTLSecondsAfterEmpty = ptr.Int64(30)
		provisioner.Spec.TTLSecondsUntilExpired = ptr.Int64(30)
		node.Annotations = lo.Assign(node.Annotations, map[string]string{
			v1alpha5.EmptinessTimestampAnnotationKey: fakeClock.Now().Format(time.RFC3339),
		})
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		// step forward to make the node expired and empty
		fakeClock.Step(60 * time.Second)
		ExpectReconcileSucceeded(ctx, machineDisruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty).IsTrue()).To(BeTrue())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired).IsTrue()).To(BeTrue())
	})
	It("should remove multiple disruption conditions simultaneously", func() {
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineEmpty)
		machine.StatusConditions().MarkTrue(v1alpha5.MachineExpired)

		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		ExpectMakeMachinesInitialized(ctx, env.Client, machine)

		// Drift, Expiration, and Emptiness are disabled through configuration
		ctx = options.ToContext(ctx, test.Options(test.OptionsFields{FeatureGates: test.FeatureGates{Drift: lo.ToPtr(false)}}))
		ExpectReconcileSucceeded(ctx, machineDisruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineEmpty)).To(BeNil())
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineExpired)).To(BeNil())
	})
})
