/*
Copyright The Kubernetes Authors.

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

package provisioning

import (
	"context"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/option"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	scheduler "sigs.k8s.io/karpenter/pkg/controllers/provisioning/scheduling"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
)

// LaunchOptions are the set of options that can be used to trigger certain
// actions and configuration during scheduling
type LaunchOptions struct {
	IgnoreStaticNodeClaims bool
	RecordPodNomination    bool
	Reason                 string
}

// RecordPodNomination causes nominate pod events to be recorded against the node.
func RecordPodNomination(o *LaunchOptions) {
	o.RecordPodNomination = true
}

func WithReason(reason string) func(*LaunchOptions) {
	return func(o *LaunchOptions) { o.Reason = reason }
}

type Provisioner struct {
	kubeClient client.Client
	recorder   events.Recorder
	cluster    *state.Cluster
}

func NewProvisioner(kubeClient client.Client, recorder events.Recorder, cluster *state.Cluster) *Provisioner {
	return &Provisioner{
		kubeClient: kubeClient,
		recorder:   recorder,
		cluster:    cluster,
	}
}

// CreateNodeClaims launches nodes passed into the function in parallel. It returns a slice of the successfully created node
// names as well as a multierr of any errors that occurred while launching nodes
func (p *Provisioner) CreateNodeClaims(ctx context.Context, nodeClaims []*scheduler.NodeClaim, opts ...option.Function[LaunchOptions]) ([]string, error) {
	// Create capacity and bind pods
	errs := make([]error, len(nodeClaims))
	nodeClaimNames := make([]string, len(nodeClaims))
	workqueue.ParallelizeUntil(ctx, len(nodeClaims), len(nodeClaims), func(i int) {
		if option.Resolve(opts...).IgnoreStaticNodeClaims && nodeClaims[i].IsStaticNode {
			return
		}
		// create a new context to avoid a data race on the ctx variable
		if name, err := p.Create(ctx, nodeClaims[i], opts...); err != nil {
			errs[i] = fmt.Errorf("creating node claim, %w", err)
		} else {
			nodeClaimNames[i] = name
		}
	})
	return nodeClaimNames, multierr.Combine(errs...)
}

func (p *Provisioner) Create(ctx context.Context, n *scheduler.NodeClaim, opts ...option.Function[LaunchOptions]) (string, error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("NodePool", klog.KRef("", n.NodePoolName)))
	options := option.Resolve(opts...)
	latest := &v1.NodePool{}
	if err := p.kubeClient.Get(ctx, types.NamespacedName{Name: n.NodePoolName}, latest); err != nil {
		return "", fmt.Errorf("getting current resource usage, %w", err)
	}
	if err := latest.Spec.Limits.ExceededBy(p.cluster.NodePoolResourcesFor(n.NodePoolName)); err != nil {
		return "", err
	}
	nodeClaim := n.ToNodeClaim()

	if err := p.kubeClient.Create(ctx, nodeClaim); err != nil {
		return "", err
	}

	// Update pod to nodeClaim mapping for newly created nodeClaims. We do
	// this here because nodeClaim does not have a name until it is created.
	p.cluster.UpdatePodToNodeClaimMapping(map[string][]*corev1.Pod{nodeClaim.Name: n.Pods})

	instanceTypeRequirement, _ := lo.Find(nodeClaim.Spec.Requirements, func(req v1.NodeSelectorRequirementWithMinValues) bool {
		return req.Key == corev1.LabelInstanceTypeStable
	})

	log.FromContext(ctx).WithValues("NodeClaim", klog.KObj(nodeClaim), "requests", nodeClaim.Spec.Resources.Requests, "instance-types", instanceTypeList(instanceTypeRequirement.Values)).
		Info("created nodeclaim")
	metrics.NodeClaimsCreatedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       options.Reason,
		metrics.NodePoolLabel:     nodeClaim.Labels[v1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[v1.CapacityTypeLabelKey],
	})
	// Update the nodeclaim manually in state to avoid eventual consistency delay races with our watcher.
	// This is essential to avoiding races where disruption can create a replacement node, then immediately
	// requeue. This can race with controller-runtime's internal cache as it watches events on the cluster
	// to then trigger cluster state updates. Triggering it manually ensures that Karpenter waits for the
	// internal cache to sync before moving onto another disruption loop.
	p.cluster.UpdateNodeClaim(nodeClaim)
	if option.Resolve(opts...).RecordPodNomination {
		for _, pod := range n.Pods {
			p.recorder.Publish(scheduler.NominatePodEvent(pod, nil, nodeClaim))
		}
	}
	return nodeClaim.Name, nil
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
