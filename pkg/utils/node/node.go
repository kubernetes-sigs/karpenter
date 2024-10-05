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

package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/pod"
)

// NodeClaimNotFoundError is an error returned when no v1.NodeClaims are found matching the passed providerID
type NodeClaimNotFoundError struct {
	ProviderID string
}

func (e *NodeClaimNotFoundError) Error() string {
	return fmt.Sprintf("no nodeclaims found for provider id '%s'", e.ProviderID)
}

func IsNodeClaimNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	nnfErr := &NodeClaimNotFoundError{}
	return errors.As(err, &nnfErr)
}

func IgnoreNodeClaimNotFoundError(err error) error {
	if !IsNodeClaimNotFoundError(err) {
		return err
	}
	return nil
}

// DuplicateNodeClaimError is an error returned when multiple v1.NodeClaims are found matching the passed providerID
type DuplicateNodeClaimError struct {
	ProviderID string
}

func (e *DuplicateNodeClaimError) Error() string {
	return fmt.Sprintf("multiple found for provider id '%s'", e.ProviderID)
}

func IsDuplicateNodeClaimError(err error) bool {
	if err == nil {
		return false
	}
	dnErr := &DuplicateNodeClaimError{}
	return errors.As(err, &dnErr)
}

func IgnoreDuplicateNodeClaimError(err error) error {
	if !IsDuplicateNodeClaimError(err) {
		return err
	}
	return nil
}

// GetPods grabs all pods that are currently bound to the passed nodes
func GetPods(ctx context.Context, kubeClient client.Client, nodes ...*corev1.Node) ([]*corev1.Pod, error) {
	var pods []*corev1.Pod
	for _, node := range nodes {
		var podList corev1.PodList
		if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
			return nil, fmt.Errorf("listing pods, %w", err)
		}
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
	}
	return pods, nil
}

// GetNodeClaims grabs all NodeClaims with a providerID that matches the provided Node
func GetNodeClaims(ctx context.Context, node *corev1.Node, kubeClient client.Client) ([]*v1.NodeClaim, error) {
	nodeClaimList := &v1.NodeClaimList{}
	if err := kubeClient.List(ctx, nodeClaimList, client.MatchingFields{"status.providerID": node.Spec.ProviderID}); err != nil {
		return nil, fmt.Errorf("listing nodeClaims, %w", err)
	}
	return lo.ToSlicePtr(nodeClaimList.Items), nil
}

// NodeClaimForNode is a helper function that takes a corev1.Node and attempts to find the matching v1.NodeClaim by its providerID
// This function will return errors if:
//  1. No v1.NodeClaims match the corev1.Node's providerID
//  2. Multiple v1.NodeClaims match the corev1.Node's providerID
func NodeClaimForNode(ctx context.Context, c client.Client, node *corev1.Node) (*v1.NodeClaim, error) {
	nodeClaims, err := GetNodeClaims(ctx, node, c)
	if err != nil {
		return nil, err
	}
	if len(nodeClaims) > 1 {
		return nil, &DuplicateNodeClaimError{ProviderID: node.Spec.ProviderID}
	}
	if len(nodeClaims) == 0 {
		return nil, &NodeClaimNotFoundError{ProviderID: node.Spec.ProviderID}
	}
	return nodeClaims[0], nil
}

// GetReschedulablePods grabs all pods from the passed nodes that satisfy the IsReschedulable criteria
func GetReschedulablePods(ctx context.Context, kubeClient client.Client, nodes ...*corev1.Node) ([]*corev1.Pod, error) {
	pods, err := GetPods(ctx, kubeClient, nodes...)
	if err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.Filter(pods, func(p *corev1.Pod, _ int) bool {
		return pod.IsReschedulable(p)
	}), nil
}

// GetProvisionablePods grabs all the pods from the passed nodes that satisfy the IsProvisionable criteria
func GetProvisionablePods(ctx context.Context, kubeClient client.Client) ([]*corev1.Pod, error) {
	var podList corev1.PodList
	if err := kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	return lo.FilterMap(podList.Items, func(p corev1.Pod, _ int) (*corev1.Pod, bool) {
		return &p, pod.IsProvisionable(&p)
	}), nil
}

// GetVolumeAttachments grabs all volumeAttachments associated with the passed node
func GetVolumeAttachments(ctx context.Context, kubeClient client.Client, node *corev1.Node) ([]*storagev1.VolumeAttachment, error) {
	var volumeAttachmentList storagev1.VolumeAttachmentList
	if err := kubeClient.List(ctx, &volumeAttachmentList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return nil, fmt.Errorf("listing volumeAttachments, %w", err)
	}
	return lo.ToSlicePtr(volumeAttachmentList.Items), nil
}

func GetCondition(n *corev1.Node, match corev1.NodeConditionType) corev1.NodeCondition {
	for _, condition := range n.Status.Conditions {
		if condition.Type == match {
			return condition
		}
	}
	return corev1.NodeCondition{}
}
