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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter-core/pkg/test"
)

var _ = Describe("Machine/Finalizer", func() {
	var provisioner *v1alpha5.Provisioner

	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	It("should add the finalizer if it doesn't exist", func() {
		machine := test.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
				},
			},
		})
		ExpectApplied(ctx, env.Client, provisioner, machine)
		ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machine))

		machine = ExpectExists(ctx, env.Client, machine)
		_, ok := lo.Find(machine.Finalizers, func(f string) bool {
			return f == v1alpha5.TerminationFinalizer
		})
		Expect(ok).To(BeTrue())
	})
})
