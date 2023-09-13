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

package nodeclaim

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/apis/v1beta1"
	"github.com/aws/karpenter-core/pkg/metrics"
	"github.com/aws/karpenter-core/pkg/scheduling"
	machineutil "github.com/aws/karpenter-core/pkg/utils/machine"
	nodepoolutil "github.com/aws/karpenter-core/pkg/utils/nodepool"
)

// EnableNodeClaims is an internal feature flag to allow functions that could List NodeClaims to work
// This flag is currently here to enable testing
// TODO @joinnis: Remove this internal flag when the v1beta1 APIs are released
var EnableNodeClaims = false

type Key struct {
	Name      string
	IsMachine bool
}

// PodEventHandler is a watcher on v1.Pods that maps Pods to NodeClaim based on the node names
// and enqueues reconcile.Requests for the NodeClaims
func PodEventHandler(ctx context.Context, c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
		if name := o.(*v1.Pod).Spec.NodeName; name != "" {
			node := &v1.Node{}
			if err := c.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
				return []reconcile.Request{}
			}
			nodeClaimList := &v1beta1.NodeClaimList{}
			if err := c.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
				return []reconcile.Request{}
			}
			return lo.Map(nodeClaimList.Items, func(n v1beta1.NodeClaim, _ int) reconcile.Request {
				return reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&n),
				}
			})
		}
		return requests
	})
}

// NodeEventHandler is a watcher on v1.Node that maps Nodes to NodeClaims based on provider ids
// and enqueues reconcile.Requests for the NodeClaims
func NodeEventHandler(ctx context.Context, c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		node := o.(*v1.Node)
		nodeClaimList := &v1beta1.NodeClaimList{}
		if err := c.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
			return []reconcile.Request{}
		}
		return lo.Map(nodeClaimList.Items, func(n v1beta1.NodeClaim, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}

// NodePoolEventHandler is a watcher on v1beta1.NodeClaim that maps Provisioner to NodeClaims based
// on the v1beta1.NodePoolLabelKey and enqueues reconcile.Requests for the NodeClaim
func NodePoolEventHandler(ctx context.Context, c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) (requests []reconcile.Request) {
		nodeClaimList := &v1beta1.NodeClaimList{}
		if err := c.List(ctx, nodeClaimList, client.MatchingLabels(map[string]string{v1beta1.NodePoolLabelKey: o.GetName()})); err != nil {
			return requests
		}
		return lo.Map(nodeClaimList.Items, func(n v1beta1.NodeClaim, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}

// NodeNotFoundError is an error returned when no v1.Nodes are found matching the passed providerID
type NodeNotFoundError struct {
	ProviderID string
}

func (e *NodeNotFoundError) Error() string {
	return fmt.Sprintf("no nodes found for provider id '%s'", e.ProviderID)
}

func IsNodeNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	nnfErr := &NodeNotFoundError{}
	return errors.As(err, &nnfErr)
}

func IgnoreNodeNotFoundError(err error) error {
	if !IsNodeNotFoundError(err) {
		return err
	}
	return nil
}

// DuplicateNodeError is an error returned when multiple v1.Nodes are found matching the passed providerID
type DuplicateNodeError struct {
	ProviderID string
}

func (e *DuplicateNodeError) Error() string {
	return fmt.Sprintf("multiple found for provider id '%s'", e.ProviderID)
}

func IsDuplicateNodeError(err error) bool {
	if err == nil {
		return false
	}
	dnErr := &DuplicateNodeError{}
	return errors.As(err, &dnErr)
}

func IgnoreDuplicateNodeError(err error) error {
	if !IsDuplicateNodeError(err) {
		return err
	}
	return nil
}

// NodeForNodeClaim is a helper function that takes a v1beta1.NodeClaim and attempts to find the matching v1.Node by its providerID
// This function will return errors if:
//  1. No v1.Nodes match the v1beta1.NodeClaim providerID
//  2. Multiple v1.Nodes match the v1beta1.NodeClaim providerID
func NodeForNodeClaim(ctx context.Context, c client.Client, nodeClaim *v1beta1.NodeClaim) (*v1.Node, error) {
	nodes, err := AllNodesForNodeClaim(ctx, c, nodeClaim)
	if err != nil {
		return nil, err
	}
	if len(nodes) > 1 {
		return nil, &DuplicateNodeError{ProviderID: nodeClaim.Status.ProviderID}
	}
	if len(nodes) == 0 {
		return nil, &NodeNotFoundError{ProviderID: nodeClaim.Status.ProviderID}
	}
	return nodes[0], nil
}

// AllNodesForNodeClaim is a helper function that takes a v1beta1.NodeClaim and finds ALL matching v1.Nodes by their providerID
// If the providerID is not resolved for a NodeClaim, then no Nodes will map to it
func AllNodesForNodeClaim(ctx context.Context, c client.Client, nodeClaim *v1beta1.NodeClaim) ([]*v1.Node, error) {
	// NodeClaims that have no resolved providerID have no nodes mapped to them
	if nodeClaim.Status.ProviderID == "" {
		return nil, nil
	}
	nodeList := v1.NodeList{}
	if err := c.List(ctx, &nodeList, client.MatchingFields{"spec.providerID": nodeClaim.Status.ProviderID}); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	return lo.ToSlicePtr(nodeList.Items), nil
}

func New(machine *v1alpha5.Machine) *v1beta1.NodeClaim {
	return &v1beta1.NodeClaim{
		TypeMeta:   machine.TypeMeta,
		ObjectMeta: machine.ObjectMeta,
		Spec: v1beta1.NodeClaimSpec{
			Taints:        machine.Spec.Taints,
			StartupTaints: machine.Spec.StartupTaints,
			Requirements:  machine.Spec.Requirements,
			Resources: v1beta1.ResourceRequirements{
				Requests: machine.Spec.Resources.Requests,
			},
			KubeletConfiguration: NewKubeletConfiguration(machine.Spec.Kubelet),
			NodeClass:            NewNodeClassReference(machine.Spec.MachineTemplateRef),
		},
		Status: v1beta1.NodeClaimStatus{
			NodeName:    machine.Status.NodeName,
			ProviderID:  machine.Status.ProviderID,
			Capacity:    machine.Status.Capacity,
			Allocatable: machine.Status.Allocatable,
			Conditions:  NewConditions(machine.Status.Conditions),
		},
		IsMachine: true,
	}
}

func NewConditions(conds apis.Conditions) apis.Conditions {
	out := conds.DeepCopy()
	for i := range out {
		switch out[i].Type {
		case v1alpha5.MachineLaunched:
			out[i].Type = v1beta1.NodeLaunched
		case v1alpha5.MachineRegistered:
			out[i].Type = v1beta1.NodeRegistered
		case v1alpha5.MachineInitialized:
			out[i].Type = v1beta1.NodeInitialized
		case v1alpha5.MachineEmpty:
			out[i].Type = v1beta1.NodeEmpty
		case v1alpha5.MachineExpired:
			out[i].Type = v1beta1.NodeExpired
		case v1alpha5.MachineDrifted:
			out[i].Type = v1beta1.NodeDrifted
		}
	}
	return out
}

func NewKubeletConfiguration(kc *v1alpha5.KubeletConfiguration) *v1beta1.KubeletConfiguration {
	if kc == nil {
		return nil
	}
	return &v1beta1.KubeletConfiguration{
		ClusterDNS:                  kc.ClusterDNS,
		ContainerRuntime:            kc.ContainerRuntime,
		MaxPods:                     kc.MaxPods,
		PodsPerCore:                 kc.PodsPerCore,
		SystemReserved:              kc.SystemReserved,
		KubeReserved:                kc.KubeReserved,
		EvictionHard:                kc.EvictionHard,
		EvictionSoft:                kc.EvictionSoft,
		EvictionSoftGracePeriod:     kc.EvictionSoftGracePeriod,
		EvictionMaxPodGracePeriod:   kc.EvictionMaxPodGracePeriod,
		ImageGCHighThresholdPercent: kc.ImageGCHighThresholdPercent,
		ImageGCLowThresholdPercent:  kc.ImageGCLowThresholdPercent,
		CPUCFSQuota:                 kc.CPUCFSQuota,
	}
}

func NewNodeClassReference(mtr *v1alpha5.MachineTemplateRef) *v1beta1.NodeClassReference {
	if mtr == nil {
		return nil
	}
	return &v1beta1.NodeClassReference{
		Kind:       mtr.Kind,
		Name:       mtr.Name,
		APIVersion: mtr.APIVersion,
	}
}

// NewFromNode converts a node into a pseudo-NodeClaim using known values from the node
// Deprecated: This NodeClaim generator function can be removed when v1beta1 migration has completed.
func NewFromNode(node *v1.Node) *v1beta1.NodeClaim {
	nc := &v1beta1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Annotations: node.Annotations,
			Labels:      node.Labels,
			Finalizers:  []string{v1alpha5.TerminationFinalizer},
		},
		Spec: v1beta1.NodeClaimSpec{
			Taints:       node.Spec.Taints,
			Requirements: scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements(),
			Resources: v1beta1.ResourceRequirements{
				Requests: node.Status.Allocatable,
			},
		},
		Status: v1beta1.NodeClaimStatus{
			NodeName:    node.Name,
			ProviderID:  node.Spec.ProviderID,
			Capacity:    node.Status.Capacity,
			Allocatable: node.Status.Allocatable,
		},
	}
	if _, ok := node.Labels[v1beta1.NodeInitializedLabelKey]; ok {
		nc.StatusConditions().MarkTrue(v1beta1.NodeInitialized)
	}
	nc.StatusConditions().MarkTrue(v1beta1.NodeLaunched)
	nc.StatusConditions().MarkTrue(v1beta1.NodeRegistered)
	return nc
}

func Get(ctx context.Context, c client.Client, key Key) (*v1beta1.NodeClaim, error) {
	if key.IsMachine {
		machine := &v1alpha5.Machine{}
		if err := c.Get(ctx, types.NamespacedName{Name: key.Name}, machine); err != nil {
			return nil, err
		}
		return New(machine), nil
	}
	nodeClaim := &v1beta1.NodeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Name: key.Name}, nodeClaim); err != nil {
		return nil, err
	}
	return nodeClaim, nil
}

func List(ctx context.Context, c client.Client, opts ...client.ListOption) (*v1beta1.NodeClaimList, error) {
	machineList := &v1alpha5.MachineList{}
	if err := c.List(ctx, machineList, opts...); err != nil {
		return nil, err
	}
	convertedNodeClaims := lo.Map(machineList.Items, func(m v1alpha5.Machine, _ int) v1beta1.NodeClaim {
		return *New(&m)
	})
	nodeClaimList := &v1beta1.NodeClaimList{}
	if EnableNodeClaims {
		if err := c.List(ctx, nodeClaimList, opts...); err != nil {
			return nil, err
		}
	}
	nodeClaimList.Items = append(nodeClaimList.Items, convertedNodeClaims...)
	return nodeClaimList, nil
}

func UpdateStatus(ctx context.Context, c client.Client, nodeClaim *v1beta1.NodeClaim) error {
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return c.Status().Update(ctx, machine)
	}
	return c.Status().Update(ctx, nodeClaim)
}

func Patch(ctx context.Context, c client.Client, stored, nodeClaim *v1beta1.NodeClaim) error {
	if nodeClaim.IsMachine {
		storedMachine := machineutil.NewFromNodeClaim(stored)
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return c.Patch(ctx, machine, client.MergeFrom(storedMachine))
	}
	return c.Patch(ctx, nodeClaim, client.MergeFrom(stored))
}

func PatchStatus(ctx context.Context, c client.Client, stored, nodeClaim *v1beta1.NodeClaim) error {
	if nodeClaim.IsMachine {
		storedMachine := machineutil.NewFromNodeClaim(stored)
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return c.Status().Patch(ctx, machine, client.MergeFrom(storedMachine))
	}
	return c.Status().Patch(ctx, nodeClaim, client.MergeFrom(stored))
}

func Delete(ctx context.Context, c client.Client, nodeClaim *v1beta1.NodeClaim) error {
	if nodeClaim.IsMachine {
		machine := machineutil.NewFromNodeClaim(nodeClaim)
		return c.Delete(ctx, machine)
	}
	return c.Delete(ctx, nodeClaim)
}

func CreatedCounter(nodeClaim *v1beta1.NodeClaim, reason string) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesCreatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      reason,
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsCreatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:   reason,
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func LaunchedCounter(nodeClaim *v1beta1.NodeClaim) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesLaunchedCounter.With(prometheus.Labels{
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsLaunchedCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func RegisteredCounter(nodeClaim *v1beta1.NodeClaim) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesRegisteredCounter.With(prometheus.Labels{
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsRegisteredCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func InitializedCounter(nodeClaim *v1beta1.NodeClaim) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesInitializedCounter.With(prometheus.Labels{
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsInitializedCounter.With(prometheus.Labels{
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func TerminatedCounter(nodeClaim *v1beta1.NodeClaim, reason string) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesTerminatedCounter.With(prometheus.Labels{
			metrics.ReasonLabel:      reason,
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsTerminatedCounter.With(prometheus.Labels{
		metrics.ReasonLabel:   reason,
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func DisruptedCounter(nodeClaim *v1beta1.NodeClaim, disruptionType string) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesDisruptedCounter.With(prometheus.Labels{
			metrics.TypeLabel:        disruptionType,
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
		metrics.TypeLabel:     disruptionType,
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func DriftedCounter(nodeClaim *v1beta1.NodeClaim, driftType string) prometheus.Counter {
	if nodeClaim.IsMachine {
		return metrics.MachinesDriftedCounter.With(prometheus.Labels{
			metrics.TypeLabel:        driftType,
			metrics.ProvisionerLabel: nodeClaim.Labels[v1alpha5.ProvisionerNameLabelKey],
		})
	}
	return metrics.NodeClaimsDisruptedCounter.With(prometheus.Labels{
		metrics.TypeLabel:     driftType,
		metrics.NodePoolLabel: nodeClaim.Labels[v1beta1.NodePoolLabelKey],
	})
}

func UpdateNodeOwnerReferences(nodeClaim *v1beta1.NodeClaim, node *v1.Node) *v1.Node {
	// Remove any provisioner owner references since we own them
	node.OwnerReferences = lo.Reject(node.OwnerReferences, func(o metav1.OwnerReference, _ int) bool {
		return o.Kind == "Provisioner"
	})
	if nodeClaim.IsMachine {
		node.OwnerReferences = append(node.OwnerReferences, metav1.OwnerReference{
			APIVersion:         v1alpha5.SchemeGroupVersion.String(),
			Kind:               "Machine",
			Name:               nodeClaim.Name,
			UID:                nodeClaim.UID,
			BlockOwnerDeletion: ptr.Bool(true),
		})
	} else {
		node.OwnerReferences = append(node.OwnerReferences, metav1.OwnerReference{
			APIVersion:         v1beta1.SchemeGroupVersion.String(),
			Kind:               "NodeClaim",
			Name:               nodeClaim.Name,
			UID:                nodeClaim.UID,
			BlockOwnerDeletion: ptr.Bool(true),
		})
	}
	return node
}

func Owner(ctx context.Context, c client.Client, obj interface{ GetLabels() map[string]string }) (*v1beta1.NodePool, error) {
	if v, ok := obj.GetLabels()[v1beta1.NodePoolLabelKey]; ok {
		nodePool := &v1beta1.NodePool{}
		if err := c.Get(ctx, types.NamespacedName{Name: v}, nodePool); err != nil {
			return nil, err
		}
		return nodePool, nil
	}
	if v, ok := obj.GetLabels()[v1alpha5.ProvisionerNameLabelKey]; ok {
		provisioner := &v1alpha5.Provisioner{}
		if err := c.Get(ctx, types.NamespacedName{Name: v}, provisioner); err != nil {
			return nil, err
		}
		return nodepoolutil.New(provisioner), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "NodePool"}, "")
}

func OwnerKey(obj interface{ GetLabels() map[string]string }) nodepoolutil.Key {
	if v, ok := obj.GetLabels()[v1beta1.NodePoolLabelKey]; ok {
		return nodepoolutil.Key{Name: v, IsProvisioner: false}
	}
	if v, ok := obj.GetLabels()[v1alpha5.ProvisionerNameLabelKey]; ok {
		return nodepoolutil.Key{Name: v, IsProvisioner: true}
	}
	return nodepoolutil.Key{}
}
