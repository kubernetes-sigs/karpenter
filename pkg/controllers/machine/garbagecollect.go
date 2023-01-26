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

package machine

import (
	"context"
	"time"

	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
)

type GarbageCollect struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	lastChecked   *cache.Cache
}

func (e *GarbageCollect) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if !machine.StatusConditions().GetCondition(v1alpha5.MachineCreated).IsTrue() {
		return reconcile.Result{}, nil
	}
	// If there is no node representation for the machine, then check if there is a representation at the cloudprovider
	if _, err := machineutil.NodeForMachine(ctx, e.kubeClient, machine); err == nil || !machineutil.IsNodeNotFoundError(err) {
		return reconcile.Result{}, nil
	}
	if _, expireTime, ok := e.lastChecked.GetWithExpiration(client.ObjectKeyFromObject(machine).String()); ok {
		return reconcile.Result{RequeueAfter: time.Until(expireTime)}, nil
	}
	if _, err := e.cloudProvider.Get(ctx, machine.Status.ProviderID); cloudprovider.IsMachineNotFoundError(err) {
		return reconcile.Result{}, client.IgnoreNotFound(e.kubeClient.Delete(ctx, machine))
	}
	e.lastChecked.SetDefault(client.ObjectKeyFromObject(machine).String(), nil)
	return reconcile.Result{}, nil
}
