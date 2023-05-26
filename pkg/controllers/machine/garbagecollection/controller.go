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

package garbagecollection

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	corecontroller "github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/utils/sets"
)

type Controller struct {
	clock         clock.Clock
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
}

func NewController(c clock.Clock, kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) corecontroller.Controller {
	return &Controller{
		clock:         c,
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Name() string {
	return "machine.garbagecollection"
}

func (c *Controller) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	machineList := &v1alpha5.MachineList{}
	if err := c.kubeClient.List(ctx, machineList); err != nil {
		return reconcile.Result{}, err
	}
	cloudProviderMachines, err := c.cloudProvider.List(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	cloudProviderMachines = lo.Filter(cloudProviderMachines, func(m *v1alpha5.Machine, _ int) bool {
		return m.DeletionTimestamp.IsZero()
	})
	cloudProviderProviderIDs := sets.New[string](lo.Map(cloudProviderMachines, func(m *v1alpha5.Machine, _ int) string {
		return m.Status.ProviderID
	})...)
	machines := lo.Filter(lo.ToSlicePtr(machineList.Items), func(m *v1alpha5.Machine, _ int) bool {
		return m.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue() &&
			c.clock.Since(m.StatusConditions().GetCondition(v1alpha5.MachineLaunched).LastTransitionTime.Inner.Time) > time.Second*10 &&
			!cloudProviderProviderIDs.Has(m.Status.ProviderID)
	})

	errs := make([]error, len(machines))
	workqueue.ParallelizeUntil(ctx, 20, len(machines), func(i int) {
		if err := c.kubeClient.Delete(ctx, machines[i]); err != nil {
			errs[i] = client.IgnoreNotFound(err)
			return
		}
		logging.FromContext(ctx).
			With("provisioner", machines[i].Labels[v1alpha5.ProvisionerNameLabelKey], "machine", machines[i].Name, "provider-id", machines[i].Status.ProviderID).
			Debugf("garbage collecting machine with no cloudprovider representation")
		metrics.MachinesTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      "garbage_collected",
			metrics.ProvisionerLabel: machines[i].Labels[v1alpha5.ProvisionerNameLabelKey],
		}).Inc()
	})
	return reconcile.Result{RequeueAfter: time.Minute * 2}, multierr.Combine(errs...)
}

func (c *Controller) Builder(_ context.Context, m manager.Manager) corecontroller.Builder {
	return corecontroller.NewSingletonManagedBy(m)
}
