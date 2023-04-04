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

package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

type Launch struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	cache         *cache.Cache // exists due to eventual consistency on the cache
}

func (l *Launch) Reconcile(ctx context.Context, machine *v1alpha5.Machine) (reconcile.Result, error) {
	if machine.StatusConditions().GetCondition(v1alpha5.MachineLaunched).IsTrue() {
		return reconcile.Result{}, nil
	}

	var err error
	var created *v1alpha5.Machine

	// One of the following scenarios can happen with a Machine that isn't marked as launched:
	//  1. It was already launched by the CloudProvider but the client-go cache wasn't updated quickly enough or
	//     patching failed on the status. In this case, we use the in-memory cached value for the created machine.
	//  2. It is a "linked" machine, which implies that the CloudProvider Machine already exists for the Machine CR, but we
	//     need to grab info from the CloudProvider to get details on the machine.
	//  3. It is a standard machine launch where we should call CloudProvider Create() and fill in details of the launched
	//     machine into the Machine CR.
	if ret, ok := l.cache.Get(client.ObjectKeyFromObject(machine).String()); ok {
		created = ret.(*v1alpha5.Machine)
	} else if _, ok := machine.Annotations[v1alpha5.MachineLinkedAnnotationKey]; ok {
		created, err = l.linkMachine(ctx, machine)
	} else {
		created, err = l.launchMachine(ctx, machine)
	}
	// Either the machine launch/linking failed or the machine was deleted due to InsufficientCapacity/NotFound
	if err != nil || created == nil {
		return reconcile.Result{}, err
	}
	l.cache.SetDefault(client.ObjectKeyFromObject(machine).String(), created)
	PopulateMachineDetails(machine, created)
	machine.StatusConditions().MarkTrue(v1alpha5.MachineLaunched)
	return reconcile.Result{}, nil
}

func (l *Launch) linkMachine(ctx context.Context, machine *v1alpha5.Machine) (*v1alpha5.Machine, error) {
	created, err := l.cloudProvider.Get(ctx, machine.Annotations[v1alpha5.MachineLinkedAnnotationKey])
	if err != nil {
		if !cloudprovider.IsMachineNotFoundError(err) {
			machine.StatusConditions().MarkFalse(v1alpha5.MachineLaunched, "LinkFailed", truncateMessage(err.Error()))
			return nil, fmt.Errorf("linking machine, %w", err)
		}
		if err = l.kubeClient.Delete(ctx, machine); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		logging.FromContext(ctx).Debugf("garbage collected machine with no cloudprovider representation")
		metrics.MachinesTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      "garbage_collected",
			metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
		}).Inc()
		return nil, nil
	}
	logging.FromContext(ctx).Debugf("linked machine")
	return created, nil
}

func (l *Launch) launchMachine(ctx context.Context, machine *v1alpha5.Machine) (*v1alpha5.Machine, error) {
	instanceTypeRequirement, _ := lo.Find(machine.Spec.Requirements, func(req v1.NodeSelectorRequirement) bool { return req.Key == v1.LabelInstanceTypeStable })
	logging.FromContext(ctx).With("pods", machine.Spec.Resources.Requests[v1.ResourcePods],
		"resources", machine.Spec.Resources.Requests, "instance-types", instanceTypeList(instanceTypeRequirement.Values)).Infof("launching machine")
	created, err := l.cloudProvider.Create(ctx, machine)
	if err != nil {
		if !cloudprovider.IsInsufficientCapacityError(err) {
			machine.StatusConditions().MarkFalse(v1alpha5.MachineLaunched, "LaunchFailed", truncateMessage(err.Error()))
			return nil, fmt.Errorf("creating machine, %w", err)
		}
		logging.FromContext(ctx).Error(err)
		if err = l.kubeClient.Delete(ctx, machine); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		metrics.MachinesTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      "insufficient_capacity",
			metrics.ProvisionerLabel: machine.Labels[v1alpha5.ProvisionerNameLabelKey],
		}).Inc()
		return nil, nil
	}
	logging.FromContext(ctx).Debugf("launched machine")
	return created, nil
}

func PopulateMachineDetails(machine, retrieved *v1alpha5.Machine) {
	// These are ordered in priority order so that user-defined machine labels and requirements trump retrieved labels
	// or the static machine labels
	machine.Labels = lo.Assign(
		retrieved.Labels, // CloudProvider-resolved labels
		scheduling.NewNodeSelectorRequirements(machine.Spec.Requirements...).Labels(), // Single-value requirement resolved labels
		machine.Labels, // User-defined labels
	)
	machine.Annotations = lo.Assign(machine.Annotations, retrieved.Annotations)
	machine.Status.ProviderID = retrieved.Status.ProviderID
	machine.Status.Allocatable = retrieved.Status.Allocatable
	machine.Status.Capacity = retrieved.Status.Capacity
}

func instanceTypeList(names []string) string {
	var itSb strings.Builder
	for i, name := range names {
		// print the first 5 instance types only (indices 0-4)
		if i > 4 {
			lo.Must(fmt.Fprintf(&itSb, " and %d other(s)", len(names)-i))
			break
		} else if i > 0 {
			lo.Must(fmt.Fprint(&itSb, ", "))
		}
		lo.Must(fmt.Fprint(&itSb, name))
	}
	return itSb.String()
}

func truncateMessage(msg string) string {
	if len(msg) < 200 {
		return msg
	}
	return msg[:200] + "..."
}
