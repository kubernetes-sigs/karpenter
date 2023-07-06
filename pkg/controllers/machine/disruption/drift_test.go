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
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/pkg/apis"

	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Drift", func() {
	var provisioner *v1alpha5.Provisioner
	var machine *v1alpha5.Machine
	var node *v1.Node
	BeforeEach(func() {
		provisioner = test.Provisioner()
		machine, node = test.MachineAndNode(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					v1.LabelInstanceTypeStable:       test.RandomName(),
				},
			},
		})
		// Machines are required to be launched before they can be evaluated for drift
		machine.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
	})
	It("should detect drift", func() {
		cp.Drifted = true
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted).IsTrue()).To(BeTrue())
	})
	It("should not detect drift if the feature flag is disabled", func() {
		cp.Drifted = true
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine if the feature flag is disabled", func() {
		cp.Drifted = true
		ctx = settings.ToContext(ctx, test.Settings(settings.Settings{DriftEnabled: false}))
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine launch condition is false", func() {
		cp.Drifted = true
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		machine.StatusConditions().MarkFalse(v1alpha5.MachineLaunched, "", "")
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine when the machine launch condition doesn't exist", func() {
		cp.Drifted = true
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine, node)
		machine.Status.Conditions = lo.Reject(machine.Status.Conditions, func(s apis.Condition, _ int) bool {
			return s.Type == v1alpha5.MachineLaunched
		})
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should not detect drift if the provisioner does not exist", func() {
		cp.Drifted = true
		ExpectApplied(ctx, env.Client, machine)
		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
	It("should remove the status condition from the machine if the machine is no longer drifted", func() {
		cp.Drifted = false
		machine.StatusConditions().MarkTrue(v1alpha5.MachineDrifted)
		ExpectApplied(ctx, env.Client, provisioner, machine)

		ExpectReconcileSucceeded(ctx, disruptionController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		Expect(machine.StatusConditions().GetCondition(v1alpha5.MachineDrifted)).To(BeNil())
	})
})
