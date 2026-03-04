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

package deletioncost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// ChangeDetector tracks cluster state changes for optimization
// It uses hash-based comparison to determine if ranking computation is needed
type ChangeDetector struct {
	lastNodeHash string
	lastPodHash  string
}

// NewChangeDetector creates a new ChangeDetector instance
func NewChangeDetector() *ChangeDetector {
	return &ChangeDetector{
		lastNodeHash: "",
		lastPodHash:  "",
	}
}

// HasChanged determines if the cluster state has changed since the last check
// Returns true if nodes or pods have changed, false otherwise
func (c *ChangeDetector) HasChanged(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (bool, error) {
	// Compute current hashes
	currentNodeHash, err := computeNodeHash(ctx, kubeClient, nodes)
	if err != nil {
		return false, err
	}

	currentPodHash, err := computePodHash(ctx, kubeClient, nodes)
	if err != nil {
		return false, err
	}

	// Compare with previous hashes
	nodeChanged := currentNodeHash != c.lastNodeHash
	podChanged := currentPodHash != c.lastPodHash

	// Update stored hashes if changed
	if nodeChanged || podChanged {
		c.lastNodeHash = currentNodeHash
		c.lastPodHash = currentPodHash
		return true, nil
	}

	return false, nil
}

// computeNodeHash computes a stable hash of the node list
// Includes node names, creation timestamps, and pod counts
func computeNodeHash(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (string, error) {
	if len(nodes) == 0 {
		return "", nil
	}

	// Sort nodes by name for deterministic ordering
	sortedNodes := make([]*state.StateNode, len(nodes))
	copy(sortedNodes, nodes)
	sort.Slice(sortedNodes, func(i, j int) bool {
		return sortedNodes[i].Name() < sortedNodes[j].Name()
	})

	// Build a string representation of node state
	h := sha256.New()
	for _, node := range sortedNodes {
		// Include node name
		h.Write([]byte(node.Name()))

		// Include creation timestamp if available
		if node.Node != nil {
			h.Write([]byte(node.Node.CreationTimestamp.String()))
		} else if node.NodeClaim != nil {
			h.Write([]byte(node.NodeClaim.CreationTimestamp.String()))
		}

		// Include pod count
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%d", len(pods))
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// computePodHash computes a stable hash of pod assignments
// Includes pod names and their assigned node names
func computePodHash(ctx context.Context, kubeClient client.Client, nodes []*state.StateNode) (string, error) {
	if len(nodes) == 0 {
		return "", nil
	}

	// Collect all pod assignments
	type podAssignment struct {
		podName  string
		nodeName string
	}
	var assignments []podAssignment

	for _, node := range nodes {
		pods, err := node.Pods(ctx, kubeClient)
		if err != nil {
			return "", err
		}

		for _, pod := range pods {
			assignments = append(assignments, podAssignment{
				podName:  fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
				nodeName: node.Name(),
			})
		}
	}

	// Sort assignments for deterministic ordering
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].nodeName != assignments[j].nodeName {
			return assignments[i].nodeName < assignments[j].nodeName
		}
		return assignments[i].podName < assignments[j].podName
	})

	// Compute hash
	h := sha256.New()
	for _, assignment := range assignments {
		h.Write([]byte(assignment.nodeName))
		h.Write([]byte(assignment.podName))
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
