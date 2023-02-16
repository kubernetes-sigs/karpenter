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
	"errors"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	"github.com/aws/karpenter-core/pkg/scheduling"
)

func EventHandler(ctx context.Context, c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		machine := o.(*v1alpha5.Machine)
		nodeList := &v1.NodeList{}
		if machine.Status.ProviderID == "" {
			return nil
		}
		if err := c.List(ctx, nodeList, client.MatchingFields{"spec.providerID": machine.Status.ProviderID}); err != nil {
			return nil
		}
		return lo.Map(nodeList.Items, func(n v1.Node, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&n),
			}
		})
	})
}

func NodeEventHandler(ctx context.Context, c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		node := o.(*v1.Node)
		machineList := &v1alpha5.MachineList{}
		if err := c.List(ctx, machineList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
			return []reconcile.Request{}
		}
		return lo.Map(machineList.Items, func(m v1alpha5.Machine, _ int) reconcile.Request {
			return reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&m),
			}
		})
	})
}

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

func NodeForMachine(ctx context.Context, c client.Client, machine *v1alpha5.Machine) (*v1.Node, error) {
	nodeList := v1.NodeList{}
	if err := c.List(ctx, &nodeList, client.MatchingFields{"spec.providerID": machine.Status.ProviderID}, client.Limit(2)); err != nil {
		return nil, fmt.Errorf("listing nodes, %w", err)
	}
	if len(nodeList.Items) > 1 {
		return nil, &DuplicateNodeError{ProviderID: machine.Status.ProviderID}
	}
	if len(nodeList.Items) == 0 {
		return nil, &NodeNotFoundError{ProviderID: machine.Status.ProviderID}
	}
	return &nodeList.Items[0], nil
}

// New converts a node into a Machine using known values from the node and provisioner spec values
// Deprecated: This Machine generator function can be removed when v1beta1 migration has completed.
func New(node *v1.Node, provisioner *v1alpha5.Provisioner) *v1alpha5.Machine {
	machine := NewFromNode(node)
	machine.Annotations = lo.Assign(provisioner.Annotations, v1alpha5.ProviderAnnotation(provisioner.Spec.Provider))
	machine.Labels = lo.Assign(provisioner.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})
	machine.Spec.Kubelet = provisioner.Spec.KubeletConfiguration
	machine.Spec.Taints = provisioner.Spec.Taints
	machine.Spec.StartupTaints = provisioner.Spec.StartupTaints
	machine.Spec.Requirements = provisioner.Spec.Requirements
	machine.Spec.ProviderRef = provisioner.Spec.ProviderRef
	lo.Must0(controllerutil.SetOwnerReference(provisioner, machine, scheme.Scheme))
	return machine
}

// NewFromNode converts a node into a pseudo-Machine using known values from the node
// Deprecated: This Machine generator function can be removed when v1beta1 migration has completed.
func NewFromNode(node *v1.Node) *v1alpha5.Machine {
	m := &v1alpha5.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        node.Name,
			Annotations: node.Annotations,
			Labels:      node.Labels,
			Finalizers:  []string{v1alpha5.TerminationFinalizer},
		},
		Spec: v1alpha5.MachineSpec{
			Taints:       node.Spec.Taints,
			Requirements: scheduling.NewLabelRequirements(node.Labels).NodeSelectorRequirements(),
			Resources: v1alpha5.ResourceRequirements{
				Requests: node.Status.Allocatable,
			},
		},
		Status: v1alpha5.MachineStatus{
			ProviderID:  node.Spec.ProviderID,
			Capacity:    node.Status.Capacity,
			Allocatable: node.Status.Allocatable,
		},
	}
	if _, ok := node.Labels[v1alpha5.LabelNodeInitialized]; ok {
		m.StatusConditions().MarkTrue(v1alpha5.MachineInitialized)
	}
	m.StatusConditions().MarkTrue(v1alpha5.MachineCreated)
	m.StatusConditions().MarkTrue(v1alpha5.MachineRegistered)
	return m
}
