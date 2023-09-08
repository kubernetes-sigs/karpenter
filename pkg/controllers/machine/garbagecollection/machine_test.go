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

package garbagecollection_test

import (
	"time"

	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/test"
	nodeclaimutil "github.com/aws/karpenter-core/pkg/utils/nodeclaim"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/aws/karpenter-core/pkg/test/expectations"
)

var _ = Describe("Machine/GarbageCollection", func() {
	var provisioner *v1alpha5.Provisioner

	BeforeEach(func() {
		provisioner = test.Provisioner()
	})
	It("should delete the Machine when the Node never appears and the instance is gone", func() {
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

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		// Delete the machine from the cloudprovider
		Expect(cloudProvider.Delete(ctx, nodeclaimutil.New(machine))).To(Succeed())

		// Expect the Machine to be removed now that the Instance is gone
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})
		ExpectFinalizersRemoved(ctx, env.Client, machine)
		ExpectNotFound(ctx, env.Client, machine)
	})
	It("should delete many Machines when the Node never appears and the instance is gone", func() {
		var machines []*v1alpha5.Machine
		for i := 0; i < 100; i++ {
			machines = append(machines, test.Machine(v1alpha5.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha5.ProvisionerNameLabelKey: provisioner.Name,
					},
				},
			}))
		}
		ExpectApplied(ctx, env.Client, provisioner)
		workqueue.ParallelizeUntil(ctx, len(machines), len(machines), func(i int) {
			defer GinkgoRecover()
			ExpectApplied(ctx, env.Client, machines[i])
			ExpectReconcileSucceeded(ctx, machineController, client.ObjectKeyFromObject(machines[i]))
			machines[i] = ExpectExists(ctx, env.Client, machines[i])
		})

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		workqueue.ParallelizeUntil(ctx, len(machines), len(machines), func(i int) {
			defer GinkgoRecover()
			// Delete the machine from the cloudprovider
			Expect(cloudProvider.Delete(ctx, nodeclaimutil.New(machines[i]))).To(Succeed())
		})

		// Expect the Machines to be removed now that the Instance is gone
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})

		workqueue.ParallelizeUntil(ctx, len(machines), len(machines), func(i int) {
			defer GinkgoRecover()
			ExpectFinalizersRemoved(ctx, env.Client, machines[i])
		})
		ExpectNotFound(ctx, env.Client, lo.Map(machines, func(m *v1alpha5.Machine, _ int) client.Object { return m })...)
	})
	It("shouldn't delete the Machine when the Node isn't there but the instance is there", func() {
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

		// Step forward to move past the cache eventual consistency timeout
		fakeClock.SetTime(time.Now().Add(time.Second * 20))

		// Reconcile the Machine. It should not be deleted by this flow since it has never been registered
		ExpectReconcileSucceeded(ctx, garbageCollectionController, client.ObjectKey{})
		ExpectFinalizersRemoved(ctx, env.Client, machine)
		ExpectExists(ctx, env.Client, machine)
	})
})
