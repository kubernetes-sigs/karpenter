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

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
)

const registrationTTL = 20 * time.Minute

type Liveness struct {
	kubeClient client.Client
}

func (l *Liveness) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	cond := machine.StatusConditions().GetCondition(v1alpha5.MachineRegistered)

	// Delete the machine if we believe the machine won't register since we haven't seen the node
	if (cond == nil || cond.Status == v1.ConditionFalse) &&
		(!machine.CreationTimestamp.IsZero() && machine.CreationTimestamp.Add(registrationTTL).Before(time.Now())) {
		if err := l.kubeClient.Delete(ctx, machine); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}
